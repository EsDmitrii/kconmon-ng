package config

import (
	"strings"
	"testing"
)

// The gateway is a deliberate exposure decision, so the default must be off and the port merely
// pre-agreed for values files.
func TestDefaultConfigExternalGatewayDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Controller.ExternalGateway.Enabled {
		t.Error("externalGateway must default to disabled")
	}
	if cfg.Controller.ExternalGateway.Port != 9443 {
		t.Errorf("externalGateway.port default = %d, want 9443", cfg.Controller.ExternalGateway.Port)
	}
	if cfg.Agent.TLS.Enabled() {
		t.Errorf("default agent.tls must be the zero (plaintext) block, got %+v", cfg.Agent.TLS)
	}
	if cfg.Agent.BootstrapTokenFile != "" {
		t.Errorf("default agent.bootstrapTokenFile must be empty, got %q", cfg.Agent.BootstrapTokenFile)
	}
}

// An enabled gateway must be able to authenticate someone: cert+key and a token are the floor.
// Everything here is a startup error because at runtime it would be a silent open port.
func TestExternalGatewayValidation(t *testing.T) {
	valid := func() ExternalGatewayConfig {
		return ExternalGatewayConfig{
			Enabled:            true,
			Port:               9443,
			TLS:                GatewayTLSConfig{CertFile: "/certs/tls.crt", KeyFile: "/certs/tls.key"},
			BootstrapTokenFile: "/secrets/token",
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid gateway", func(*Config) {}, ""},
		{"valid with client CA", func(c *Config) {
			c.Controller.ExternalGateway.TLS.ClientCAFile = "/certs/ca.crt"
		}, ""},
		{"missing certFile", func(c *Config) {
			c.Controller.ExternalGateway.TLS.CertFile = ""
		}, "externalGateway.tls"},
		{"missing keyFile", func(c *Config) {
			c.Controller.ExternalGateway.TLS.KeyFile = ""
		}, "externalGateway.tls"},
		{"missing token file", func(c *Config) {
			c.Controller.ExternalGateway.BootstrapTokenFile = ""
		}, "bootstrapTokenFile"},
		{"port out of range", func(c *Config) {
			c.Controller.ExternalGateway.Port = 0
		}, "externalGateway.port"},
		{"port collides with grpcPort", func(c *Config) {
			c.Controller.ExternalGateway.Port = c.GRPCPort
		}, "must differ"},
		{"port collides with httpPort", func(c *Config) {
			c.Controller.ExternalGateway.Port = c.HTTPPort
		}, "must differ"},
		{"port collides with metricsPort", func(c *Config) {
			c.Controller.ExternalGateway.Port = c.MetricsPort
		}, "must differ"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Controller.ExternalGateway = valid()
			tt.mutate(cfg)
			err := NewLoader("").validate(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error should mention %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

// A disabled gateway is not validated at all, mirroring checkers.external: the block stays
// byte-identical to what the operator wrote until they flip the switch.
func TestExternalGatewayDisabledIsNotValidated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Controller.ExternalGateway = ExternalGatewayConfig{Enabled: false, Port: -1}
	if err := NewLoader("").validate(cfg); err != nil {
		t.Errorf("disabled gateway must not be validated, got %v", err)
	}
}

// The agent-side half-configurations fail at startup: a cert without its key cannot handshake, and
// a bearer token over the plaintext in-cluster dial is a published secret.
func TestAgentSecurityValidation(t *testing.T) {
	tests := []struct {
		name    string
		agent   AgentConfig
		wantErr string
	}{
		{"empty block is plaintext", AgentConfig{}, ""},
		{"ca only", AgentConfig{TLS: AgentTLSConfig{CAFile: "/etc/ca.pem"}}, ""},
		{"serverName only", AgentConfig{TLS: AgentTLSConfig{ServerName: "gw.example.com"}}, ""},
		{"full mTLS with token", AgentConfig{
			TLS:                AgentTLSConfig{CAFile: "/etc/ca.pem", CertFile: "/etc/c.pem", KeyFile: "/etc/k.pem"},
			BootstrapTokenFile: "/etc/token",
		}, ""},
		{"cert without key", AgentConfig{TLS: AgentTLSConfig{CertFile: "/etc/c.pem"}}, "set together"},
		{"key without cert", AgentConfig{TLS: AgentTLSConfig{KeyFile: "/etc/k.pem"}}, "set together"},
		{"token without tls", AgentConfig{BootstrapTokenFile: "/etc/token"}, "requires the agent.tls block"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Agent = tt.agent
			err := NewLoader("").validate(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error should mention %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

// Pins the exact yaml spellings on both sides: the loader decodes with KnownFields, so a drifted
// key in chart or packaging fails HERE, not in a cluster.
func TestLoadSecurityBlocksFromFile(t *testing.T) {
	content := `
agent:
  tls:
    caFile: /etc/kconmon-ng/ca.pem
    certFile: /etc/kconmon-ng/agent.pem
    keyFile: /etc/kconmon-ng/agent-key.pem
    serverName: kconmon-gw.example.com
  bootstrapTokenFile: /etc/kconmon-ng/token
controller:
  externalGateway:
    enabled: true
    port: 9443
    tls:
      certFile: /certs/tls.crt
      keyFile: /certs/tls.key
      clientCaFile: /certs/ca.crt
    bootstrapTokenFile: /secrets/token
`
	loader := NewLoader(writeConfig(t, content))
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := loader.Get()

	if cfg.Agent.TLS.CAFile != "/etc/kconmon-ng/ca.pem" ||
		cfg.Agent.TLS.CertFile != "/etc/kconmon-ng/agent.pem" ||
		cfg.Agent.TLS.KeyFile != "/etc/kconmon-ng/agent-key.pem" ||
		cfg.Agent.TLS.ServerName != "kconmon-gw.example.com" {
		t.Errorf("agent.tls did not load: %+v", cfg.Agent.TLS)
	}
	if cfg.Agent.BootstrapTokenFile != "/etc/kconmon-ng/token" {
		t.Errorf("agent.bootstrapTokenFile = %q", cfg.Agent.BootstrapTokenFile)
	}
	gw := cfg.Controller.ExternalGateway
	if !gw.Enabled || gw.Port != 9443 {
		t.Errorf("externalGateway enabled/port did not load: %+v", gw)
	}
	if gw.TLS.CertFile != "/certs/tls.crt" || gw.TLS.KeyFile != "/certs/tls.key" ||
		gw.TLS.ClientCAFile != "/certs/ca.crt" {
		t.Errorf("externalGateway.tls did not load: %+v", gw.TLS)
	}
	if gw.BootstrapTokenFile != "/secrets/token" {
		t.Errorf("externalGateway.bootstrapTokenFile = %q", gw.BootstrapTokenFile)
	}
}
