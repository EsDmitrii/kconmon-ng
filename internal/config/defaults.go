package config

import "time"

func DefaultConfig() *Config {
	return &Config{
		Mode:               "",
		MetricsPrefix:      "kconmon_ng",
		HTTPPort:           8080,
		MetricsPort:        9091,
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
			// External probing is opt-in and has no default allowlist on purpose.
			External: ExternalCheckerConfig{
				Enabled:      false,
				AllowedCIDRs: nil,
				DeniedCIDRs:  nil,
			},
		},
		Controller: ControllerConfig{
			LeaderElection: true,
			AgentTTL:       30 * time.Second,
			Events:         EventsConfig{Enabled: false},
			// Off by default: enabling the gateway is a deliberate exposure decision, and 9443
			// only pre-picks a port so values files agree on one.
			ExternalGateway: ExternalGatewayConfig{Enabled: false, Port: 9443},
		},
		// Full mesh by default: sparse is an opt-in for fleets where N*(N-1) pairs stop being
		// affordable. The sparse knobs default to a plan that stays connected (ring) and keeps
		// every zone pair observed (chords).
		Topology: TopologyConfig{
			Mode: TopologyModeFull,
			Sparse: SparseTopologyConfig{
				RingDegree:    2,
				ZoneChords:    2,
				AutoThreshold: 0,
			},
		},
		Observability: ObservabilityConfig{
			OTel: OTelConfig{
				Enabled:  false,
				Endpoint: "",
			},
		},
	}
}
