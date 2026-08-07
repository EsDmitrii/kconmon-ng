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

	"github.com/EsDmitrii/kconmon-ng/internal/console/authz"
)

// OIDCCallbackPath is the fixed path the console's OIDC redirect handler is
// wired up at (a later task adds the route). auth.oidc.redirectURL must end
// with it — go-oidc's code-flow callback is a fixed, well-known endpoint, not
// operator-configurable, so pinning it here catches a misconfigured
// redirectURL at boot instead of as a runtime 404 from the IdP redirect.
const OIDCCallbackPath = "/api/v1/auth/oidc/callback"

// Config is the Console runtime configuration. auth.mode selects one of
// anonymous, local, header, or oidc (SECURITY.md §10.1).
type Config struct {
	HTTPPort      int        `yaml:"httpPort"`
	LogLevel      string     `yaml:"logLevel"`
	LogFormat     string     `yaml:"logFormat"`
	MetricsPrefix string     `yaml:"metricsPrefix"`
	Auth          AuthConfig `yaml:"auth"`

	Controller ControllerConfig `yaml:"controller"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Valkey     ValkeyConfig     `yaml:"valkey"`
	Database   DatabaseConfig   `yaml:"database"`
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

// DatabaseConfig configures the console's PostgreSQL store (internal/console/store).
// An empty resolved DSN disables persistence entirely: GET /api/v1/events answers
// 503, run history is in-memory only, and the whole M1/M2 surface is unchanged.
// This is the same "empty means off" convention Controller.URL, Prometheus.URL,
// Controller.GRPCAddr and Valkey.Address already use; Helm's
// console.database.mode=cnpg|external|disabled resolves INTO it.
type DatabaseConfig struct {
	DSN            string        `yaml:"dsn"`            // postgres://... ; MUST NOT carry a password (use DSNFile)
	DSNFile        string        `yaml:"dsnFile"`        // path to a file holding the full DSN; WINS over DSN when set
	MaxConns       int32         `yaml:"maxConns"`       // pgxpool max; default 10
	ConnectTimeout time.Duration `yaml:"connectTimeout"` // default 10s
	MigrateOnStart bool          `yaml:"migrateOnStart"` // default true (ADR-001: "run on start")
	RetentionDays  int           `yaml:"retentionDays"`  // default 90 (DATA.md §5.2); 0 disables pruning
}

// AuthConfig selects the authentication mode and configures every mode's
// settings. Only the block matching Mode is validated; the others are
// ignored (SECURITY.md §10.1: anonymous | local | header | oidc).
type AuthConfig struct {
	Mode        string          `yaml:"mode"` // anonymous | local | header | oidc
	Anonymous   AnonymousConfig `yaml:"anonymous"`
	Local       LocalConfig     `yaml:"local"`
	Header      HeaderConfig    `yaml:"header"`
	OIDC        OIDCConfig      `yaml:"oidc"`
	Session     SessionConfig   `yaml:"session"`
	DefaultRole string          `yaml:"defaultRole"` // role for an authenticated subject with no binding; empty = none (403)
}

// AnonymousConfig configures the fixed role used in anonymous mode.
type AnonymousConfig struct {
	Role string `yaml:"role"`
}

// LocalConfig configures auth.mode=local (users in PostgreSQL, argon2id;
// SECURITY.md §10.1). Requires a resolved database DSN — see Config.Validate.
type LocalConfig struct {
	BootstrapAdmin             string `yaml:"bootstrapAdmin"`             // username created on first start when the users table is empty
	BootstrapAdminPasswordFile string `yaml:"bootstrapAdminPasswordFile"` // file path; NEVER an inline password
}

// HeaderConfig configures auth.mode=header (trusted-proxy header auth;
// SECURITY.md §10.1: "explicit opt-in"). TrustedProxyCIDRs has no default —
// an empty list is a validation error, not "trust everyone".
type HeaderConfig struct {
	UserHeader        string   `yaml:"userHeader"`        // default X-Remote-User
	GroupsHeader      string   `yaml:"groupsHeader"`      // default X-Remote-Groups
	GroupsDelimiter   string   `yaml:"groupsDelimiter"`   // default ","
	TrustedProxyCIDRs []string `yaml:"trustedProxyCIDRs"` // REQUIRED non-empty in header mode
}

// OIDCConfig configures auth.mode=oidc (code flow + PKCE; SECURITY.md
// §10.1). Requires a resolved database DSN — see Config.Validate.
type OIDCConfig struct {
	Issuer           string   `yaml:"issuer"`
	ClientID         string   `yaml:"clientID"`
	ClientSecretFile string   `yaml:"clientSecretFile"`
	RedirectURL      string   `yaml:"redirectURL"`
	Scopes           []string `yaml:"scopes"`        // default [openid, profile, email, groups]
	UsernameClaim    string   `yaml:"usernameClaim"` // default preferred_username
	GroupsClaim      string   `yaml:"groupsClaim"`   // default groups
}

// SessionConfig configures the session cookie used by every non-anonymous
// mode. __Host-prefixed cookie names require Secure=true (browsers reject
// __Host- cookies without it).
type SessionConfig struct {
	TTL        time.Duration `yaml:"ttl"`        // default 12h
	CookieName string        `yaml:"cookieName"` // default __Host-kconmon_session
	Secure     bool          `yaml:"secure"`     // default true; false ONLY for local http:// development
}

func defaults() *Config {
	return &Config{
		HTTPPort:      8080,
		LogLevel:      "info",
		LogFormat:     "json",
		MetricsPrefix: "kconmon_ng",
		Auth: AuthConfig{
			Mode:      "anonymous",
			Anonymous: AnonymousConfig{Role: "viewer"},
			Header: HeaderConfig{
				UserHeader:      "X-Remote-User",
				GroupsHeader:    "X-Remote-Groups",
				GroupsDelimiter: ",",
			},
			OIDC: OIDCConfig{
				Scopes:        []string{"openid", "profile", "email", "groups"},
				UsernameClaim: "preferred_username",
				GroupsClaim:   "groups",
			},
			Session: SessionConfig{
				TTL:        12 * time.Hour,
				CookieName: "__Host-kconmon_session",
				Secure:     true,
			},
		},
		Controller: ControllerConfig{Timeout: 10 * time.Second},
		Prometheus: PrometheusConfig{QueryTimeout: 30 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 8 << 20},
		Valkey:     ValkeyConfig{DialTimeout: 5 * time.Second},
		Database:   DatabaseConfig{MaxConns: 10, ConnectTimeout: 10 * time.Second, MigrateOnStart: true, RetentionDays: 90},
	}
}

// ResolveDSN returns the effective DSN: DSNFile's trimmed contents when set,
// otherwise DSN. An empty return means persistence is disabled. Called once at
// boot by cmd/console — never per request.
func (d *DatabaseConfig) ResolveDSN() (string, error) {
	if d.DSNFile == "" {
		return d.DSN, nil
	}
	data, err := os.ReadFile(d.DSNFile) //nolint:gosec // path comes from operator config, not user input
	if err != nil {
		return "", fmt.Errorf("read database.dsnFile %q: %w", d.DSNFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// validate enforces database.* invariants: a well-formed postgres:// DSN with
// no embedded password, and positive pool/timeout settings.
func (d *DatabaseConfig) validate() error {
	if d.DSN != "" && d.DSNFile != "" {
		return errors.New("set either database.dsn or database.dsnFile, not both")
	}
	if d.DSN != "" {
		u, err := url.Parse(d.DSN)
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
			return fmt.Errorf("database.dsn must be a postgres:// URL, got %q", d.DSN)
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return errors.New("database.dsn must not embed a password (it would land in a ConfigMap): " +
				"use database.dsnFile with a mounted Secret")
		}
	}
	if d.MaxConns < 1 {
		return fmt.Errorf("database.maxConns must be positive, got %d", d.MaxConns)
	}
	if d.ConnectTimeout <= 0 {
		return fmt.Errorf("database.connectTimeout must be positive, got %v", d.ConnectTimeout)
	}
	if d.RetentionDays < 0 {
		return fmt.Errorf("database.retentionDays must be >= 0 (0 disables pruning), got %d", d.RetentionDays)
	}
	return nil
}

// validateAuth enforces the full auth.* matrix (SECURITY.md §10.1): a
// per-mode switch plus the cross-cutting rules that apply regardless of
// mode (session, defaultRole). local and oidc need a resolved database DSN
// (Decision 7: users/sessions live in PostgreSQL); anonymous and header do
// not, since roles come from config/headers, not a users table — this is
// why the DSN check lives here, at the Config level, rather than inside
// AuthConfig or DatabaseConfig alone.
func (c *Config) validateAuth() error {
	switch c.Auth.Mode {
	case "anonymous":
		if c.Auth.Anonymous.Role == "" {
			return errors.New("auth.anonymous.role must not be empty")
		}
	case "header":
		if err := c.Auth.Header.validate(); err != nil {
			return err
		}
	case "local":
		dsn, err := c.Database.ResolveDSN()
		if err != nil {
			return err
		}
		if dsn == "" {
			return errors.New("auth.mode=local requires database.dsn or database.dsnFile to be set " +
				"(local auth stores users in PostgreSQL — SECURITY.md §10.1)")
		}
	case "oidc":
		dsn, err := c.Database.ResolveDSN()
		if err != nil {
			return err
		}
		if dsn == "" {
			return errors.New("auth.mode=oidc requires database.dsn or database.dsnFile to be set " +
				"(oidc auth stores sessions in PostgreSQL — SECURITY.md §10.1)")
		}
		if err := c.Auth.OIDC.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("auth.mode must be one of anonymous|local|header|oidc, got %q", c.Auth.Mode)
	}

	if c.Auth.DefaultRole != "" && !authz.IsBuiltinRole(c.Auth.DefaultRole) {
		return fmt.Errorf("auth.defaultRole must be a known built-in role (viewer|operator|alert-editor|admin), got %q",
			c.Auth.DefaultRole)
	}

	if c.Auth.Session.TTL <= 0 {
		return fmt.Errorf("auth.session.ttl must be positive, got %v", c.Auth.Session.TTL)
	}
	if strings.HasPrefix(c.Auth.Session.CookieName, "__Host-") && !c.Auth.Session.Secure {
		return fmt.Errorf("auth.session.cookieName %q uses the __Host- prefix, which requires "+
			"auth.session.secure=true (browsers reject __Host- cookies without Secure)", c.Auth.Session.CookieName)
	}
	return nil
}

// validate enforces header mode's invariants (SECURITY.md §10.1: "behind
// trusted proxy; explicit opt-in"). An empty TrustedProxyCIDRs is rejected
// rather than defaulted, because a mode that trusts an unauthenticated
// request header from anywhere is an auth bypass, not a mode.
func (h *HeaderConfig) validate() error {
	if h.UserHeader == "" {
		return errors.New("auth.header.userHeader must not be empty")
	}
	if len(h.TrustedProxyCIDRs) == 0 {
		return errors.New("auth.header.trustedProxyCIDRs must be non-empty in header mode " +
			"(SECURITY.md §10.1: explicit opt-in)")
	}
	for _, cidr := range h.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("auth.header.trustedProxyCIDRs entry %q is not a valid CIDR: %w", cidr, err)
		}
	}
	return nil
}

// validate enforces oidc mode's invariants. The issuer must not carry a
// trailing slash: go-oidc discovery appends /.well-known/openid-configuration,
// and a doubled slash produces a confusing 404. redirectURL must end with
// OIDCCallbackPath, the fixed route the console's OIDC handler is wired up
// at.
func (o *OIDCConfig) validate() error {
	u, err := url.Parse(o.Issuer)
	if o.Issuer == "" || err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("auth.oidc.issuer must be an absolute https URL, got %q", o.Issuer)
	}
	if strings.HasSuffix(o.Issuer, "/") {
		return fmt.Errorf("auth.oidc.issuer must not end with a trailing slash "+
			"(discovery appends /.well-known/openid-configuration), got %q", o.Issuer)
	}
	if o.ClientID == "" {
		return errors.New("auth.oidc.clientID must not be empty")
	}
	if o.ClientSecretFile == "" {
		return errors.New("auth.oidc.clientSecretFile must not be empty")
	}
	ru, err := url.Parse(o.RedirectURL)
	if err != nil || (ru.Scheme != "http" && ru.Scheme != "https") || ru.Host == "" {
		return fmt.Errorf("auth.oidc.redirectURL must be an absolute http(s) URL, got %q", o.RedirectURL)
	}
	if !strings.HasSuffix(o.RedirectURL, OIDCCallbackPath) {
		return fmt.Errorf("auth.oidc.redirectURL must end with %q, got %q", OIDCCallbackPath, o.RedirectURL)
	}
	return nil
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

// Validate enforces every Config invariant, including the full auth.* matrix.
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
	if err := c.validateAuth(); err != nil {
		return err
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
	if err := c.Database.validate(); err != nil {
		return err
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
