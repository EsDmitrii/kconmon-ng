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
	RateLimit  RateLimitConfig  `yaml:"rateLimit"`
	Scheduler  SchedulerConfig  `yaml:"scheduler"`
	MTR        MTRConfig        `yaml:"mtr"`

	KubernetesContext KubernetesContextConfig `yaml:"kubernetesContext"`
	Webhooks          WebhooksConfig          `yaml:"webhooks"`
}

// WebhooksConfig carries the ONE thing the outbound webhook dispatcher
// (internal/console/webhooks, M6 Task 5) cannot derive for itself: the AES-GCM
// key that encrypts each endpoint's HMAC signing secret at rest (M6 Decision
// 4). Everything else about a webhook -- its URL, its event filter, its
// enabled flag -- is a database row an admin typed; only the key is a
// deployment secret, so only the key is config.
//
// The whole block is OPTIONAL, and that is deliberate. A console that never
// declares a webhook must not fail to start over a cipher it will never use,
// so an empty block leaves the feature keyless: every read/update/delete route
// keeps working and the two operations that actually need the cipher --
// creating an endpoint and testing one -- answer 503 naming this value
// (httpapi's webhookKeyUnavailableDetail). A key that IS configured but
// unusable is the opposite case: that is a broken Secret mount, not a
// deliberate omission, and cmd/console refuses to start on it the same way it
// refuses to start on an unreadable database.dsnFile.
type WebhooksConfig struct {
	// EncryptionKey is 32 raw bytes, base64-encoded (standard encoding). It
	// belongs in a Secret, not a ConfigMap -- prefer EncryptionKeyFile.
	EncryptionKey string `yaml:"encryptionKey"`
	// EncryptionKeyFile is a path to a file holding the same base64 value;
	// it WINS over EncryptionKey when both somehow reach this struct, and
	// setting both in a config file is refused outright -- database.dsnFile's
	// rule, for database.dsnFile's reason.
	EncryptionKeyFile string `yaml:"encryptionKeyFile"`
}

// webhookKeyLen is the AES-256-GCM key length. Pinned rather than "whatever
// aes.NewCipher accepts" (16/24/32) so a 16-byte key is a boot error an
// operator can fix, not a quietly weaker cipher nothing reports.
const webhookKeyLen = 32

// ResolveEncryptionKey returns the decoded webhook encryption key: the trimmed
// contents of EncryptionKeyFile when set, otherwise EncryptionKey. A nil
// return with a nil error means NO key is configured, which is the documented
// keyless state, not a failure. Called once at boot by cmd/console -- never per
// delivery.
//
// Mirrors DatabaseConfig.ResolveDSN's shape exactly: the mounted file wins, and
// resolution is an explicit method rather than something Load bakes in, so a
// test can pin either side.
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

// decodeWebhookKey turns a base64 string into the 32-byte key, or reports why
// it cannot. The error NEVER echoes the value: a decode failure is about the
// shape of a secret, and repeating the secret to say it is malformed would put
// it in a log line.
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

// validate enforces webhooks.* invariants that need no I/O: the two ways of
// naming the key are mutually exclusive, and an INLINE key must already be
// well-formed. The FILE is not read here -- validation is pure by convention in
// this package, and cmd/console's ResolveEncryptionKey call is where a bad
// mount is caught, on the same footing as an unreadable database.dsnFile.
func (w *WebhooksConfig) validate() error {
	if w.EncryptionKey != "" && w.EncryptionKeyFile != "" {
		return errors.New("set either webhooks.encryptionKey or webhooks.encryptionKeyFile, not both")
	}
	if _, err := decodeWebhookKey("webhooks.encryptionKey", w.EncryptionKey); err != nil {
		return err
	}
	return nil
}

// KubernetesContextConfig configures the Kubernetes event reader
// (internal/console/kubectx, M6 Decision 3): the only part of the Console that
// talks to the apiserver. It captures core/v1 Events for nodes in the fleet
// topology and for pods in one namespace, so the Investigate timeline can show
// "the kubelet restarted this pod" next to "loss to this node spiked".
//
// Enabled defaults to FALSE for the same reason mtr.enrichment does: turning it
// on gives the console pod a NEW egress and a NEW RBAC grant (events/nodes/pods
// read), and an unfiltered cluster event firehose is a cardinality and privacy
// bug, not a feature. Switching it on is a deliberate act by an operator who
// has also applied the RBAC.
//
// The captured rows live in PostgreSQL, so the whole block is inert with
// database.mode=disabled — cmd/console warns and skips the reader rather than
// watching events it has nowhere to put.
type KubernetesContextConfig struct {
	// Enabled is the master gate.
	Enabled bool `yaml:"enabled"`
	// Namespace is the ONE namespace whose pod events are captured. Empty --
	// the default -- means "the namespace this pod runs in", read from the
	// POD_NAMESPACE environment variable (the chart sets it from
	// metadata.namespace via the downward API), falling back to "default" when
	// even that is unset, which is what a non-cluster process gets.
	//
	// It is deliberately one namespace and not a list: the release namespace is
	// where the agents and the controller run, and widening this is how a
	// capture turns into a cluster-wide event firehose (Decision 3, and the
	// leak-conscious constraint the milestone opens with).
	Namespace string `yaml:"namespace"`
	// ResyncInterval forces a periodic relist even while the watch is healthy.
	// It is the reader's backstop against a watch that is silently wedged --
	// connected, delivering nothing -- which no error path can detect. Ten
	// minutes because the apiserver keeps events for an hour by default, so a
	// relist at this cadence cannot miss one, and every relisted row the
	// database already holds costs one conflicting INSERT and a duplicate
	// counter increment, never a duplicate row.
	ResyncInterval time.Duration `yaml:"resyncInterval"`
}

