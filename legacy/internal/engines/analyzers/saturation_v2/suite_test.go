package saturation_v2

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSaturationV2(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Saturation V2 Suite")
}
