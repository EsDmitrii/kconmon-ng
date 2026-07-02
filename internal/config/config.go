package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

type Config struct {
	Mode               string              `yaml:"mode"`
	MetricsPrefix      string              `yaml:"metricsPrefix"`
	HTTPPort           int                 `yaml:"httpPort"`
	GRPCPort           int                 `yaml:"grpcPort"`
	LogLevel           string              `yaml:"logLevel"`
	LogFormat          string              `yaml:"logFormat"`
	FailureDomainLabel string              `yaml:"failureDomainLabel"`
	ControllerAddress  string              `yaml:"controllerAddress"`
	Checkers           CheckersConfig      `yaml:"checkers"`
	Controller         ControllerConfig    `yaml:"controller"`
	Observability      ObservabilityConfig `yaml:"observability"`
}

type CheckersConfig struct {
	TCP  TCPCheckerConfig  `yaml:"tcp"`
	UDP  UDPCheckerConfig  `yaml:"udp"`
	ICMP ICMPCheckerConfig `yaml:"icmp"`
	DNS  DNSCheckerConfig  `yaml:"dns"`
	HTTP HTTPCheckerConfig `yaml:"http"`
	MTR  MTRCheckerConfig  `yaml:"mtr"`
}

type TCPCheckerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

type UDPCheckerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Packets  int           `yaml:"packets"`
}

type ICMPCheckerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
}

type DNSCheckerConfig struct {
	Enabled   bool          `yaml:"enabled"`
	Interval  time.Duration `yaml:"interval"`
	Timeout   time.Duration `yaml:"timeout"`
	Hosts     []string      `yaml:"hosts"`
	Resolvers []string      `yaml:"resolvers,omitempty"`
}

type HTTPCheckerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Targets  []HTTPTarget  `yaml:"targets"`
}

type HTTPTarget struct {
	URL          string `yaml:"url"`
	Method       string `yaml:"method,omitempty"`
	ExpectStatus int    `yaml:"expectStatus,omitempty"`
	BodyPattern  string `yaml:"bodyPattern,omitempty"`
}

type MTRCheckerConfig struct {
	Cooldown time.Duration `yaml:"cooldown"`
	MaxHops  int           `yaml:"maxHops"`
}

type ControllerConfig struct {
	LeaderElection bool          `yaml:"leaderElection"`
	AgentTTL       time.Duration `yaml:"agentTtl"`
}

type ObservabilityConfig struct {
	OTel OTelConfig `yaml:"otel"`
}

type OTelConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

type OnChangeFunc func(*Config)

type Loader struct {
	mu       sync.RWMutex
	cfg      *Config
	filePath string
	onChange []OnChangeFunc
	watcher  *fsnotify.Watcher
}

func NewLoader(filePath string) *Loader {
	return &Loader{
		cfg:      DefaultConfig(),
		filePath: filePath,
	}
}

func (l *Loader) Load() error {
	cfg := DefaultConfig()

	if l.filePath != "" {
		if err := l.loadFromFile(cfg); err != nil {
			return fmt.Errorf("loading config file: %w", err)
		}
	}

	l.loadFromEnv(cfg)

	if err := l.validate(cfg); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}

	l.mu.Lock()
	l.cfg = cfg
	l.mu.Unlock()

	return nil
}

func (l *Loader) Get() *Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c := *l.cfg
	return &c
}

func (l *Loader) OnChange(fn OnChangeFunc) {
	l.onChange = append(l.onChange, fn)
}

func (l *Loader) WatchForChanges() error {
	if l.filePath == "" {
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	l.watcher = watcher

	if err := watcher.Add(l.filePath); err != nil {
		return fmt.Errorf("watching file %s: %w", l.filePath, err)
	}

	go l.watchLoop()
	return nil
}

func (l *Loader) Close() error {
	if l.watcher != nil {
		return l.watcher.Close()
	}
	return nil
}

func (l *Loader) watchLoop() {
	for {
		select {
		case event, ok := <-l.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				slog.Info("config file changed, reloading", "file", l.filePath)
				if err := l.Load(); err != nil {
					slog.Error("failed to reload config", "error", err)
					continue
				}
				cfg := l.Get()
				for _, fn := range l.onChange {
					fn(cfg)
				}
			}
		case err, ok := <-l.watcher.Errors:
			if !ok {
				return
			}
			slog.Error("config watcher error", "error", err)
		}
	}
}

