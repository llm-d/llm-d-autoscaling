package constants_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

var _ = Describe("IsAcceleratorResolved", func() {
	DescribeTable("classifies an accelerator name as resolved or not",
		func(name string, want bool) {
			Expect(constants.IsAcceleratorResolved(name)).To(Equal(want))
		},
		Entry("empty is unresolved", "", false),
		Entry("the \"unknown\" internal sentinel is unresolved", constants.DefaultAcceleratorName, false),
		Entry("the \"unresolved\" label placeholder is unresolved", constants.UnresolvedAcceleratorType, false),
		Entry("a real accelerator type is resolved", "A100", true),
		Entry("a real long accelerator type is resolved", "NVIDIA-A100-PCIE-80GB", true),
	)
})
