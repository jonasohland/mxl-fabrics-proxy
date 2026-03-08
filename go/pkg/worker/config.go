package worker

type Config struct {
	Target         bool   `json:"target"`
	MetricsSocket  string `json:"metrics_socket"`
	ProxyID        string `json:"proxy_id"`
	Node           string `json:"node"`
	Service        string `json:"service"`
	Provider       string `json:"provider"`
	Domain         string `json:"domain"`
	TargetInfo     string `json:"target_info"`
	FlowDefinition string `json:"flow_def"`
	FlowID         string `json:"flow_id"`
	EFAUseWait     bool   `json:"efa_use_wait"`
}

func (c *Config) IsInitiator() bool {
	return !c.Target
}

func (c *Config) IsTarget() bool {
	return c.Target
}