func (l *Loader) loadFromFile(cfg *Config) error {
	data, err := os.ReadFile(l.filePath)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		// An empty file (or only comments) yields EOF from Decode; treat it as
		// an empty config so all defaults apply rather than a load failure.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func (l *Loader) loadFromEnv(cfg *Config) {
	if v := os.Getenv("KCONMON_NG_MODE"); v != "" {
		cfg.Mode = v
	}
	if v := os.Getenv("KCONMON_NG_METRICS_PREFIX"); v != "" {
		cfg.MetricsPrefix = v
	}
	if v := os.Getenv("KCONMON_NG_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("KCONMON_NG_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	if v := os.Getenv("KCONMON_NG_CONTROLLER_ADDRESS"); v != "" {
		cfg.ControllerAddress = v
	}
	if v := os.Getenv("KCONMON_NG_FAILURE_DOMAIN_LABEL"); v != "" {
		cfg.FailureDomainLabel = v
	}
}

func (l *Loader) validate(cfg *Config) error {
	if cfg.HTTPPort < 1 || cfg.HTTPPort > 65535 {
		return fmt.Errorf("httpPort must be between 1 and 65535, got %d", cfg.HTTPPort)
	}
	if cfg.GRPCPort < 1 || cfg.GRPCPort > 65535 {
		return fmt.Errorf("grpcPort must be between 1 and 65535, got %d", cfg.GRPCPort)
	}
	if cfg.HTTPPort == cfg.GRPCPort {
		return fmt.Errorf("httpPort and grpcPort must be different")
	}

	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[strings.ToLower(cfg.LogLevel)] {
		return fmt.Errorf("logLevel must be one of debug, info, warn, error; got %q", cfg.LogLevel)
	}

	validFormats := map[string]bool{"json": true, "text": true}
	if !validFormats[strings.ToLower(cfg.LogFormat)] {
		return fmt.Errorf("logFormat must be one of json, text; got %q", cfg.LogFormat)
	}

	if cfg.Checkers.UDP.Packets < 1 {
		return fmt.Errorf("udp.packets must be >= 1, got %d", cfg.Checkers.UDP.Packets)
	}
	if cfg.Checkers.MTR.MaxHops < 1 || cfg.Checkers.MTR.MaxHops > 64 {
		return fmt.Errorf("mtr.maxHops must be between 1 and 64, got %d", cfg.Checkers.MTR.MaxHops)
	}

	if cfg.Checkers.TCP.Enabled {
		if err := validateTiming("tcp", cfg.Checkers.TCP.Interval, cfg.Checkers.TCP.Timeout); err != nil {
			return err
		}
	}
	if cfg.Checkers.UDP.Enabled {
		if err := validateTiming("udp", cfg.Checkers.UDP.Interval, cfg.Checkers.UDP.Timeout); err != nil {
			return err
		}
	}
	if cfg.Checkers.ICMP.Enabled {
		if err := validateTiming("icmp", cfg.Checkers.ICMP.Interval, cfg.Checkers.ICMP.Timeout); err != nil {
			return err
		}
	}
	if cfg.Checkers.DNS.Enabled {
		if err := validateTiming("dns", cfg.Checkers.DNS.Interval, cfg.Checkers.DNS.Timeout); err != nil {
			return err
		}
		if err := validateDNS(cfg.Checkers.DNS); err != nil {
			return err
		}
	}
	if cfg.Checkers.HTTP.Enabled {
		if err := validateTiming("http", cfg.Checkers.HTTP.Interval, cfg.Checkers.HTTP.Timeout); err != nil {
			return err
		}
		if err := validateHTTP(cfg.Checkers.HTTP); err != nil {
			return err
		}
	}

	return nil
}

// validateTiming enforces positive interval/timeout for an enabled checker.
// Timeout >= Interval is intentionally only a warning: probes may be tuned
// tight and the operator may know what they are doing.
func validateTiming(name string, interval, timeout time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("%s.interval must be > 0 when the checker is enabled, got %v", name, interval)
	}
	if timeout <= 0 {
		return fmt.Errorf("%s.timeout must be > 0 when the checker is enabled, got %v", name, timeout)
	}
	if timeout >= interval {
		slog.Warn("checker timeout >= interval; probes may overlap or starve",
			"checker", name, "timeout", timeout, "interval", interval)
	}
	return nil
}

func validateDNS(dns DNSCheckerConfig) error {
	if len(dns.Hosts) == 0 {
		return fmt.Errorf("dns.hosts must not be empty when the dns checker is enabled")
	}
	for i, h := range dns.Hosts {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("dns.hosts[%d] must not be empty", i)
		}
	}
	for i, r := range dns.Resolvers {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("dns.resolvers[%d] must not be empty", i)
		}
		// Accept either "host" or "host:port".
		if strings.Contains(r, ":") {
			host, port, err := net.SplitHostPort(r)
			if err != nil {
				return fmt.Errorf("dns.resolvers[%d] %q is not a valid host or host:port: %w", i, r, err)
			}
			if host == "" {
				return fmt.Errorf("dns.resolvers[%d] %q has an empty host", i, r)
			}
			if _, err := strconv.Atoi(port); err != nil {
				return fmt.Errorf("dns.resolvers[%d] %q has an invalid port %q", i, r, port)
			}
		}
	}
	return nil
}

func validateHTTP(h HTTPCheckerConfig) error {
	if len(h.Targets) == 0 {
		return fmt.Errorf("http.targets must not be empty when the http checker is enabled")
	}
	for i, t := range h.Targets {
		if strings.TrimSpace(t.URL) == "" {
			return fmt.Errorf("http.targets[%d].url must not be empty", i)
		}
		u, err := url.Parse(t.URL)
		if err != nil {
			return fmt.Errorf("http.targets[%d].url %q is not a valid URL: %w", i, t.URL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("http.targets[%d].url %q must use scheme http or https, got %q", i, t.URL, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("http.targets[%d].url %q must include a host", i, t.URL)
		}
	}
	return nil
}
