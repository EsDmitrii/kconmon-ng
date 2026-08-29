package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.MetricsPrefix != "kconmon_ng" {
		t.Errorf("expected metrics prefix kconmon_ng, got %s", cfg.MetricsPrefix)
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("expected HTTP port 8080, got %d", cfg.HTTPPort)
	}
	if cfg.GRPCPort != 9090 {
		t.Errorf("expected gRPC port 9090, got %d", cfg.GRPCPort)
	}
	if !cfg.Checkers.TCP.Enabled {
		t.Error("expected TCP checker enabled by default")
	}
	if cfg.Checkers.UDP.Packets != 5 {
		t.Errorf("expected 5 UDP packets, got %d", cfg.Checkers.UDP.Packets)
	}
	if cfg.Checkers.MTR.MaxHops != 30 {
		t.Errorf("expected 30 max hops, got %d", cfg.Checkers.MTR.MaxHops)
	}
}

func TestLoadFromFile(t *testing.T) {
	content := `
httpPort: 9999
grpcPort: 8888
logLevel: debug
metricsPrefix: custom_prefix
checkers:
  tcp:
    enabled: false
    interval: 10s
    timeout: 2s
  udp:
    enabled: true
    interval: 3s
    timeout: 500ms
    packets: 10
`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	loader := NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}

	cfg := loader.Get()

	if cfg.HTTPPort != 9999 {
		t.Errorf("expected HTTP port 9999, got %d", cfg.HTTPPort)
	}
	if cfg.GRPCPort != 8888 {
		t.Errorf("expected gRPC port 8888, got %d", cfg.GRPCPort)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level debug, got %s", cfg.LogLevel)
	}
	if cfg.MetricsPrefix != "custom_prefix" {
		t.Errorf("expected prefix custom_prefix, got %s", cfg.MetricsPrefix)
	}
	if cfg.Checkers.TCP.Enabled {
		t.Error("expected TCP checker disabled")
	}
	if cfg.Checkers.TCP.Interval != 10*time.Second {
		t.Errorf("expected TCP interval 10s, got %v", cfg.Checkers.TCP.Interval)
	}
	if cfg.Checkers.UDP.Packets != 10 {
		t.Errorf("expected 10 UDP packets, got %d", cfg.Checkers.UDP.Packets)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("KCONMON_NG_LOG_LEVEL", "debug")
	t.Setenv("KCONMON_NG_METRICS_PREFIX", "env_prefix")
	t.Setenv("KCONMON_NG_CONTROLLER_ADDRESS", "controller:9090")

	loader := NewLoader("")
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}

	cfg := loader.Get()

	if cfg.LogLevel != "debug" {
		t.Errorf("expected log level debug, got %s", cfg.LogLevel)
	}
	if cfg.MetricsPrefix != "env_prefix" {
		t.Errorf("expected prefix env_prefix, got %s", cfg.MetricsPrefix)
	}
	if cfg.ControllerAddress != "controller:9090" {
		t.Errorf("expected controller address controller:9090, got %s", cfg.ControllerAddress)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid default config",
			modify:  func(_ *Config) {},
			wantErr: false,
		},
		{
			name: "invalid HTTP port zero",
			modify: func(c *Config) {
				c.HTTPPort = 0
			},
			wantErr: true,
		},
		{
			name: "invalid HTTP port too high",
			modify: func(c *Config) {
				c.HTTPPort = 70000
			},
			wantErr: true,
		},
		{
			name: "same HTTP and gRPC port",
			modify: func(c *Config) {
				c.HTTPPort = 8080
				c.GRPCPort = 8080
			},
			wantErr: true,
		},
		{
			name: "invalid log level",
			modify: func(c *Config) {
				c.LogLevel = "verbose"
			},
			wantErr: true,
		},
		{
			name: "zero UDP packets",
			modify: func(c *Config) {
				c.Checkers.UDP.Packets = 0
			},
			wantErr: true,
		},
		{
			name: "MTR max hops too high",
			modify: func(c *Config) {
				c.Checkers.MTR.MaxHops = 100
			},
			wantErr: true,
		},
		{
			name: "invalid log format",
			modify: func(c *Config) {
				c.LogFormat = "yaml"
			},
			wantErr: true,
		},
		{
			name: "valid log format text",
			modify: func(c *Config) {
				c.LogFormat = "text"
			},
			wantErr: false,
		},
		{
			name: "valid log format json",
			modify: func(c *Config) {
				c.LogFormat = "json"
			},
			wantErr: false,
		},
		{
			name: "enabled checker with zero interval",
			modify: func(c *Config) {
				c.Checkers.TCP.Enabled = true
				c.Checkers.TCP.Interval = 0
			},
			wantErr: true,
		},
		{
			name: "enabled checker with zero timeout",
			modify: func(c *Config) {
				c.Checkers.TCP.Enabled = true
				c.Checkers.TCP.Timeout = 0
			},
			wantErr: true,
		},
		{
			name: "disabled checker with zero interval is ok",
			modify: func(c *Config) {
				c.Checkers.HTTP.Enabled = false
				c.Checkers.HTTP.Interval = 0
				c.Checkers.HTTP.Timeout = 0
			},
			wantErr: false,
		},
		{
			name: "timeout >= interval is a warning not error",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Interval = 5 * time.Second
				c.Checkers.DNS.Timeout = 5 * time.Second
			},
			wantErr: false,
		},
		{
			name: "dns enabled with no hosts",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Hosts = nil
			},
			wantErr: true,
		},
		{
			name: "dns enabled with empty-string host",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Hosts = []string{"  "}
			},
			wantErr: true,
		},
		{
			name: "dns resolver host only is valid",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Resolvers = []string{"8.8.8.8"}
			},
			wantErr: false,
		},
		{
			name: "dns resolver host:port is valid",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Resolvers = []string{"8.8.8.8:53"}
			},
			wantErr: false,
		},
		{
			name: "dns resolver with bad port",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Resolvers = []string{"8.8.8.8:notaport"}
			},
			wantErr: true,
		},
		{
			name: "dns resolver empty string",
			modify: func(c *Config) {
				c.Checkers.DNS.Enabled = true
				c.Checkers.DNS.Resolvers = []string{""}
			},
			wantErr: true,
		},
		{
			name: "http enabled with valid target",
			modify: func(c *Config) {
				c.Checkers.HTTP.Enabled = true
				c.Checkers.HTTP.Targets = []HTTPTarget{{URL: "https://example.com/healthz"}}
			},
			wantErr: false,
		},
		{
			name: "http enabled with no targets",
			modify: func(c *Config) {
				c.Checkers.HTTP.Enabled = true
				c.Checkers.HTTP.Targets = nil
			},
			wantErr: true,
		},
		{
			name: "http target with empty url",
			modify: func(c *Config) {
				c.Checkers.HTTP.Enabled = true
				c.Checkers.HTTP.Targets = []HTTPTarget{{URL: ""}}
			},
			wantErr: true,
		},
		{
			name: "http target with unsupported scheme",
			modify: func(c *Config) {
				c.Checkers.HTTP.Enabled = true
				c.Checkers.HTTP.Targets = []HTTPTarget{{URL: "ftp://example.com/x"}}
			},
			wantErr: true,
		},
		{
			name: "http target missing host",
			modify: func(c *Config) {
				c.Checkers.HTTP.Enabled = true
				c.Checkers.HTTP.Targets = []HTTPTarget{{URL: "http:///healthz"}}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loader := NewLoader("")
			cfg := DefaultConfig()
			tt.modify(cfg)

			err := loader.validate(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigGetReturnsCopy(t *testing.T) {
	loader := NewLoader("")
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}

	cfg1 := loader.Get()
	cfg2 := loader.Get()

	cfg1.HTTPPort = 12345
	if cfg2.HTTPPort == 12345 {
		t.Error("Get() should return a copy, not a reference")
	}
}

func TestHotReload(t *testing.T) {
	content := `
httpPort: 8080
grpcPort: 9090
logLevel: info
`

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	loader := NewLoader(path)
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}

	changed := make(chan *Config, 1)
	loader.OnChange(func(cfg *Config) {
		select {
		case changed <- cfg:
		default:
			<-changed
			changed <- cfg
		}
	})

	if err := loader.WatchForChanges(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loader.Close() }()

	select {
	case <-changed:
	default:
	}

	newContent := `
httpPort: 7777
grpcPort: 9090
logLevel: debug
`

	if err := os.WriteFile(path, []byte(newContent), 0o600); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case cfg := <-changed:
			if cfg.HTTPPort == 7777 && cfg.LogLevel == "debug" {
				return
			}
		case <-deadline:
			last := loader.Get()
			t.Fatalf(
				"timeout waiting for config reload (last seen: httpPort=%d logLevel=%s)",
				last.HTTPPort,
				last.LogLevel,
			)
		}
	}
}

func TestDefaultConfigEventsDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Controller.Events.Enabled {
		t.Error("expected controller.events.enabled to default false")
	}
}

func TestLoadFromFileEventsEnabled(t *testing.T) {
	l := NewLoader("")
	cfg := DefaultConfig()
	data := []byte("controller:\n  leaderElection: true\n  agentTtl: 30s\n  events:\n    enabled: true\n")
	dir := t.TempDir()
	p := dir + "/config.yaml"
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	l.filePath = p
	if err := l.loadFromFile(cfg); err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if !cfg.Controller.Events.Enabled {
		t.Error("expected controller.events.enabled true after load")
	}
}

// writeExternalConfig writes a minimal valid config with the given
// checkers.external block and returns its path.
func writeExternalConfig(t *testing.T, external string) string {
	t.Helper()
	content := "httpPort: 8080\ngrpcPort: 9090\nlogLevel: info\nlogFormat: json\ncheckers:\n  external:\n" + external
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultConfigExternalChecksDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Checkers.External.Enabled {
		t.Error("checkers.external.enabled must default to false: external probing is opt-in")
	}
	if len(cfg.Checkers.External.AllowedCIDRs) != 0 {
		t.Errorf("checkers.external.allowedCidrs must have no default, got %v", cfg.Checkers.External.AllowedCIDRs)
	}
}

