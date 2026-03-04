package worker

type Config struct {
	ProxyID        string `json:"proxy_id"`
	Node           string `json:"node"`
	Service        string `json:"service"`
	Provider       string `json:"provider"`
	Domain         string `json:"domain"`
	TargetInfo     string `json:"target_info"`
	FlowDefinition string `json:"flow_def"`
	FlowID         string `json:"flow_id"`
}

func (c *Config) IsInitiator() bool {
	return c.FlowID != ""
}

func (c *Config) IsTarget() bool {
	return c.FlowDefinition != ""
}
