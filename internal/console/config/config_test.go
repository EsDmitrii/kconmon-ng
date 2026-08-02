package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsWhenFileMissing(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("expected defaults, got error: %v", err)
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("HTTPPort default = %d, want 8080", cfg.HTTPPort)
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "json" {
		t.Errorf("log defaults = %q/%q, want info/json", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.MetricsPrefix != "kconmon_ng" {
		t.Errorf("MetricsPrefix default = %q, want kconmon_ng", cfg.MetricsPrefix)
	}
	if cfg.Auth.Mode != "anonymous" || cfg.Auth.Anonymous.Role != "viewer" {
		t.Errorf("auth defaults = %q/%q, want anonymous/viewer", cfg.Auth.Mode, cfg.Auth.Anonymous.Role)
	}
}

func TestLoadFromFileOverrides(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("httpPort: 9000\nlogLevel: debug\nauth:\n  mode: anonymous\n  anonymous:\n    role: operator\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPPort != 9000 || cfg.LogLevel != "debug" || cfg.Auth.Anonymous.Role != "operator" {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestValidateRejectsNonAnonymousMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("auth:\n  mode: oidc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for auth.mode=oidc in M0, got nil")
	}
}

func TestValidateRejectsBadPort(t *testing.T) {
	c := &Config{HTTPPort: 0, LogLevel: "info", LogFormat: "json", MetricsPrefix: "kconmon_ng", Auth: AuthConfig{Mode: "anonymous"}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for httpPort 0")
	}
}

func TestLoadControllerPrometheusDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Controller.URL != "" || cfg.Prometheus.URL != "" {
		t.Errorf("controller/prometheus URLs must default empty, got %q %q", cfg.Controller.URL, cfg.Prometheus.URL)
	}
	if cfg.Controller.Timeout != 10*time.Second {
		t.Errorf("controller.timeout default: got %v", cfg.Controller.Timeout)
	}
	if cfg.Prometheus.QueryTimeout != 30*time.Second || cfg.Prometheus.MaxRange != 24*time.Hour || cfg.Prometheus.MaxResponseBytes != 8<<20 {
		t.Errorf("prometheus defaults wrong: %+v", cfg.Prometheus)
	}
}

func TestValidateRejectsBadURLs(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"controller not http", "controller:\n  url: \"ftp://x\"\n"},
		{"prometheus not absolute", "prometheus:\n  url: \"prometheus:9090\"\n"},
		{"nonpositive timeout", "controller:\n  url: \"http://c:8080\"\n  timeout: -1s\n"},
		{"nonpositive maxRange", "prometheus:\n  url: \"http://p:9090\"\n  maxRange: 0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestLoadControllerGRPCAddrAndValkeyDefaults(t *testing.T) {
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("Load defaults: %v", err)
	}
	if cfg.Controller.GRPCAddr != "" {
		t.Errorf("controller.grpcAddr must default empty, got %q", cfg.Controller.GRPCAddr)
	}
	if cfg.Valkey.Address != "" {
		t.Errorf("valkey.address must default empty, got %q", cfg.Valkey.Address)
	}
	if cfg.Valkey.DialTimeout != 5*time.Second {
		t.Errorf("valkey.dialTimeout default: got %v", cfg.Valkey.DialTimeout)
	}
}

func TestLoadValkeyAddress(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	y := "valkey:\n  address: \"kconmon-ng-console-valkey:6379\"\n  dialTimeout: 2s\n" +
		"controller:\n  grpcAddr: \"kconmon-ng-controller:9090\"\n"
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Valkey.Address != "kconmon-ng-console-valkey:6379" || cfg.Valkey.DialTimeout != 2*time.Second {
		t.Errorf("valkey config not applied: %+v", cfg.Valkey)
	}
	if cfg.Controller.GRPCAddr != "kconmon-ng-controller:9090" {
		t.Errorf("controller.grpcAddr not applied: %q", cfg.Controller.GRPCAddr)
	}
}

func TestValidateRejectsBadValkeyAddress(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"missing port", "valkey:\n  address: \"not-a-host-port\"\n"},
		{"nonpositive dialTimeout", "valkey:\n  address: \"v:6379\"\n  dialTimeout: 0s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(p); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("httpPrt: 9000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown key httpPrt, got nil")
	}
}
