/*
Copyright 2025.

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
	"context"
	"errors"
	"sync"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

// spyAnalyzer is a minimal domain.Analyzer used in registration tests.
// Configurable to record calls, return an error, or panic.
type spyAnalyzer struct {
	name      string
	callCount int
	err       error
	panicMsg  string
}

func (s *spyAnalyzer) Name() string { return s.name }

func (s *spyAnalyzer) Analyze(_ context.Context, in domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	s.callCount++
	if s.panicMsg != "" {
		panic(s.panicMsg)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &domain.AnalyzerResult{AnalyzerName: s.name, ModelID: in.ModelID}, nil
}

var _ = Describe("Engine analyzer registry", func() {

	Describe("NewEngine", func() {
		It("pre-registers the V2 saturation analyzer at slot 0", func() {
			sourceRegistry := source.NewSourceRegistry()
			Expect(sourceRegistry.Register("prometheus", source.NewNoOpSource())).To(Succeed())
			testConfig := config.NewTestConfig()
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			Expect(engine.analyzers).To(HaveLen(1))
			Expect(engine.analyzers[0].name).To(Equal(domain.SaturationAnalyzerName))
			Expect(engine.analyzers[0].analyzer).To(BeIdenticalTo(engine.saturationV2Analyzer))
		})
	})

	Describe("RegisterAnalyzer", func() {
		It("appends new analyzers in registration order", func() {
			e := &Engine{
				analyzers: []analyzerEntry{
					{name: domain.SaturationAnalyzerName, analyzer: &spyAnalyzer{name: domain.SaturationAnalyzerName}},
				},
			}

			Expect(e.RegisterAnalyzer("throughput", &spyAnalyzer{name: "throughput"})).To(Succeed())
			Expect(e.RegisterAnalyzer("slo", &spyAnalyzer{name: "slo"})).To(Succeed())

			Expect(e.analyzers).To(HaveLen(3))
			Expect(e.analyzers[0].name).To(Equal(domain.SaturationAnalyzerName))
			Expect(e.analyzers[1].name).To(Equal("throughput"))
			Expect(e.analyzers[2].name).To(Equal("slo"))
		})

		It("returns an error when re-registering an existing name", func() {
			e := &Engine{
				analyzers: []analyzerEntry{
					{name: domain.SaturationAnalyzerName, analyzer: &spyAnalyzer{name: domain.SaturationAnalyzerName}},
					{name: "throughput", analyzer: &spyAnalyzer{name: "throughput"}},
				},
			}

			Expect(e.RegisterAnalyzer("throughput", &spyAnalyzer{name: "throughput"})).
				To(MatchError(ContainSubstring(`duplicate analyzer name "throughput"`)))

			Expect(e.RegisterAnalyzer(domain.SaturationAnalyzerName, &spyAnalyzer{name: "x"})).
				To(MatchError(ContainSubstring(`duplicate analyzer name`)))
		})

		It("returns an error when called after StartOptimizeLoop has frozen the registry", func() {
			e := &Engine{
				analyzers: []analyzerEntry{
					{name: domain.SaturationAnalyzerName, analyzer: &spyAnalyzer{name: domain.SaturationAnalyzerName}},
				},
				started: true,
			}

			Expect(e.RegisterAnalyzer("throughput", &spyAnalyzer{name: "throughput"})).
				To(MatchError(ContainSubstring("called after StartOptimizeLoop")))
		})
	})

	Describe("StartOptimizeLoop", func() {
		It("snapshots the analyzer registry and flips started before launching the executor", func() {
			sourceRegistry := source.NewSourceRegistry()
			Expect(sourceRegistry.Register("prometheus", source.NewNoOpSource())).To(Succeed())
			testConfig := config.NewTestConfig()
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))

			Expect(engine.RegisterAnalyzer("throughput", &spyAnalyzer{name: "throughput"})).To(Succeed())
			Expect(engine.RegisterAnalyzer("slo", &spyAnalyzer{name: "slo"})).To(Succeed())

			// Cancel context so the executor's polling loop exits immediately.
			startCtx, cancelStart := context.WithCancel(context.Background())
			cancelStart()
			engine.StartOptimizeLoop(startCtx)

			Expect(engine.started).To(BeTrue())
			Expect(engine.analyzersSnapshot).To(HaveLen(len(engine.analyzers)))
			for i := range engine.analyzers {
				Expect(engine.analyzersSnapshot[i].name).To(Equal(engine.analyzers[i].name))
				Expect(engine.analyzersSnapshot[i].analyzer).To(BeIdenticalTo(engine.analyzers[i].analyzer))
			}
		})

		It("snapshot reader does not race with a post-Start RegisterAnalyzer attempt", func() {
			// Verifies the race-safety contract under -race: the optimize
			// goroutine reads analyzersSnapshot (immutable after Start) while
			// any post-Start RegisterAnalyzer returns an error before mutating anything.
			sourceRegistry := source.NewSourceRegistry()
			Expect(sourceRegistry.Register("prometheus", source.NewNoOpSource())).To(Succeed())
			testConfig := config.NewTestConfig()
			engine := NewEngine(k8sClient, k8sClient, k8sClient.Scheme(), nil, sourceRegistry, testConfig, pipeline.NewNoOpLimiter("test"))
			Expect(engine.RegisterAnalyzer("throughput", &spyAnalyzer{name: "throughput"})).To(Succeed())

			startCtx, cancelStart := context.WithCancel(context.Background())
			cancelStart()
			engine.StartOptimizeLoop(startCtx)

			var wg sync.WaitGroup
			const iterations = 200

			// Reader: iterate the snapshot repeatedly.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					for _, entry := range engine.analyzersSnapshot {
						_ = entry.name
					}
				}
			}()

			// Writers: each call must return an error (called after Start).
			errCount := 0
			var mu sync.Mutex
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := engine.RegisterAnalyzer("late", &spyAnalyzer{name: "late"}); err != nil {
						mu.Lock()
						errCount++
						mu.Unlock()
					}
				}()
			}

			wg.Wait()
			Expect(errCount).To(Equal(4))
		})
	})

	Describe("runRegisteredAnalyzer", func() {

		var (
			testCtx    context.Context
			testLogger logr.Logger
		)

		BeforeEach(func() {
			testCtx = context.Background()
			testLogger = logf.Log
		})

		It("returns nil and does not panic when the analyzer returns an error", func() {
			spy := &spyAnalyzer{name: "throughput", err: errors.New("boom")}
			entry := analyzerEntry{name: "throughput", analyzer: spy}
			result := runRegisteredAnalyzer(testCtx, testLogger, entry, "model-1",
				domain.AnalyzerInput{ModelID: "model-1"})
			Expect(result).To(BeNil())
			Expect(spy.callCount).To(Equal(1))
		})

		It("recovers from a panicking analyzer and returns nil", func() {
			spy := &spyAnalyzer{name: "throughput", panicMsg: "boom"}
			entry := analyzerEntry{name: "throughput", analyzer: spy}
			var result *domain.AnalyzerResult
			Expect(func() {
				result = runRegisteredAnalyzer(testCtx, testLogger, entry, "model-1",
					domain.AnalyzerInput{ModelID: "model-1"})
			}).NotTo(Panic())
			Expect(result).To(BeNil())
		})
	})
})
