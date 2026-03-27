package config

import "time"

func DefaultConfig() *Config {
	return &Config{
		Mode:               "",
		MetricsPrefix:      "kconmon_ng",
		HTTPPort:           8080,
		GRPCPort:           9090,
		LogLevel:           "info",
		LogFormat:          "json",
		FailureDomainLabel: "topology.kubernetes.io/zone",
		Checkers: CheckersConfig{
			TCP: TCPCheckerConfig{
				Enabled:  true,
				Interval: 5 * time.Second,
				Timeout:  1 * time.Second,
			},
			UDP: UDPCheckerConfig{
				Enabled:  true,
				Interval: 5 * time.Second,
				Timeout:  250 * time.Millisecond,
				Packets:  5,
			},
			ICMP: ICMPCheckerConfig{
				Enabled:  true,
				Interval: 5 * time.Second,
				Timeout:  1 * time.Second,
			},
			DNS: DNSCheckerConfig{
				Enabled:  true,
				Interval: 5 * time.Second,
				Timeout:  5 * time.Second,
				Hosts:    []string{"kubernetes.default.svc.cluster.local"},
			},
			HTTP: HTTPCheckerConfig{
				Enabled:  false,
				Interval: 30 * time.Second,
				Timeout:  5 * time.Second,
				Targets:  nil,
			},
			MTR: MTRCheckerConfig{
				Cooldown: 60 * time.Second,
				MaxHops:  30,
			},
		},
		Controller: ControllerConfig{
			LeaderElection: true,
			AgentTTL:       30 * time.Second,
		},
		Observability: ObservabilityConfig{
			OTel: OTelConfig{
				Enabled:  false,
				Endpoint: "",
			},
		},
	}
}
