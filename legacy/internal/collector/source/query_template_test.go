package source

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("EscapePromQLValue", func() {
	It("returns empty string unchanged", func() {
		Expect(EscapePromQLValue("")).To(Equal(""))
	})

	It("escapes backslashes", func() {
		Expect(EscapePromQLValue(`\`)).To(Equal(`\\`))
		Expect(EscapePromQLValue(`foo\bar`)).To(Equal(`foo\\bar`))
		Expect(EscapePromQLValue(`\\`)).To(Equal(`\\\\`))
	})

	It("escapes double quotes", func() {
		Expect(EscapePromQLValue(`"`)).To(Equal(`\"`))
		Expect(EscapePromQLValue(`foo"bar`)).To(Equal(`foo\"bar`))
		Expect(EscapePromQLValue(`""`)).To(Equal(`\"\"`))
	})

	It("escapes backslashes before quotes so order is correct", func() {
		// Backslash first, then quote - result is \\ then \"
		Expect(EscapePromQLValue(`\""`)).To(Equal(`\\\"\"`))
	})

	It("leaves safe characters unchanged", func() {
		Expect(EscapePromQLValue("test-ns")).To(Equal("test-ns"))
		Expect(EscapePromQLValue("my-model-id")).To(Equal("my-model-id"))
		Expect(EscapePromQLValue("a1b2c3")).To(Equal("a1b2c3"))
	})

	It("prevents PromQL label injection by escaping malicious payload", func() {
		// If unescaped, this could close the label and inject another: namespace="other"
		malicious := `prod",namespace="other"`
		escaped := EscapePromQLValue(malicious)
		Expect(escaped).To(Equal(`prod\",namespace=\"other\"`))
		// The escaped value should be safe to embed in: metric{namespace="<value>"}
		// i.e. metric{namespace="prod\",namespace=\"other\""} is one literal label value
		Expect(escaped).NotTo(ContainSubstring(`namespace="other`))
	})
})
