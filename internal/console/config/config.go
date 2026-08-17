// Package config loads and validates the Console binary's runtime configuration.
// It is intentionally separate from internal/config (agent/controller config)
// but reuses that package's SetupLogger and version vars in cmd/console.
package config

import (
	"bytes"
	"encoding/base64"
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

// OIDCCallbackPath is the fixed path the console's OIDC redirect handler is wired up at (a later
// task adds the route).
const OIDCCallbackPath = "/api/v1/auth/oidc/callback"

// Config is the Console runtime configuration. auth.mode selects one of
// anonymous, local, header, or oidc (SECURITY.md §10.1).
type Config struct {
	HTTPPort int `yaml:"httpPort"`
	/* MetricsPort carries /metrics and the health endpoints on a listener of their OWN.

	   httpPort serves the whole Console API and the SPA. /metrics used to ride there too, which
	   meant a NetworkPolicy rule admitting a scraper admitted it to the API as well: the rule cannot
	   name a single pod (a scraper is whatever the operator runs), so on a real cluster the
	   monitoring namespace also holds Grafana, node-exporter, kube-state-metrics and the operator,
	   and every one of them landed inside whatever console.networkPolicy.ingressFrom was narrowed
	   to. Letting a scraper in and letting a caller reach the API are two different decisions, and
	   two ports is the only shape a NetworkPolicy can express. The agent and controller made the
	   same split -- see internal/metrics/listener.go, which this reuses. */
	MetricsPort   int        `yaml:"metricsPort"`
	LogLevel      string     `yaml:"logLevel"`
	LogFormat     string     `yaml:"logFormat"`
	MetricsPrefix string     `yaml:"metricsPrefix"`
	Auth          AuthConfig `yaml:"auth"`

	Controller ControllerConfig `yaml:"controller"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Redis      RedisConfig      `yaml:"redis"`
	Database   DatabaseConfig   `yaml:"database"`
	RateLimit  RateLimitConfig  `yaml:"rateLimit"`
	Scheduler  SchedulerConfig  `yaml:"scheduler"`
	MTR        MTRConfig        `yaml:"mtr"`

	KubernetesContext KubernetesContextConfig `yaml:"kubernetesContext"`
	Webhooks          WebhooksConfig          `yaml:"webhooks"`
	Alerting          AlertingConfig          `yaml:"alerting"`
}

// WebhooksConfig carries the ONE thing the outbound webhook dispatcher cannot derive for itself.
type WebhooksConfig struct {
	// EncryptionKey is 32 raw bytes, base64-encoded (standard encoding). It
	// belongs in a Secret, not a ConfigMap -- prefer EncryptionKeyFile.
	EncryptionKey string `yaml:"encryptionKey"`
	// EncryptionKeyFile is a path to a file holding the same base64 value.
	EncryptionKeyFile string `yaml:"encryptionKeyFile"`
	// AlertPollInterval is how often the alert-transition watcher reads Prometheus' current alert set
	// to detect fired/resolved edges; it lives under webhooks rather than under alerting, and the
	// distinction is not cosmetic.
	AlertPollInterval time.Duration `yaml:"alertPollInterval"`
}

// DefaultWebhookAlertPollInterval is the alert-state poll cadence.
const DefaultWebhookAlertPollInterval = 30 * time.Second

// webhookKeyLen is the AES-256-GCM key length. Pinned rather than "whatever
// aes.NewCipher accepts" (16/24/32) so a 16-byte key is a boot error an
// operator can fix, not a quietly weaker cipher nothing reports.
const webhookKeyLen = 32

// ResolveEncryptionKey returns the decoded webhook encryption key: the trimmed contents of
// EncryptionKeyFile when set.
func (w *WebhooksConfig) ResolveEncryptionKey() ([]byte, error) {
	raw := w.EncryptionKey
	source := "webhooks.encryptionKey"
	if w.EncryptionKeyFile != "" {
		data, err := os.ReadFile(w.EncryptionKeyFile) //nolint:gosec // path comes from operator config, not user input
		if err != nil {
			return nil, fmt.Errorf("read webhooks.encryptionKeyFile %q: %w", w.EncryptionKeyFile, err)
		}
		raw, source = string(data), "webhooks.encryptionKeyFile"
	}
	return decodeWebhookKey(source, raw)
}

// decodeWebhookKey turns a base64 string into the 32-byte key, or reports why it cannot.
func decodeWebhookKey(field, raw string) ([]byte, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64 (standard encoding) of %d random bytes: %w",
			field, webhookKeyLen, err)
	}
	if len(key) != webhookKeyLen {
		return nil, fmt.Errorf("%s decodes to %d bytes, must be exactly %d "+
			"(generate one with: openssl rand -base64 %d)", field, len(key), webhookKeyLen, webhookKeyLen)
	}
	return key, nil
}

// validate enforces webhooks.* invariants that need no I/O: the two ways of naming the key are
// mutually exclusive.
func (w *WebhooksConfig) validate(alertingEnabled bool) error {
	if w.EncryptionKey != "" && w.EncryptionKeyFile != "" {
		return errors.New("set either webhooks.encryptionKey or webhooks.encryptionKeyFile, not both")
	}
	if _, err := decodeWebhookKey("webhooks.encryptionKey", w.EncryptionKey); err != nil {
		return err
	}
	// "Webhooks enabled" is "a key is named", which is exactly the condition cmd/console builds the
	// dispatcher on.
	keyed := w.EncryptionKey != "" || w.EncryptionKeyFile != ""
	if keyed && alertingEnabled && w.AlertPollInterval <= 0 {
		return fmt.Errorf("webhooks.alertPollInterval must be positive when a webhook encryption key "+
			"is configured and alerting.enabled is true (it is the alert-transition poll cadence, and "+
			"the granularity of every alert.resolved delivery), got %v", w.AlertPollInterval)
	}
	return nil
}

// KubernetesContextConfig configures the Kubernetes event reader: the only part of the Console that
// talks to the apiserver.
type KubernetesContextConfig struct {
	// Enabled is the master gate.
	Enabled bool `yaml:"enabled"`
	// Namespace is the ONE namespace whose pod events are captured.
	Namespace string `yaml:"namespace"`
	// ResyncInterval forces a periodic relist even while the watch is healthy; ten minutes because the
	// apiserver keeps events for an hour by default.
	ResyncInterval time.Duration `yaml:"resyncInterval"`
}

// podNamespaceEnv is the downward-API variable the chart sets on the console pod; it is read ONLY
// as the fallback for an empty kubernetesContext.namespace.
const podNamespaceEnv = "POD_NAMESPACE"

// ResolveNamespace returns the effective namespace for pod-event capture: Namespace when set;
// called once at startup (kubectx.New and cmd/console's log line) -- never per event.
func (k *KubernetesContextConfig) ResolveNamespace() string {
	return resolveNamespace(k.Namespace)
}

// resolveNamespace is the shared three-step fallback both namespace-bearing blocks use
// (kubernetesContext and alerting); it is one function rather than two copies because the two
// blocks MUST agree.
func resolveNamespace(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if ns := strings.TrimSpace(os.Getenv(podNamespaceEnv)); ns != "" {
		return ns
	}
	return "default"
}

// validate enforces kubernetesContext.* invariants; like SchedulerConfig's, the interval is only
// checked when the feature is enabled.
func (k *KubernetesContextConfig) validate() error {
	if k.Enabled && k.ResyncInterval <= 0 {
		return fmt.Errorf("kubernetesContext.resyncInterval must be positive when kubernetesContext.enabled is true, got %v",
			k.ResyncInterval)
	}
	return nil
}

// AlertingConfig configures the PrometheusRule reconciler; the extra one: the CRD may not exist.
type AlertingConfig struct {
	// Enabled is the master gate.
	Enabled bool `yaml:"enabled"`
	// Namespace is the namespace the bundle object is applied into.
	Namespace string `yaml:"namespace"`
	// SyncInterval is the reconcile cadence; sixty seconds because a reconcile is a single SSA of a
	// few kilobytes and the loop exists to CONVERGE.
	SyncInterval time.Duration `yaml:"syncInterval"`
	// BundleName is the name of the single PrometheusRule object the console owns; it is configurable
	// only so two consoles can share a namespace without fighting over the same object.
	BundleName string `yaml:"bundleName"`
}

// DefaultAlertingBundleName is the name of the console-owned PrometheusRule
// object. Exported so the chart's RBAC docs and cmd/console's log line quote
// one spelling.
const DefaultAlertingBundleName = "kconmon-ng-console-rules"

// DefaultAlertingSyncInterval is the reconcile cadence. See SyncInterval.
const DefaultAlertingSyncInterval = 60 * time.Second

// ResolveNamespace returns the namespace the bundle is applied into:
// Namespace when set, else $POD_NAMESPACE, else "default". Called once at
// startup by cmd/console -- never per reconcile.
func (a *AlertingConfig) ResolveNamespace() string {
	return resolveNamespace(a.Namespace)
}

// validate enforces alerting.* invariants, and like KubernetesContextConfig's it only checks them
// when the feature is ON.
func (a *AlertingConfig) validate() error {
	if !a.Enabled {
		return nil
	}
	if a.SyncInterval <= 0 {
		return fmt.Errorf("alerting.syncInterval must be positive when alerting.enabled is true, got %v",
			a.SyncInterval)
	}
	if a.BundleName == "" {
		return errors.New("alerting.bundleName must not be empty when alerting.enabled is true")
	}
	// The name becomes a Kubernetes object name.
	if err := validateDNS1123Subdomain("alerting.bundleName", a.BundleName); err != nil {
		return err
	}
	// The namespace is checked only when it is EXPLICIT: the resolved value comes from the downward
	// API.
	if a.Namespace != "" {
		if err := validateDNS1123Subdomain("alerting.namespace", a.Namespace); err != nil {
			return err
		}
	}
	return nil
}

// dns1123SubdomainMaxLen is the Kubernetes object-name bound (RFC 1123).
const dns1123SubdomainMaxLen = 253

// validateDNS1123Subdomain applies the Kubernetes object-name grammar (lowercase alphanumerics, '-'
// and '.', starting and ending alphanumeric); hand-rolled rather than imported from
// k8s.io/apimachinery/pkg/util/validation so internal/console/config keeps its
// zero-Kubernetes-import posture.
func validateDNS1123Subdomain(field, value string) error {
	bad := func() error {
		return fmt.Errorf("%s must be a valid Kubernetes object name "+
			"(lowercase alphanumerics, '-' or '.', starting and ending alphanumeric, at most %d characters), got %q",
			field, dns1123SubdomainMaxLen, value)
	}
	if len(value) > dns1123SubdomainMaxLen {
		return bad()
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '.') && i > 0 && i < len(value)-1:
		default:
			return bad()
		}
	}
	return nil
}

// MTRConfig groups everything the Console does with MTR path history beyond simply storing it;
// today that is exactly one thing — hop enrichment.
type MTRConfig struct {
	Enrichment EnrichmentConfig `yaml:"enrichment"`
}

// EnrichmentConfig configures the hop-address enrichment resolver (internal/console/enrich);
// enabled defaults to FALSE, for a stronger reason than SchedulerConfig's.
type EnrichmentConfig struct {
	// Enabled is the master gate. With it off, nothing below is read and the
	// snapshot-detail handler keeps serving the cache-only view Task 4 built.
	Enabled bool `yaml:"enabled"`
	// RDNS and GeoIP gate INDEPENDENTLY: an air-gapped cluster with mounted
	// mmdb files and no reachable resolver runs geoip-only, and a cluster with
	// internal DNS and no MaxMind licence runs rdns-only.
	RDNS  RDNSConfig  `yaml:"rdns"`
	GeoIP GeoIPConfig `yaml:"geoip"`
	// TTL is a cache row's lifetime.
	TTL time.Duration `yaml:"ttl"`
}

// RDNSConfig gates the reverse-DNS source and bounds it.
type RDNSConfig struct {
	Enabled bool `yaml:"enabled"`
	// TimeoutMs bounds ONE lookup; it is milliseconds rather than a time.Duration because the useful
	// range is 100-1000ms and an operator who writes `timeoutMs: 500` cannot accidentally mean 500ns.
	TimeoutMs int `yaml:"timeoutMs"`
}

// GeoIPConfig points at the two mmdb files; an UNREADABLE path is not a boot failure either.
type GeoIPConfig struct {
	ASNPath  string `yaml:"asnPath"`  // e.g. /geoip/GeoLite2-ASN.mmdb; empty = ASN/provider lookups off
	CityPath string `yaml:"cityPath"` // e.g. /geoip/GeoLite2-City.mmdb; empty = geo lookups off
	// ReloadInterval re-stats the two paths and reopens whichever changed, so a geoipupdate sidecar
	// refreshing the files is picked up without restarting the console. Zero (the default) never reloads,
	// which is the right answer for an operator who mounts their own read-only copies.
	ReloadInterval time.Duration `yaml:"reloadInterval"`
}

// validate enforces mtr.enrichment.* invariants; everything is checked ONLY when the master gate.
func (e *EnrichmentConfig) validate() error {
	if !e.Enabled {
		return nil
	}
	if !e.RDNS.Enabled && e.GeoIP.ASNPath == "" && e.GeoIP.CityPath == "" {
		return errors.New("mtr.enrichment.enabled is true but every source is off: set mtr.enrichment.rdns.enabled, " +
			"mtr.enrichment.geoip.asnPath or mtr.enrichment.geoip.cityPath (an enabled resolver with no source " +
			"would cache empty rows for mtr.enrichment.ttl)")
	}
	if e.TTL <= 0 {
		return fmt.Errorf("mtr.enrichment.ttl must be positive when mtr.enrichment.enabled is true, got %v", e.TTL)
	}
	if e.RDNS.Enabled && e.RDNS.TimeoutMs <= 0 {
		return fmt.Errorf("mtr.enrichment.rdns.timeoutMs must be positive when mtr.enrichment.rdns.enabled is true, got %d",
			e.RDNS.TimeoutMs)
	}
	return nil
}

// validate delegates to the one block mtr: currently carries.
func (m *MTRConfig) validate() error {
	return m.Enrichment.validate()
}

// SchedulerConfig configures the schedule loop (internal/console/scheduler); schedules can already
// be created and stored without anything acting on them.
type SchedulerConfig struct {
	// Enabled turns the loop on; it also requires a resolved database DSN.
	Enabled bool `yaml:"enabled"`
	// Short by design (seconds): the advisory lock is taken and released per tick.
	TickInterval time.Duration `yaml:"tickInterval"`
}

// validate enforces scheduler.* invariants; the tick interval is only checked when the loop is
// enabled: a disabled loop never reads.
func (s *SchedulerConfig) validate() error {
	if s.Enabled && s.TickInterval <= 0 {
		return fmt.Errorf("scheduler.tickInterval must be positive when scheduler.enabled is true, got %v", s.TickInterval)
	}
	return nil
}

// RateLimitConfig configures the console's fixed-window request limits
// (internal/console/httpapi/ratelimit.go); that is weaker than configured.
type RateLimitConfig struct {
	// RunsPerMinute caps POST /api/v1/runs per SUBJECT per minute (default
	// 10): a diagnostics run fans out to up to 400 agent pairs, so an
	// unbounded caller is a controller-load amplifier.
	RunsPerMinute int `yaml:"runsPerMinute"`
	// LoginPerMinute caps POST /api/v1/auth/login per USERNAME per minute (default 5). The per-SOURCE
	// IP budget is counted independently and is loginIPBurstFactor times this, because behind an
	// Ingress one address is the whole cluster -- see handleAuthLogin for the reasoning.
	LoginPerMinute int `yaml:"loginPerMinute"`
	/* PromQLPerMinute caps the PromQL proxy per SUBJECT per minute (default 60).
	   /api/v1/promql/query[_range] forwards arbitrary PromQL to the cluster's Prometheus, and
	   promql:query belongs to the VIEWER role — which, on the chart's demo default of
	   auth.mode=anonymous, is everybody who can reach the console. One range query over a wide window
	   is many series and much work upstream; nothing bounded how many of them a caller could ask
	   for. Monitoring is the thing this console exists to watch, and it must not be the thing it
	   knocks over. */
	PromQLPerMinute int `yaml:"promqlPerMinute"`
}

// validate enforces rateLimit.* invariants; zero is legal (that limit is off).
func (rl *RateLimitConfig) validate() error {
	if rl.RunsPerMinute < 0 {
		return fmt.Errorf("rateLimit.runsPerMinute must be >= 0 (0 disables the limit), got %d", rl.RunsPerMinute)
	}
	if rl.LoginPerMinute < 0 {
		return fmt.Errorf("rateLimit.loginPerMinute must be >= 0 (0 disables the limit), got %d", rl.LoginPerMinute)
	}
	if rl.PromQLPerMinute < 0 {
		return fmt.Errorf("rateLimit.promqlPerMinute must be >= 0 (0 disables the limit), got %d", rl.PromQLPerMinute)
	}
	return nil
}

// ControllerConfig configures the console's HTTP client for the controller
// topology API. An empty URL disables the feature (endpoints answer 503).
type ControllerConfig struct {
	URL      string        `yaml:"url"`      // e.g. http://kconmon-ng-controller:8080; empty = feature disabled (503)
	Timeout  time.Duration `yaml:"timeout"`  // per-request; default 10s
	GRPCAddr string        `yaml:"grpcAddr"` // e.g. kconmon-ng-controller:9090; empty = realtime disabled (M1 polling only)
}

/*
 * RedisConfig configures the console's Redis-compatible bus (internal/console/cache): sessions,
 * rate-limit counters and cross-replica pub/sub.
 *
 * ONE DSN, exactly like the database, and for the same reason: an address plus a password file plus
 * a TLS flag is three settings that have to agree, and the server already has a URL form that says
 * all of it at once. `redis://`, `rediss://` (TLS), `valkey://`, `valkeys://` and `unix://` are all
 * understood, with the password, the username and the database number in their usual places —
 * whatever server is on the other end, it is described the way its own documentation describes it.
 *
 * Empty disables the bus: the console falls back to an in-process one, which is single-replica only.
 */
type RedisConfig struct {
	// DSN is here for local development ONLY; the chart never templates a credential into config.
	DSN string `yaml:"dsn"`
	// DSNFile holds the DSN in a mounted Secret, the same posture as database.dsnFile.
	DSNFile     string        `yaml:"dsnFile"` // WINS over DSN when set
	DialTimeout time.Duration `yaml:"dialTimeout"`
}

// ResolveDSN returns the effective Redis DSN: DSNFile's trimmed contents when set, otherwise DSN.
// An empty return means the in-process bus. Called once at boot, never per request.
func (v *RedisConfig) ResolveDSN() (string, error) {
	if v.DSNFile == "" {
		return v.DSN, nil
	}
	data, err := os.ReadFile(v.DSNFile) //nolint:gosec // path comes from operator config, not user input
	if err != nil {
		return "", fmt.Errorf("read redis.dsnFile %q: %w", v.DSNFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// validate mirrors database.dsn/dsnFile: naming both is a mistake worth reporting, not a precedence
// puzzle for the reader.
func (v *RedisConfig) validate() error {
	if v.DSN != "" && v.DSNFile != "" {
		return errors.New("set either redis.dsn or redis.dsnFile, not both")
	}
	return nil
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
	Scopes           []string `yaml:"scopes"` // default [openid, profile, email, groups]
	// UsernameClaim picks the DISPLAY name only — the label in the header menu. NOT the audit log:
	// that records the identity (authz.Subject.ID), which is "oidc:"+sub.
	// It does NOT decide identity: RBAC binds to "oidc:"+sub, because sub is the one claim OIDC
	// Core §5.7 guarantees stable and unique, and preferred_username/email are explicitly forbidden
	// as identifiers (authn/identity.go). Changing this renames a person; it never re-points their
	// roles.
	UsernameClaim string `yaml:"usernameClaim"` // default preferred_username
	GroupsClaim   string `yaml:"groupsClaim"`   // default groups
}

// SessionConfig configures the session cookie used by every non-anonymous
// mode. __Host-prefixed cookie names require Secure=true (browsers reject
// __Host- cookies without it).
type SessionConfig struct {
	// TTL is the ABSOLUTE lifetime, counted from login and never extended.
	TTL time.Duration `yaml:"ttl"` // default 12h
	// IdleTimeout is how long a session may go unused before it is refused and purged; it slides
	// forward on every request but never past TTL. Zero disables it, leaving TTL the only bound.
	IdleTimeout time.Duration `yaml:"idleTimeout"` // default 1h
	CookieName  string        `yaml:"cookieName"`  // default __Host-kconmon_session
	Secure      bool          `yaml:"secure"`      // default true; false ONLY for local http:// development
}

func defaults() *Config {
	return &Config{
		HTTPPort:      8080,
		MetricsPort:   9091,
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
				TTL:         12 * time.Hour,
				IdleTimeout: time.Hour,
				CookieName:  "__Host-kconmon_session",
				Secure:      true,
			},
		},
		Controller: ControllerConfig{Timeout: 10 * time.Second},
		Prometheus: PrometheusConfig{QueryTimeout: 30 * time.Second, MaxRange: 24 * time.Hour, MaxResponseBytes: 8 << 20},
		Redis:      RedisConfig{DialTimeout: 5 * time.Second},
		Database:   DatabaseConfig{MaxConns: 10, ConnectTimeout: 10 * time.Second, MigrateOnStart: true, RetentionDays: 90},
		RateLimit:  RateLimitConfig{RunsPerMinute: 10, LoginPerMinute: 5, PromQLPerMinute: 60},
		// enabled stays false (see SchedulerConfig); the interval is still
		// defaulted so switching the loop on is a one-line change.
		Scheduler: SchedulerConfig{TickInterval: 5 * time.Second},
		// Same shape as Scheduler above: every gate stays off, every budget is
		// pre-defaulted, so turning a source on is a one-line change and never
		// a two-line one that fails validation on the first try.
		MTR: MTRConfig{Enrichment: EnrichmentConfig{
			RDNS: RDNSConfig{TimeoutMs: 500},
			TTL:  24 * time.Hour,
		}},
		// Same shape again: the gate stays off, the namespace stays empty (=
		// resolve from POD_NAMESPACE), and the cadence is pre-defaulted.
		KubernetesContext: KubernetesContextConfig{ResyncInterval: 10 * time.Minute},
		// The key stays empty (= the documented keyless state) and only the cadence is pre-defaulted.
		Webhooks: WebhooksConfig{AlertPollInterval: DefaultWebhookAlertPollInterval},
		// Same shape once more: the gate stays off, the namespace stays empty (= resolve from
		// POD_NAMESPACE).
		Alerting: AlertingConfig{
			SyncInterval: DefaultAlertingSyncInterval,
			BundleName:   DefaultAlertingBundleName,
		},
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

// validateAuth enforces the full auth.* matrix (SECURITY.md §10.1).
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
	// An idle window longer than the absolute lifetime can never fire; refused rather than silently
	// ignored, because it reads like idle expiry is configured when it is not.
	if c.Auth.Session.IdleTimeout < 0 {
		return fmt.Errorf("auth.session.idleTimeout must not be negative, got %v (0 disables it)",
			c.Auth.Session.IdleTimeout)
	}
	if c.Auth.Session.IdleTimeout > c.Auth.Session.TTL {
		return fmt.Errorf("auth.session.idleTimeout (%v) must not exceed auth.session.ttl (%v): "+
			"a session cannot go idle for longer than it is allowed to live",
			c.Auth.Session.IdleTimeout, c.Auth.Session.TTL)
	}
	if strings.HasPrefix(c.Auth.Session.CookieName, "__Host-") && !c.Auth.Session.Secure {
		return fmt.Errorf("auth.session.cookieName %q uses the __Host- prefix, which requires "+
			"auth.session.secure=true (browsers reject __Host- cookies without Secure)", c.Auth.Session.CookieName)
	}
	return nil
}

// validate enforces header mode's invariants.
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

// validate enforces oidc mode's invariants; the issuer must not carry a trailing slash.
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
	if c.MetricsPort < 1 || c.MetricsPort > 65535 {
		return fmt.Errorf("metricsPort must be 1-65535, got %d", c.MetricsPort)
	}
	// The whole point is that they are DIFFERENT listeners; sharing a port would silently restore
	// the coupling this split exists to break.
	if c.MetricsPort == c.HTTPPort {
		return fmt.Errorf("metricsPort must differ from httpPort, both are %d", c.MetricsPort)
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
	if c.Redis.DialTimeout <= 0 {
		return fmt.Errorf("redis.dialTimeout must be positive, got %v", c.Redis.DialTimeout)
	}
	if err := c.Redis.validate(); err != nil {
		return err
	}
	if err := c.Database.validate(); err != nil {
		return err
	}
	if err := c.RateLimit.validate(); err != nil {
		return err
	}
	if err := c.Scheduler.validate(); err != nil {
		return err
	}
	if err := c.MTR.validate(); err != nil {
		return err
	}
	if err := c.KubernetesContext.validate(); err != nil {
		return err
	}
	if err := c.Webhooks.validate(c.Alerting.Enabled); err != nil {
		return err
	}
	if err := c.Alerting.validate(); err != nil {
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
