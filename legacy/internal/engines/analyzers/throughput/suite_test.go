package throughput

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestThroughput(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Throughput Analyzer Suite")
}
