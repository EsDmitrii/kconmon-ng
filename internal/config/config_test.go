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
