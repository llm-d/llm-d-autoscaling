package registration

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestRegistration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Registration Suite")
}
