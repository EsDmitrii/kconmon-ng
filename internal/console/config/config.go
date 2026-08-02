// Package config loads and validates the Console binary's runtime configuration.
// It is intentionally separate from internal/config (agent/controller config)
// but reuses that package's SetupLogger and version vars in cmd/console.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the Console runtime configuration. In M0 the only supported auth
// mode is "anonymous".
type Config struct {
	HTTPPort      int        `yaml:"httpPort"`
	LogLevel      string     `yaml:"logLevel"`
	LogFormat     string     `yaml:"logFormat"`
	MetricsPrefix string     `yaml:"metricsPrefix"`
	Auth          AuthConfig `yaml:"auth"`

	Controller ControllerConfig `yaml:"controller"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Valkey     ValkeyConfig     `yaml:"valkey"`
}

// ControllerConfig configures the console's HTTP client for the controller
// topology API. An empty URL disables the feature (endpoints answer 503).
type ControllerConfig struct {
	URL      string        `yaml:"url"`      // e.g. http://kconmon-ng-controller:8080; empty = feature disabled (503)
	Timeout  time.Duration `yaml:"timeout"`  // per-request; default 10s
	GRPCAddr string        `yaml:"grpcAddr"` // e.g. kconmon-ng-controller:9090; empty = realtime disabled (M1 polling only)
}

// ValkeyConfig configures the console's Valkey pub/sub client (internal/console/cache).
// An empty Address disables Valkey: the console falls back to an in-process
// bus (single-replica only; see ADR-002).
type ValkeyConfig struct {
	Address     string        `yaml:"address"`     // host:port; empty = disabled (in-process fallback)
	DialTimeout time.Duration `yaml:"dialTimeout"` // default 5s
}

// PrometheusConfig configures the console's Prometheus HTTP API client.
// An empty URL disables the feature (endpoints answer 503).
type PrometheusConfig struct {
	URL              string        `yaml:"url"`              // e.g. http://prometheus.monitoring:9090; empty = disabled (503)
	QueryTimeout     time.Duration `yaml:"queryTimeout"`     // default 30s
	MaxRange         time.Duration `yaml:"maxRange"`         // default 24h — query_range end-start cap
	MaxResponseBytes int64         `yaml:"maxResponseBytes"` // default 8388608 (8 MiB)
}

// AuthConfig selects the authentication mode. M0 supports only "anonymous".
type AuthConfig struct {
	Mode      string          `yaml:"mode"`
	Anonymous AnonymousConfig `yaml:"anonymous"`
}

// AnonymousConfig configures the fixed role used in anonymous mode.
type AnonymousConfig struct {
	Role string `yaml:"role"`
}

func defaults() *Config {
	return &Config{
		HTTPPort:      8080,
		LogLevel:      "info",
		LogFormat:     "json",
		MetricsPrefix: "kconmon_ng",
		Auth:          AuthConfig{Mode: "anonymous", Anonymous: AnonymousConfig{Role: "viewer"}},
		Controller:    ControllerConfig{Timeout: 10 * time.Second},
		Prometheus:    PrometheusConfig{QueryTimeout: 30 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 8 << 20},
		Valkey:        ValkeyConfig{DialTimeout: 5 * time.Second},
	}
}

// Load reads the YAML config at path onto a defaulted Config and validates it.
// A missing file is not an error: defaults are returned (validated).
func Load(path string) (*Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path) //nolint:gosec // path comes from operator config, not user input
	switch {
	case errors.Is(err, os.ErrNotExist):
		// keep defaults
	case err != nil:
		return nil, fmt.Errorf("read console config %q: %w", path, err)
	default:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse console config %q: %w", path, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces the M0 invariants.
func (c *Config) Validate() error {
	if c.HTTPPort < 1 || c.HTTPPort > 65535 {
		return fmt.Errorf("httpPort must be 1-65535, got %d", c.HTTPPort)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logLevel must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		return fmt.Errorf("logFormat must be json or text, got %q", c.LogFormat)
	}
	if c.MetricsPrefix == "" {
		return errors.New("metricsPrefix must not be empty")
	}
	if c.Auth.Mode != "anonymous" {
		return fmt.Errorf("M0 supports only auth.mode=anonymous, got %q", c.Auth.Mode)
	}
	if c.Auth.Anonymous.Role == "" {
		return errors.New("auth.anonymous.role must not be empty")
	}
	if err := validateBaseURL("controller.url", c.Controller.URL); err != nil {
		return err
	}
	if c.Controller.Timeout <= 0 {
		return fmt.Errorf("controller.timeout must be positive, got %v", c.Controller.Timeout)
	}
	if err := validateBaseURL("prometheus.url", c.Prometheus.URL); err != nil {
		return err
	}
	if c.Prometheus.QueryTimeout <= 0 || c.Prometheus.MaxRange <= 0 || c.Prometheus.MaxResponseBytes <= 0 {
		return fmt.Errorf("prometheus.queryTimeout/maxRange/maxResponseBytes must be positive, got %v/%v/%d",
			c.Prometheus.QueryTimeout, c.Prometheus.MaxRange, c.Prometheus.MaxResponseBytes)
	}
	// Controller.GRPCAddr is deliberately unvalidated beyond being a plain string:
	// like Controller.URL it is operator config, and an unreachable address just
	// fails to dial at runtime (surfaced by the ingester's reconnect-loop logs).
	if c.Valkey.Address != "" {
		if _, _, err := net.SplitHostPort(c.Valkey.Address); err != nil {
			return fmt.Errorf("valkey.address must be host:port, got %q: %w", c.Valkey.Address, err)
		}
	}
	if c.Valkey.DialTimeout <= 0 {
		return fmt.Errorf("valkey.dialTimeout must be positive, got %v", c.Valkey.DialTimeout)
	}
	return nil
}

// validateBaseURL accepts "" (feature disabled) or an absolute http(s) URL.
func validateBaseURL(field, raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be an absolute http(s) URL, got %q", field, raw)
	}
	if strings.HasSuffix(raw, "/") {
		return fmt.Errorf("%s must not end with a trailing slash, got %q", field, raw)
	}
	return nil
}
