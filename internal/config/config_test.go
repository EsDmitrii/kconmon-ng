package config

import (
	"os"
	"path/filepath"
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
