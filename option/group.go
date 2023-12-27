package option

type SelectorOutboundOptions struct {
	Outbounds                 []string `json:"outbounds"`
	Default                   string   `json:"default,omitempty"`
	InterruptExistConnections bool     `json:"interrupt_exist_connections,omitempty"`
}

type URLTestOutboundOptions struct {
	Outbounds                 []string `json:"outbounds"`
	URL                       string   `json:"url,omitempty"`
	Interval                  Duration `json:"interval,omitempty"`
	Tolerance                 uint16   `json:"tolerance,omitempty"`
	IdleTimeout               Duration `json:"idle_timeout,omitempty"`
	InterruptExistConnections bool     `json:"interrupt_exist_connections,omitempty"`
}

type LoadBalanceOutboundOptions struct {
	Outbounds []string `json:"outbounds"`
	Weights   []int64  `json:"weights,omitempty"`
}
