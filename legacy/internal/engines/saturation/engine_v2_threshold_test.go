/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

var _ = Describe("applyUniversalThreshold", func() {
	const (
		scaleUp   = 0.85
		scaleDown = 0.70
	)

	It("scales up when demand exceeds the scale-up threshold against anticipated supply", func() {
		// RC = 10000/0.85 − 10000 = 1764.7...
		r := &domain.AnalyzerResult{
			TotalDemand:            10000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		Expect(r.RequiredCapacity).To(BeNumerically("~", 10000/scaleUp-10000, 1e-9))
		// SC clamps to 0: TotalDemand/scaleDown = 14285.7 > TotalSupply.
		Expect(r.SpareCapacity).To(BeZero())
	})

	It("scales down when demand is well below the scale-down boundary", func() {
		// SC = 10000 − 5000/0.70 = 2857.1
		r := &domain.AnalyzerResult{
			TotalDemand:            5000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		Expect(r.RequiredCapacity).To(BeZero()) // 5000/0.85 = 5882.3 < 10000
		Expect(r.SpareCapacity).To(BeNumerically("~", 10000-5000/scaleDown, 1e-9))
	})

	It("yields RC=SC=0 when demand sits in the hysteresis band", func() {
		// utilization 0.80: above scaleDown 0.70 (no SC) but below scaleUp 0.85 (no RC)
		r := &domain.AnalyzerResult{
			TotalDemand:            8000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		Expect(r.RequiredCapacity).To(BeZero())
		Expect(r.SpareCapacity).To(BeZero())
	})

	It("clamps RC and SC to zero exactly at the respective thresholds", func() {
		rUp := &domain.AnalyzerResult{
			TotalDemand:            10000 * scaleUp,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
		}
		applyUniversalThreshold(rUp, scaleUp, scaleDown)
		Expect(rUp.RequiredCapacity).To(BeNumerically("~", 0, 1e-9))

		rDown := &domain.AnalyzerResult{
			TotalDemand:            10000 * scaleDown,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
		}
		applyUniversalThreshold(rDown, scaleUp, scaleDown)
		Expect(rDown.SpareCapacity).To(BeNumerically("~", 0, 1e-9))
	})

	It("uses anticipated supply for RC but steady-state TotalSupply for SC", func() {
		// Pending replica: anticipated=10000 > steady-state=7000 → RC=0 despite demand>supply.
		// SC still uses TotalSupply (7000), not anticipated.
		r := &domain.AnalyzerResult{
			TotalDemand:            8000,
			TotalSupply:            7000,
			TotalAnticipatedSupply: 10000,
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		// 8000/0.85 = 9411.7 < 10000 anticipated → RC = 0
		Expect(r.RequiredCapacity).To(BeZero())
		// 7000 − 8000/0.70 = 7000 − 11428.57 → clamps to 0
		Expect(r.SpareCapacity).To(BeZero())
	})

	It("treats TotalAnticipatedSupply == 0 as a literal value — RC = TotalDemand/scaleUp", func() {
		// No fallback: zero means zero anticipated supply.
		// RC = 10000/0.85 − 0 = 11764.7...
		r := &domain.AnalyzerResult{
			TotalDemand:            10000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 0,
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		Expect(r.RequiredCapacity).To(BeNumerically("~", 10000/scaleUp, 1e-9))
	})

	It("does not divide by zero when scaleUp or scaleDown is non-positive", func() {
		r := &domain.AnalyzerResult{
			TotalDemand:            5000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
			RequiredCapacity:       42,
			SpareCapacity:          17,
		}
		applyUniversalThreshold(r, 0, 0)
		Expect(r.RequiredCapacity).To(Equal(42.0))
		Expect(r.SpareCapacity).To(Equal(17.0))

		applyUniversalThreshold(r, -1, -1)
		Expect(r.RequiredCapacity).To(Equal(42.0))
		Expect(r.SpareCapacity).To(Equal(17.0))
	})

	It("is idempotent on its own output under the same inputs", func() {
		r := &domain.AnalyzerResult{
			TotalDemand:            12000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 11000,
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		rc1, sc1 := r.RequiredCapacity, r.SpareCapacity
		applyUniversalThreshold(r, scaleUp, scaleDown)
		Expect(r.RequiredCapacity).To(Equal(rc1))
		Expect(r.SpareCapacity).To(Equal(sc1))
	})

	It("is a no-op on a nil result", func() {
		Expect(func() { applyUniversalThreshold(nil, scaleUp, scaleDown) }).NotTo(Panic())
	})

	It("recalibrates RoleCapacities with the same formula and threshold values", func() {
		// P/D disaggregated: two roles with distinct demand/anticipated/supply.
		// prefill: RC = 8000/0.85 − 11000 = 9411.7 − 11000 → 0; SC = 10000 − 8000/0.70 → 0
		// decode:  RC = 9500/0.85 − 10000 = 11176.4 − 10000 = 1176.4; SC = 10000 − 9500/0.70 → 0
		r := &domain.AnalyzerResult{
			TotalDemand:            17500,
			TotalSupply:            20000,
			TotalAnticipatedSupply: 21000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"prefill": {
					TotalDemand:            8000,
					TotalSupply:            10000,
					TotalAnticipatedSupply: 11000,
				},
				"decode": {
					TotalDemand:            9500,
					TotalSupply:            10000,
					TotalAnticipatedSupply: 10000,
				},
			},
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)

		prefill := r.RoleCapacities["prefill"]
		Expect(prefill.RequiredCapacity).To(BeZero())
		Expect(prefill.SpareCapacity).To(BeZero())

		decode := r.RoleCapacities["decode"]
		Expect(decode.RequiredCapacity).To(BeNumerically("~", 9500/scaleUp-10000, 1e-9))
		Expect(decode.SpareCapacity).To(BeZero())
	})

	It("treats per-role TotalAnticipatedSupply == 0 as a literal value — RC = TotalDemand/scaleUp", func() {
		// No fallback to TotalSupply; zero means zero anticipated supply for that role.
		// RC = 5000/0.85 − 0 = 5882.3
		r := &domain.AnalyzerResult{
			TotalDemand:            5000,
			TotalSupply:            10000,
			TotalAnticipatedSupply: 10000,
			RoleCapacities: map[string]domain.RoleCapacity{
				"both": {
					TotalDemand:            5000,
					TotalSupply:            10000,
					TotalAnticipatedSupply: 0,
				},
			},
		}
		applyUniversalThreshold(r, scaleUp, scaleDown)
		both := r.RoleCapacities["both"]
		Expect(both.RequiredCapacity).To(BeNumerically("~", 5000/scaleUp, 1e-9))
		Expect(both.SpareCapacity).To(BeNumerically("~", 10000-5000/scaleDown, 1e-9))
	})
})