// podNamespaceEnv is the downward-API variable the chart sets on the console
// pod. It is read ONLY as the fallback for an empty
// kubernetesContext.namespace: KCONMON_NG_CONSOLE_CONFIG stays the console's
// one true env var for configuration, and this is identity, not configuration.
const podNamespaceEnv = "POD_NAMESPACE"

// ResolveNamespace returns the effective namespace for pod-event capture:
// Namespace when set, else $POD_NAMESPACE, else "default". Called once at
// startup (kubectx.New and cmd/console's log line) -- never per event.
//
// It mirrors DatabaseConfig.ResolveDSN's shape: the config field wins, the
// mounted/injected value is the fallback, and resolution is an explicit method
// rather than something Load bakes in, so a test can pin either side.
func (k *KubernetesContextConfig) ResolveNamespace() string {
	if k.Namespace != "" {
		return k.Namespace
	}
	if ns := strings.TrimSpace(os.Getenv(podNamespaceEnv)); ns != "" {
		return ns
	}
	return "default"
}

// validate enforces kubernetesContext.* invariants. Like SchedulerConfig's, the
// interval is only checked when the feature is enabled: a leftover zero in the
// values.yaml of an operator who never switched the reader on must not be a
// boot failure. When it IS on, a non-positive resync would fire a relist
// storm, so it is rejected rather than silently re-defaulted.
func (k *KubernetesContextConfig) validate() error {
	if k.Enabled && k.ResyncInterval <= 0 {
		return fmt.Errorf("kubernetesContext.resyncInterval must be positive when kubernetesContext.enabled is true, got %v",
			k.ResyncInterval)
	}
	return nil
}

// MTRConfig groups everything the Console does with MTR path history beyond
// simply storing it. Today that is exactly one thing — hop enrichment — but
// the block exists rather than a top-level `enrichment:` key because the next
// MTR-shaped knob (a projector cap, a snapshot retention override) belongs
// beside it, not beside `prometheus:`.
type MTRConfig struct {
	Enrichment EnrichmentConfig `yaml:"enrichment"`
}

// EnrichmentConfig configures the hop-address enrichment resolver
// (internal/console/enrich): a TTL cache in mtr_hop_enrichment over two
// independently-gated sources, rDNS and MaxMind mmdb files.
//
// Enabled defaults to FALSE, for a stronger reason than SchedulerConfig's:
// enrichment is the only part of the Console that makes the pod talk to
// something other than the controller, Prometheus, Valkey and PostgreSQL. rDNS
// sends every hop address in the fleet's traces to whatever resolver the pod's
// /etc/resolv.conf names. That is a deliberate act with an egress footprint,
// not a default.
//
// The cache lives in PostgreSQL, so the whole block is inert with
// database.mode=disabled — cmd/console warns and skips the resolver rather
// than resolving the same address on every single read.
type EnrichmentConfig struct {
	// Enabled is the master gate. With it off, nothing below is read and the
	// snapshot-detail handler keeps serving the cache-only view Task 4 built.
	Enabled bool `yaml:"enabled"`
	// RDNS and GeoIP gate INDEPENDENTLY: an air-gapped cluster with mounted
	// mmdb files and no reachable resolver runs geoip-only, and a cluster with
	// internal DNS and no MaxMind licence runs rdns-only.
	RDNS  RDNSConfig  `yaml:"rdns"`
	GeoIP GeoIPConfig `yaml:"geoip"`
	// TTL is a cache row's lifetime. A row older than this is re-resolved on
	// the next read that wants it (M5 Decision 4: no background refresher --
	// an address nobody looks at costs nothing). 24h by default because the
	// answers are slow-moving: a hop's PTR record and its ASN change on the
	// order of months.
	TTL time.Duration `yaml:"ttl"`
}

// RDNSConfig gates the reverse-DNS source and bounds it.
type RDNSConfig struct {
	Enabled bool `yaml:"enabled"`
	// TimeoutMs bounds ONE lookup, not the batch. It is milliseconds rather
	// than a time.Duration because the useful range is 100-1000ms and an
	// operator who writes `timeoutMs: 500` cannot accidentally mean 500ns --
	// a resolver budget that quietly rounds to nothing would make every hop
	// look unresolvable.
	TimeoutMs int `yaml:"timeoutMs"`
}

