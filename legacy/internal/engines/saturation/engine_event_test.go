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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// newVAFixture builds a minimal VariantAutoscaling for event-emission tests.
func newVAFixture(name, namespace string) *llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	return &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

var _ = Describe("emitAcceleratorNotResolvedEvent", func() {
	var (
		recorder *record.FakeRecorder
		engine   *Engine
		va       *llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	)

	BeforeEach(func() {
		recorder = record.NewFakeRecorder(10)
		engine = &Engine{Recorder: recorder}
		va = newVAFixture("variant-a", "team-x")
	})

	It("records a Warning event with the AcceleratorNotResolved reason and a remediation message", func() {
		engine.emitAcceleratorNotResolvedEvent(va)

		var got string
		Eventually(recorder.Events).Should(Receive(&got))
		Expect(got).To(HavePrefix("Warning AcceleratorNotResolved "))
		Expect(got).To(ContainSubstring("nodeSelector"),
			"event message should mention nodeSelector remediation")
		Expect(got).To(ContainSubstring("accelerator_type=\"unresolved\""),
			"event message should note scaling metrics are still emitted with the unresolved label")
		Expect(got).To(ContainSubstring("saturation/capacity metrics are withheld"),
			"event message should warn that accelerator-specific metrics are gated until resolution")
	})

	It("is a no-op when the engine has no recorder configured", func() {
		engineWithoutRecorder := &Engine{Recorder: nil}

		Expect(func() { engineWithoutRecorder.emitAcceleratorNotResolvedEvent(va) }).
			NotTo(Panic())
	})

	It("produces identical event strings on repeat emissions to enable API-server-side dedup", func() {
		engine.emitAcceleratorNotResolvedEvent(va)
		engine.emitAcceleratorNotResolvedEvent(va)

		var first, second string
		Eventually(recorder.Events).Should(Receive(&first))
		Eventually(recorder.Events).Should(Receive(&second))
		Expect(first).To(Equal(second),
			"identical event strings let the API server's event aggregator collapse repeats into a single entry")
		Expect(first).To(ContainSubstring(corev1.EventTypeWarning),
			"event should be Warning-typed")
	})
})
