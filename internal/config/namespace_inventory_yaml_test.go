package config

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	yaml "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/labels"
)

// labelSet builds a node label set for selector matching assertions.
func labelSet(kv ...string) labels.Set {
	s := labels.Set{}
	for i := 0; i+1 < len(kv); i += 2 {
		s[kv[i]] = kv[i+1]
	}
	return s
}

// The saturation ConfigMap is decoded with gopkg.in/yaml.v3, which ignores json
// tags. A selector type carrying only json tags (metav1.LabelSelector) parses as
// empty, compiles to labels.Everything(), and silently hands one namespace every
// node. These round-trip the documented YAML to keep that from regressing.
var _ = Describe("namespace-inventory YAML decoding", func() {
	decode := func(src string) SaturationScalingConfig {
		GinkgoHelper()
		var c SaturationScalingConfig
		Expect(yaml.Unmarshal([]byte(src), &c)).To(Succeed())
		return c
	}

	It("preserves matchLabels through a yaml.v3 decode", func() {
		c := decode(`
limiters:
  - name: namespace-inventory
    type: namespace-inventory
    exclude:
      - kube-system
    selectors:
      ns-prod:
        matchLabels:
          team: prod
`)
		Expect(c.Limiters).To(HaveLen(1))
		entry := c.Limiters[0]
		Expect(entry.Exclude).To(ConsistOf("kube-system"))
		Expect(entry.Selectors["ns-prod"].MatchLabels).To(HaveKeyWithValue("team", "prod"),
			"an empty selector here would match every node")
		Expect(entry.Selectors["ns-prod"].IsEmpty()).To(BeFalse())
	})

	It("preserves matchExpressions through a yaml.v3 decode", func() {
		c := decode(`
limiters:
  - name: namespace-inventory
    type: namespace-inventory
    selectors:
      ns-dev:
        matchExpressions:
          - key: team
            operator: In
            values: [dev, dev-canary]
`)
		reqs := c.Limiters[0].Selectors["ns-dev"].MatchExpressions
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Key).To(Equal("team"))
		Expect(reqs[0].Operator).To(Equal("In"))
		Expect(reqs[0].Values).To(ConsistOf("dev", "dev-canary"))
	})

	It("compiles a decoded selector to one that does not match every node", func() {
		c := decode(`
limiters:
  - name: namespace-inventory
    type: namespace-inventory
    selectors:
      ns-prod:
        matchLabels:
          team: prod
`)
		sel, err := c.Limiters[0].Selectors["ns-prod"].Compile()
		Expect(err).NotTo(HaveOccurred())
		Expect(sel.Matches(labelSet("team", "prod"))).To(BeTrue())
		Expect(sel.Matches(labelSet("team", "dev"))).To(BeFalse(),
			"a match-all selector would give this namespace the whole cluster")
	})

	It("passes validation for the documented example", func() {
		c := decode(`
limiters:
  - name: namespace-inventory
    type: namespace-inventory
    exclude:
      - kube-system
    selectors:
      llm-d-prod:
        matchLabels:
          team: prod
      default:
        matchLabels:
          pool: shared
`)
		Expect(c.validateLimiters()).To(Succeed())
	})
})