// GeoIPConfig points at the two mmdb files (Decision 5: operator-provided
// volumes, never downloaded by the Console). An empty path is that source
// switched off, the same "empty means disabled" convention controller.url,
// prometheus.url and valkey.address already use. An UNREADABLE path is not a
// boot failure either: enrich.New warns and disables that one source, because
// a bad mount must never cost the operator their trace history.
type GeoIPConfig struct {
	ASNPath  string `yaml:"asnPath"`  // e.g. /geoip/GeoLite2-ASN.mmdb; empty = ASN/provider lookups off
	CityPath string `yaml:"cityPath"` // e.g. /geoip/GeoLite2-City.mmdb; empty = geo lookups off
}

// validate enforces mtr.enrichment.* invariants. Everything is checked ONLY
// when the master gate is on, SchedulerConfig.validate's convention: rejecting
// a leftover zero for a feature the operator has not switched on would be a
// boot failure over nothing.
//
// The all-sources-off case fails CLOSED and names all three knobs. It is the
// one misconfiguration that would otherwise be invisible: the resolver would
// start, every lookup would resolve to an empty row, and the cache would fill
// with authoritative-looking nothing that the TTL then protects for a day.
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

// SchedulerConfig configures the schedule loop (internal/console/scheduler):
// the advisory-locked tick that fires due check_schedules rows through the
// diagnostics runner and drives the stuck-run reaper.
//
// Enabled defaults to FALSE on purpose. Schedules can already be created and
// stored (M4 Task 4) without anything acting on them, so an upgrade to the
// milestone that adds this loop must not, on its own, start dispatching fleet
// traffic from rows an operator entered while nothing was consuming them.
// Turning it on is a deliberate act.
type SchedulerConfig struct {
	// Enabled turns the loop on. It also requires a resolved database DSN --
	// check_schedules and the cross-replica advisory lock both live in
	// PostgreSQL -- and a controller, since a fired schedule becomes an
	// ordinary diagnostics run; cmd/console logs and skips the loop rather
	// than failing to start when either is missing.
	Enabled bool `yaml:"enabled"`
	// TickInterval is the poll cadence. Short by design (seconds): the
	// advisory lock is taken and released per tick, so a replica that dies
	// mid-tick delays exactly one tick instead of wedging the fleet until
	// its session is reaped.
	TickInterval time.Duration `yaml:"tickInterval"`
}

// validate enforces scheduler.* invariants. The tick interval is only
// checked when the loop is enabled: a disabled loop never reads it, and
// rejecting a leftover zero in an operator's values.yaml for a feature they
// have not switched on would be a boot failure over nothing.
func (s *SchedulerConfig) validate() error {
	if s.Enabled && s.TickInterval <= 0 {
		return fmt.Errorf("scheduler.tickInterval must be positive when scheduler.enabled is true, got %v", s.TickInterval)
	}
	return nil
}

// RateLimitConfig configures the console's fixed-window request limits
// (internal/console/httpapi/ratelimit.go). Both are counts per MINUTE, and
// both follow the same "0 disables THAT limit" convention
// database.retentionDays already uses for pruning -- a negative value is a
// configuration error, not a disable.
//
// The window is counted in the cache.KV, so with console.valkey.mode=valkey
// the limit is cluster-wide, and with console.valkey.mode=disabled it is
// per-replica (the in-process KV has no cross-replica visibility, ADR-002):
// N replicas then admit up to N times the configured rate. That is weaker
// than configured, never stronger.
type RateLimitConfig struct {
	// RunsPerMinute caps POST /api/v1/runs per SUBJECT per minute (default
	// 10): a diagnostics run fans out to up to 400 agent pairs, so an
	// unbounded caller is a controller-load amplifier.
	RunsPerMinute int `yaml:"runsPerMinute"`
	// LoginPerMinute caps POST /api/v1/auth/login per USERNAME and, counted
	// independently, per SOURCE IP per minute (default 5). This one is an
	// availability control, not just an anti-brute-force one: argon2id is
	// deliberately 64 MiB per verification, and unlimited concurrent logins
	// against a 256Mi console pod is an unauthenticated OOM.
	LoginPerMinute int `yaml:"loginPerMinute"`
}

// validate enforces rateLimit.* invariants. Zero is legal (that limit is
// off); negative is not -- it would otherwise silently read as "off" too,
// hiding a typo in an operator's values.yaml behind a disabled security
// control.
func (rl *RateLimitConfig) validate() error {
	if rl.RunsPerMinute < 0 {
		return fmt.Errorf("rateLimit.runsPerMinute must be >= 0 (0 disables the limit), got %d", rl.RunsPerMinute)
	}
	if rl.LoginPerMinute < 0 {
		return fmt.Errorf("rateLimit.loginPerMinute must be >= 0 (0 disables the limit), got %d", rl.LoginPerMinute)
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
		RateLimit:  RateLimitConfig{RunsPerMinute: 10, LoginPerMinute: 5},
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
	if err := c.Webhooks.validate(); err != nil {
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
