package config

import (
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
)

// The shipped saturation ConfigMaps must select the V2 (token/capacity-based)
// analyzer by default as of v0.9.0. Every e2e suite overwrites the `default`
// entry with its own config, so without this test a YAML typo, an indentation
// slip, or an ApplyDefaults()/IsV2() regression could silently revert the
// shipped default back to V1 with no signal. This reads the actual files.
var _ = Describe("shipped saturation ConfigMap default entry", func() {
	DescribeTable("selects the V2 analyzer and validates",
		func(path string) {
			raw, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred(), "reading %s", path)

			var cm struct {
				Data map[string]string `yaml:"data"`
			}
			Expect(yaml.Unmarshal(raw, &cm)).To(Succeed(), "parsing ConfigMap %s", path)

			defaultEntry, ok := cm.Data["default"]
			Expect(ok).To(BeTrue(), "%s must have a data.default entry", path)

			var cfg SaturationScalingConfig
			Expect(yaml.Unmarshal([]byte(defaultEntry), &cfg)).To(Succeed(), "parsing default entry of %s", path)

			cfg.ApplyDefaults()
			Expect(cfg.Validate()).To(Succeed(), "%s default entry must validate", path)
			Expect(cfg.IsV2()).To(BeTrue(), "%s default entry must select the V2 analyzer", path)
			Expect(cfg.GetAnalyzerName()).To(Equal("saturation"), "%s default entry must resolve to the saturation (V2) analyzer", path)
		},
		// Paths are relative to this package directory (internal/config).
		Entry("kustomize base", "../../config/base/manager/saturation-scaling-configmap.yaml"),
		Entry("deploy overlay", "../../deploy/configmap-saturation-scaling.yaml"),
	)
})