func TestExternalMalformedCIDRIsIndexedStartupError(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n    allowedCidrs:\n      - 10.0.0.0/8\n      - 192.168.0.0/16\n      - nonsense\n"))
	err := l.Load()
	if err == nil {
		t.Fatal("a malformed allowed CIDR must fail startup")
	}
	if !strings.Contains(err.Error(), "checkers.external.allowedCidrs[2]") {
		t.Errorf("error must be indexed as checkers.external.allowedCidrs[2], got: %v", err)
	}
}

func TestExternalMalformedDeniedCIDRIsIndexedStartupError(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n    allowedCidrs:\n      - 10.0.0.0/8\n    deniedCidrs:\n      - 10.1.0.0/16\n      - 300.0.0.0/8\n"))
	err := l.Load()
	if err == nil {
		t.Fatal("a malformed denied CIDR must fail startup")
	}
	if !strings.Contains(err.Error(), "checkers.external.deniedCidrs[1]") {
		t.Errorf("error must be indexed as checkers.external.deniedCidrs[1], got: %v", err)
	}
}

func TestExternalEnabledWithEmptyAllowedCIDRsIsError(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n"))
	err := l.Load()
	if err == nil {
		t.Fatal("an enabled external block with no allowedCidrs must fail: an empty list is not 'allow everything'")
	}
	if !strings.Contains(err.Error(), "allowedCidrs") {
		t.Errorf("error must name allowedCidrs, got: %v", err)
	}
}

// A disabled block is inert: it is not even parsed into an Allowlist, so an
// operator can leave a half-written block in values.yaml without bricking the
// agent. The gate is checkers.external.enabled, nothing else.
func TestExternalDisabledWithGarbageStillLoads(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: false\n    allowedCidrs:\n      - total-nonsense\n      - 999.999.999.999/99\n    deniedCidrs:\n      - also-garbage\n    maxTargets: -5\n    timeout: -3s\n"))
	if err := l.Load(); err != nil {
		t.Fatalf("a disabled external block must load regardless of its contents, got: %v", err)
	}
	if l.Get().Checkers.External.Enabled {
		t.Error("the block must stay disabled")
	}
}

func TestExternalEnabledFillsDefaultsForZeroValues(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n    allowedCidrs:\n      - 10.0.0.0/8\n"))
	if err := l.Load(); err != nil {
		t.Fatalf("valid enabled external block must load, got: %v", err)
	}
	ext := l.Get().Checkers.External
	if ext.MaxTargets != defaultExternalMaxTargets {
		t.Errorf("maxTargets = %d, want the %d default", ext.MaxTargets, defaultExternalMaxTargets)
	}
	if ext.Timeout != defaultExternalTimeout {
		t.Errorf("timeout = %v, want the %v default", ext.Timeout, defaultExternalTimeout)
	}
}

func TestExternalEnabledKeepsExplicitValues(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n    allowedCidrs:\n      - 10.0.0.0/8\n      - 2001:db8::/32\n    deniedCidrs:\n      - 10.1.2.0/24\n    maxTargets: 7\n    timeout: 3s\n"))
	if err := l.Load(); err != nil {
		t.Fatalf("valid enabled external block must load, got: %v", err)
	}
	ext := l.Get().Checkers.External
	if ext.MaxTargets != 7 {
		t.Errorf("maxTargets = %d, want 7", ext.MaxTargets)
	}
	if ext.Timeout != 3*time.Second {
		t.Errorf("timeout = %v, want 3s", ext.Timeout)
	}
	if len(ext.AllowedCIDRs) != 2 || len(ext.DeniedCIDRs) != 1 {
		t.Errorf("CIDR lists not loaded: allowed=%v denied=%v", ext.AllowedCIDRs, ext.DeniedCIDRs)
	}
}

