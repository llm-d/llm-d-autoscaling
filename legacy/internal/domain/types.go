package domain

type ServiceClassEntry struct {
	Model   string `yaml:"model"`
	SLOTPOT int    `yaml:"slo-tpot"`
	SLOTTFT int    `yaml:"slo-ttft"`
}

type ServiceClass struct {
	Name     string              `yaml:"name"`
	Priority int                 `yaml:"priority"`
	Data     []ServiceClassEntry `yaml:"data"`
}