func TestExternalEnabledRejectsNegativeMaxTargetsAndTimeout(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n    allowedCidrs:\n      - 10.0.0.0/8\n    maxTargets: -1\n"))
	if err := l.Load(); err == nil {
		t.Error("a negative maxTargets must fail validation")
	}

	l2 := NewLoader(writeExternalConfig(t, "    enabled: true\n    allowedCidrs:\n      - 10.0.0.0/8\n    timeout: -1s\n"))
	if err := l2.Load(); err == nil {
		t.Error("a negative timeout must fail validation")
	}
}

// An operator who allows nothing but denies something has still allowed
// nothing: the enabled path requires a non-empty allowlist, deniedCidrs alone
// is not a configuration.
func TestExternalEnabledWithOnlyDeniedCIDRsIsError(t *testing.T) {
	l := NewLoader(writeExternalConfig(t, "    enabled: true\n    deniedCidrs:\n      - 169.254.0.0/16\n"))
	if err := l.Load(); err == nil {
		t.Error("deniedCidrs alone must not satisfy the enabled path")
	}
}

// The agent identity block defaults to all-empty: in-cluster the Downward API
// env fills it, and an empty block must keep today's behavior byte-identical.
func TestDefaultConfigAgentIdentityIsEmpty(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.NodeName != "" || cfg.Agent.AdvertiseAddress != "" || cfg.Agent.Zone != "" {
		t.Errorf("default agent identity must be empty, got %+v", cfg.Agent)
	}
}

func TestLoadAgentIdentityFromFile(t *testing.T) {
	content := `
agent:
  nodeName: edge-host-1
  advertiseAddress: 198.51.100.7
  zone: dc-east
`
	loader := NewLoader(writeConfig(t, content))
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := loader.Get()
	if cfg.Agent.NodeName != "edge-host-1" {
		t.Errorf("agent.nodeName = %q, want edge-host-1", cfg.Agent.NodeName)
	}
	if cfg.Agent.AdvertiseAddress != "198.51.100.7" {
		t.Errorf("agent.advertiseAddress = %q, want 198.51.100.7", cfg.Agent.AdvertiseAddress)
	}
	if cfg.Agent.Zone != "dc-east" {
		t.Errorf("agent.zone = %q, want dc-east", cfg.Agent.Zone)
	}
}

// Env beats file for the whole identity block, mirroring every other override:
// this is what lets the chart's Downward API env fill nodeName/zone while the
// same ConfigMap is mounted on every node.
func TestAgentIdentityEnvOverrides(t *testing.T) {
	t.Setenv("KCONMON_NG_NODE_NAME", "env-node")
	t.Setenv("KCONMON_NG_ADVERTISE_ADDRESS", "192.0.2.33")
	t.Setenv("KCONMON_NG_ZONE", "env-zone")

	content := `
agent:
  nodeName: file-node
  advertiseAddress: 198.51.100.7
  zone: file-zone
`
	loader := NewLoader(writeConfig(t, content))
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := loader.Get()
	if cfg.Agent.NodeName != "env-node" {
		t.Errorf("agent.nodeName = %q, want env override env-node", cfg.Agent.NodeName)
	}
	if cfg.Agent.AdvertiseAddress != "192.0.2.33" {
		t.Errorf("agent.advertiseAddress = %q, want env override 192.0.2.33", cfg.Agent.AdvertiseAddress)
	}
	if cfg.Agent.Zone != "env-zone" {
		t.Errorf("agent.zone = %q, want env override env-zone", cfg.Agent.Zone)
	}
}

// agent.advertiseAddress is published to every peer as a probe target and the
// controller refuses anything net.ParseIP refuses (validateAgentMeta), so a
// hostname or host:port must fail at startup, not at registration.
func TestAgentAdvertiseAddressMustBeAnIPLiteral(t *testing.T) {
	tests := []struct {
		name    string
		address string
		wantErr bool
	}{
		{"empty is allowed and means autodetect", "", false},
		{"IPv4 literal", "10.1.2.3", false},
		{"IPv6 literal", "2001:db8::7", false},
		{"hostname refused", "edge-host-1.example.com", true},
		{"host:port refused", "10.1.2.3:8080", true},
		{"garbage refused", "not-an-ip", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Agent.AdvertiseAddress = tt.address
			loader := NewLoader("")
			err := loader.validate(cfg)
			if tt.wantErr && err == nil {
				t.Errorf("advertiseAddress %q must be rejected", tt.address)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("advertiseAddress %q must be accepted, got %v", tt.address, err)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "advertiseAddress") {
				t.Errorf("error should name the offending key, got: %v", err)
			}
		})
	}
}
