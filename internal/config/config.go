package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

type Config struct {
	// Mode (KCONMON_NG_MODE) is read by nothing. It still parses so an existing config keeps
	// loading, and validation warns that it is ignored.
	Mode          string `yaml:"mode"`
	MetricsPrefix string `yaml:"metricsPrefix"`
	HTTPPort      int    `yaml:"httpPort"`
	GRPCPort      int    `yaml:"grpcPort"`
	// MetricsPort carries /metrics and the health endpoints on a listener of its own: the controller's
	// httpPort serves an unauthenticated API, and a NetworkPolicy opens ports, not paths, so a separate
	// port is how a scraper gets in without being able to drive the fleet.
	MetricsPort        int              `yaml:"metricsPort"`
	LogLevel           string           `yaml:"logLevel"`
	LogFormat          string           `yaml:"logFormat"`
	FailureDomainLabel string           `yaml:"failureDomainLabel"`
	ControllerAddress  string           `yaml:"controllerAddress"`
	Agent              AgentConfig      `yaml:"agent"`
	Checkers           CheckersConfig   `yaml:"checkers"`
	Controller         ControllerConfig `yaml:"controller"`
	Topology           TopologyConfig   `yaml:"topology"`
	// Observability is read by nothing: no tracer is created. It still parses so an existing config
	// keeps loading, and validation warns when it is set.
	Observability ObservabilityConfig `yaml:"observability"`
}

// The two probe-mesh shapes the controller can plan. Full is the default: every agent probes every
// other.
const (
	TopologyModeFull   = "full"
	TopologyModeSparse = "sparse"
)

// TopologyConfig selects the probe-mesh shape the controller plans. Controller-side only: agents
// just probe whatever peer list they are handed, so this block changes nothing in agent code.
type TopologyConfig struct {
	Mode   string               `yaml:"mode"`
	Sparse SparseTopologyConfig `yaml:"sparse"`
}

// SparseTopologyConfig tunes the sparse plan (see internal/controller/meshplan): a ring over the
// node-name order plus HRW-picked cross-zone chords.
type SparseTopologyConfig struct {
	// RingDegree is how many ring successors each agent probes; >= 1, or the graph falls apart.
	RingDegree int `yaml:"ringDegree"`
	// ZoneChords is how many extra cross-zone targets each agent probes on top of the ring.
	ZoneChords int `yaml:"zoneChords"`
	// AutoThreshold keeps fleets SMALLER than it on full mesh even in sparse mode; 0 disables the
	// floor. Small fleets lose nothing by probing everyone, and full mesh needs no matrix caveats.
	AutoThreshold int `yaml:"autoThreshold"`
}

/*
AgentConfig is the agent's identity: what it asserts about itself at registration. Every key is
optional — in-cluster the chart's Downward API env fills the same values, and on a bare host each
key has a fallback (hostname for nodeName, outbound-interface autodetect for advertiseAddress,
controller-side zone resolution for zone). Identity is resolved once at startup; a hot-reload of
this block takes effect on the next agent restart, because a changed identity is a different agent
to every peer.
*/
type AgentConfig struct {
	// NodeName is the agent's identity in the mesh; empty falls back to os.Hostname().
	NodeName string `yaml:"nodeName"`
	// AdvertiseAddress must be an IP LITERAL: the controller publishes it to every peer as a probe
	// target and rejects anything net.ParseIP refuses (validateAgentMeta). Empty means autodetect.
	AdvertiseAddress string `yaml:"advertiseAddress"`
	// Zone is an explicit assertion that always wins; empty lets the controller resolve the zone
	// from the node's failure-domain label (in-cluster only).
	Zone string `yaml:"zone"`
	// TLS switches the controller connection to the external gateway; an empty block keeps the
	// in-cluster plaintext dial byte-identical.
	TLS AgentTLSConfig `yaml:"tls"`
	// BootstrapTokenFile names a file whose content is sent as a bearer token on every RPC. Validation
	// refuses it unless TLS is on (TLS.InUse): a token over plaintext is published to the network.
	BootstrapTokenFile string `yaml:"bootstrapTokenFile"`
}

// AgentTLSConfig is the agent's side of the external gateway: how to verify the controller and,
// when the gateway pins identities, how to prove its own.
type AgentTLSConfig struct {
	// Enabled dials the gateway over TLS with no field below set: the system pool verifies a publicly
	// trusted certificate against the controllerAddress host.
	Enabled bool `yaml:"enabled"`
	// CAFile verifies the gateway's certificate against a private CA; empty means the system pool
	// once TLS is on through Enabled or another field. An empty caFile alone does not turn TLS on.
	CAFile string `yaml:"caFile"`
	// CertFile/KeyFile present a client certificate; both or neither.
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
	// ServerName overrides the hostname verified against the server certificate, for fleets that
	// dial the gateway by IP or through a load balancer.
	ServerName string `yaml:"serverName"`
}

// InUse reports whether the agent dials over TLS: enabled, or any field set. The zero block is the
// plaintext in-cluster dial, unchanged.
func (t AgentTLSConfig) InUse() bool {
	return t.Enabled || t.CAFile != "" || t.CertFile != "" || t.KeyFile != "" || t.ServerName != ""
}

type CheckersConfig struct {
	TCP      TCPCheckerConfig      `yaml:"tcp"`
	UDP      UDPCheckerConfig      `yaml:"udp"`
	ICMP     ICMPCheckerConfig     `yaml:"icmp"`
	PMTU     PMTUCheckerConfig     `yaml:"pmtu"`
	DNS      DNSCheckerConfig      `yaml:"dns"`
	HTTP     HTTPCheckerConfig     `yaml:"http"`
	MTR      MTRCheckerConfig      `yaml:"mtr"`
	External ExternalCheckerConfig `yaml:"external"`
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

// PMTUCheckerConfig drives the path-MTU probe. Size 0 probes at the MTU of the route to the peer (its
// mtu metric, never above the egress device's), which is what the CNI configured for the pod and the
// right question almost always; an explicit size exists for networks that deliberately run below it.
type PMTUCheckerConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	Size     int           `yaml:"size"`
}

// PMTUMinSize and PMTUMaxSize bound an explicit checkers.pmtu.size: 576 is the datagram every IPv4
// host must accept, 65535 the IPv4 total-length ceiling.
const (
	PMTUMinSize = 576
	PMTUMaxSize = 65535
)

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
	// InsecureSkipVerify turns certificate verification off for this target only. The default
	// verifies, as the external HTTP checker does, so an expired, misissued or intercepted
	// certificate fails the check instead of counting as healthy https.
	InsecureSkipVerify bool `yaml:"insecureSkipVerify,omitempty"`
}

type MTRCheckerConfig struct {
	Cooldown time.Duration `yaml:"cooldown"`
	MaxHops  int           `yaml:"maxHops"`
}

// ExternalCheckerConfig gates every probe whose destination is not a peer agent; timeout bounds the
// resolution-and-authorisation step of an external task (DNS lookup plus allowlist check).
type ExternalCheckerConfig struct {
	Enabled      bool          `yaml:"enabled"`
	AllowedCIDRs []string      `yaml:"allowedCidrs"`
	DeniedCIDRs  []string      `yaml:"deniedCidrs"`
	MaxTargets   int           `yaml:"maxTargets"`
	Timeout      time.Duration `yaml:"timeout"`
}

type ControllerConfig struct {
	LeaderElection  bool                  `yaml:"leaderElection"`
	AgentTTL        time.Duration         `yaml:"agentTtl"`
	Events          EventsConfig          `yaml:"events"`
	ExternalGateway ExternalGatewayConfig `yaml:"externalGateway"`
	PrometheusSD    PrometheusSDConfig    `yaml:"prometheusSD"`
}

// PrometheusSDConfig gates GET /api/v1/prometheus/sd on both the API and the metrics listener.
// Off is the opt-out for operators who do not want external hosts' addresses disclosed to
// everything admitted to metricsPort; when off the route answers 404.
type PrometheusSDConfig struct {
	Enabled bool `yaml:"enabled"`
}

/*
ExternalGatewayConfig is a SECOND gRPC listener for agents outside the cluster: same services, same
registry, but TLS with a bearer token — because the in-cluster port is guarded by a NetworkPolicy
and the gateway is guarded by nothing but what is configured here. The in-cluster listener is not
affected by this block in any way.
*/
type ExternalGatewayConfig struct {
	Enabled bool             `yaml:"enabled"`
	Port    int              `yaml:"port"`
	TLS     GatewayTLSConfig `yaml:"tls"`
	// BootstrapTokenFile names the file holding the shared bearer token every external agent must
	// present. Required when the gateway is enabled: without it the gateway would trust network
	// position alone, which is exactly what it exists to not do.
	BootstrapTokenFile string `yaml:"bootstrapTokenFile"`
}

// GatewayTLSConfig is the gateway's certificate material. ClientCAFile is the v1 identity story:
// when set, every caller must present a certificate this CA signed, and the cert's CN/URI SAN is
// pinned to the agent identity in each request (see internal/controller/authn.go). Token-only mode
// (no client CA) authenticates fleet membership but cannot tell agents apart.
type GatewayTLSConfig struct {
	CertFile     string `yaml:"certFile"`
	KeyFile      string `yaml:"keyFile"`
	ClientCAFile string `yaml:"clientCaFile"`
}

// EventsConfig gates the controller's WatchEvents gRPC stream and the
// "events" capability flag on GET /api/v1/version.
type EventsConfig struct {
	Enabled bool `yaml:"enabled"`
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
	mu  sync.RWMutex
	cfg *Config
	// appliedHash is the sha256 of the file content cfg was built from, so the watcher can tell a
	// real change from a chmod or a rewrite with the same bytes.
	appliedHash [sha256.Size]byte
	filePath    string
	onChange    []OnChangeFunc

	// watchMu serialises WatchForChanges and Close, so a restarted watcher never overlaps the old
	// watch goroutine.
	watchMu   sync.Mutex
	watcher   *fsnotify.Watcher
	watchDone chan struct{}
	// target is the event name of the file the config path resolves to, "" when the path is no
	// symlink; targetDir is the extra directory watched for it, "" when it lies in the config
	// directory. The watch goroutine owns both while it runs.
	target    string
	targetDir string
}

// configReloadDebounce folds the burst of events one rewrite produces into a single reload.
const configReloadDebounce = 250 * time.Millisecond

func NewLoader(filePath string) *Loader {
	return &Loader{
		cfg:      DefaultConfig(),
		filePath: filePath,
	}
}

func (l *Loader) Load() error {
	var data []byte
	if l.filePath != "" {
		var err error
		if data, err = os.ReadFile(l.filePath); err != nil {
			return fmt.Errorf("loading config file: %w", err)
		}
	}
	_, err := l.load(data)
	return err
}

// load builds the config from the file content data (nil when there is no file), validates it and
// applies it together with the content hash, so the hash always describes the config in effect. It
// returns the subscribers registered at the moment of the swap.
func (l *Loader) load(data []byte) ([]OnChangeFunc, error) {
	cfg := DefaultConfig()

	if data != nil {
		if err := decodeConfig(data, cfg); err != nil {
			return nil, fmt.Errorf("loading config file: %w", err)
		}
	}

	l.loadFromEnv(cfg)
	applyDerivedDefaults(cfg)

	if err := l.validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	// Stored lowercase, so Diff does not report a case-only edit as a change.
	cfg.LogLevel = strings.ToLower(cfg.LogLevel)
	cfg.LogFormat = strings.ToLower(cfg.LogFormat)

	l.mu.Lock()
	l.cfg = cfg
	l.appliedHash = sha256.Sum256(data)
	subscribers := slices.Clip(l.onChange)
	l.mu.Unlock()

	return subscribers, nil
}

func (l *Loader) Get() *Config {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c := *l.cfg
	return &c
}

// OnChange registers fn to run on the watch goroutine after every applied reload. The config swap and
// the subscriber snapshot happen under one lock, so a subscriber that registers first and then reads
// Get() cannot miss a reload in between. Close waits for running callbacks: fn must not call Close.
func (l *Loader) OnChange(fn OnChangeFunc) {
	l.mu.Lock()
	l.onChange = append(l.onChange, fn)
	l.mu.Unlock()
}

// WatchForChanges starts the watcher; a second call while it runs is a no-op.
func (l *Loader) WatchForChanges() error {
	if l.filePath == "" {
		return nil
	}
	l.watchMu.Lock()
	defer l.watchMu.Unlock()
	if l.watcher != nil {
		return nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}

	// The directory is watched, not the file: a rename-replace (editors, ansible, puppet, mv) leaves a
	// file watch on the old inode, and a ConfigMap swaps a ..data symlink only a directory watch sees.
	dir := filepath.Dir(l.filePath)
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("watching directory %s: %w", dir, err)
	}
	l.watcher = watcher
	l.watchDone = make(chan struct{})
	l.target, l.targetDir = "", ""
	l.followTarget(watcher)

	go l.watchLoop(watcher, l.watchDone)
	return nil
}

// Close stops the watcher and waits for a reload in flight, OnChange callbacks included.
func (l *Loader) Close() error {
	l.watchMu.Lock()
	defer l.watchMu.Unlock()
	if l.watcher == nil {
		return nil
	}
	err := l.watcher.Close()
	<-l.watchDone
	l.watcher, l.watchDone = nil, nil
	return err
}

func (l *Loader) watchLoop(watcher *fsnotify.Watcher, done chan<- struct{}) {
	defer close(done)
	var debounce <-chan time.Time
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if l.isConfigEvent(event) {
				debounce = time.After(configReloadDebounce)
			}
		case <-debounce:
			debounce = nil
			l.followTarget(watcher)
			l.reloadIfChanged()
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			slog.Error("config watcher error", "error", err)
		}
	}
}

// followTarget tracks the file the config path resolves to: inotify reports an edit made to it under
// its own name, never the symlink's. A target in another directory needs that directory watched too.
// It re-resolves on every reload because the link can be repointed; an unresolvable path keeps the
// current watch.
func (l *Loader) followTarget(watcher *fsnotify.Watcher) {
	resolved, err := filepath.EvalSymlinks(l.filePath)
	if err != nil {
		return
	}
	target, targetDir := "", ""
	if resolved != filepath.Clean(l.filePath) {
		target = resolved
		if configDir, err := filepath.EvalSymlinks(filepath.Dir(l.filePath)); err == nil && filepath.Dir(resolved) == configDir {
			// fsnotify names events by the watched path, which is the config directory as given.
			target = filepath.Join(filepath.Dir(l.filePath), filepath.Base(resolved))
		} else {
			targetDir = filepath.Dir(resolved)
		}
	}
	if targetDir != l.targetDir {
		if l.targetDir != "" {
			// Already gone when the directory was deleted (a superseded ConfigMap version).
			_ = watcher.Remove(l.targetDir)
		}
		if targetDir != "" {
			if err := watcher.Add(targetDir); err != nil {
				slog.Warn("cannot watch the config symlink target's directory; in-place edits of the target "+
					"are not picked up", "file", l.filePath, "target", target, "error", err)
				target, targetDir = "", ""
			}
		}
	}
	l.target, l.targetDir = target, targetDir
}

// isConfigEvent keeps the events about our file, its symlink target and the ConfigMap symlink swap
// apart from everything else that happens in the watched directories. inotify reports the swap as
// Rename `..data_tmp` + Create `..data`; fsnotify's kqueue backend does not report the rename at all
// and at best a Create of `..data_tmp`, so every `..data*` name counts. A false positive costs one
// read, because the reload compares content first.
func (l *Loader) isConfigEvent(event fsnotify.Event) bool {
	name := filepath.Clean(event.Name)
	return name == filepath.Clean(l.filePath) || (l.target != "" && name == l.target) ||
		strings.HasPrefix(filepath.Base(name), "..data")
}

// reloadIfChanged applies the file when its content differs from what is applied. A missing file
// keeps the current config: a rewrite is often a remove followed by a create. So does an empty one:
// an in-place writer truncates before it writes, and a debounce that expires in between would
// otherwise reset every setting to its default.
func (l *Loader) reloadIfChanged() {
	data, err := os.ReadFile(l.filePath)
	if err != nil {
		slog.Warn("config file unreadable, keeping the current config", "file", l.filePath, "error", err)
		return
	}
	if len(data) == 0 {
		slog.Warn("config file is empty, keeping the current config until it is written", "file", l.filePath)
		return
	}
	l.mu.RLock()
	unchanged := sha256.Sum256(data) == l.appliedHash
	l.mu.RUnlock()
	if unchanged {
		return
	}
	slog.Info("config file changed, reloading", "file", l.filePath)
	subscribers, err := l.load(data)
	if err != nil {
		slog.Error("failed to reload config", "error", err)
		return
	}
	cfg := l.Get()
	for _, fn := range subscribers {
		fn(cfg)
	}
}

// decodeConfig overlays the YAML in data onto cfg. Unknown keys are an error, so a typo cannot
// silently fall back to a default.
func decodeConfig(data []byte, cfg *Config) error {
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
	// Decode reads one document; a second one would be dropped whole, unknown keys and all. An
	// empty trailing document (a closing `---`) is harmless.
	for {
		var extra yaml.Node
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !isEmptyDocument(&extra) {
			return fmt.Errorf("config file must contain a single YAML document, found another at line %d", extra.Line)
		}
	}
}

func isEmptyDocument(doc *yaml.Node) bool {
	for _, n := range doc.Content {
		if n.Kind != yaml.ScalarNode || n.Tag != "!!null" {
			return false
		}
	}
	return true
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
	// The identity block shares its env names with the chart's Downward API injection, so the same
	// ConfigMap can be mounted fleet-wide while each pod still registers as its own node. Env wins
	// over the file for the whole block, decided here and nowhere else.
	if v := os.Getenv("KCONMON_NG_NODE_NAME"); v != "" {
		cfg.Agent.NodeName = v
	}
	if v := os.Getenv("KCONMON_NG_ADVERTISE_ADDRESS"); v != "" {
		cfg.Agent.AdvertiseAddress = v
	}
	if v := os.Getenv("KCONMON_NG_ZONE"); v != "" {
		cfg.Agent.Zone = v
	}
}

// minAgentTTL is two agent heartbeats (the agent beats every 5s): a shorter TTL evicts agents that
// are answering perfectly well.
const minAgentTTL = 10 * time.Second

// metricsPrefixPattern keeps every family a classic Prometheus name that needs no escaping: a dash or
// a dot is rewritten differently by classic and UTF-8 scrapers, and an empty prefix leaves `_tcp_...`.
var metricsPrefixPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// maxUDPPackets matches the chart schema: the probe waits for each packet in turn.
const maxUDPPackets = 100

// maxMTRHops matches the chart schema.
const maxMTRHops = 64

func (l *Loader) validate(cfg *Config) error {
	if cfg.HTTPPort < 1 || cfg.HTTPPort > 65535 {
		return fmt.Errorf("httpPort must be between 1 and 65535, got %d", cfg.HTTPPort)
	}
	if cfg.GRPCPort < 1 || cfg.GRPCPort > 65535 {
		return fmt.Errorf("grpcPort must be between 1 and 65535, got %d", cfg.GRPCPort)
	}
	if cfg.MetricsPort < 1 || cfg.MetricsPort > 65535 {
		return fmt.Errorf("metricsPort must be between 1 and 65535, got %d", cfg.MetricsPort)
	}
	if cfg.HTTPPort == cfg.GRPCPort {
		return fmt.Errorf("httpPort and grpcPort must be different")
	}
	if cfg.MetricsPort == cfg.HTTPPort || cfg.MetricsPort == cfg.GRPCPort {
		return fmt.Errorf("metricsPort must differ from httpPort and grpcPort (got %d)", cfg.MetricsPort)
	}
	if !metricsPrefixPattern.MatchString(cfg.MetricsPrefix) {
		return fmt.Errorf("metricsPrefix must start with a letter and contain only letters, digits and "+
			"underscores, got %q", cfg.MetricsPrefix)
	}

	if _, ok := logLevels[strings.ToLower(cfg.LogLevel)]; !ok {
		return fmt.Errorf("logLevel must be one of debug, info, warn, error; got %q", cfg.LogLevel)
	}

	validFormats := map[string]bool{"json": true, "text": true}
	if !validFormats[strings.ToLower(cfg.LogFormat)] {
		return fmt.Errorf("logFormat must be one of json, text; got %q", cfg.LogFormat)
	}

	warn := warnLogger(cfg)
	if cfg.Mode != "" {
		warn.Warn("mode (KCONMON_NG_MODE) is ignored: nothing reads it; remove it from the config", "mode", cfg.Mode)
	}
	if cfg.Observability.OTel != (OTelConfig{}) {
		warn.Warn("observability.otel is ignored: no tracer is created; remove it from the config")
	}

	// The controller broadcasts this address to every peer as a probe target and refuses anything
	// net.ParseIP refuses, so a hostname or host:port must fail here, not at registration.
	if a := cfg.Agent.AdvertiseAddress; a != "" {
		ip := net.ParseIP(a)
		if ip == nil {
			return fmt.Errorf("agent.advertiseAddress %q must be an IP literal (no hostname, no port)", a)
		}
		if kind := UnreachableAdvertiseAddress(ip); kind != "" {
			return fmt.Errorf("agent.advertiseAddress %q is %s; peers probe this address, so it must be "+
				"one they can reach from their own hosts", a, kind)
		}
	}

	if err := validateAgentSecurity(&cfg.Agent); err != nil {
		return err
	}
	if err := validateExternalGateway(cfg); err != nil {
		return err
	}
	if err := validateTopology(cfg.Topology); err != nil {
		return err
	}

	// The TTL becomes a ticker period (agentTtl/2 in controller.Run), and time.NewTicker panics on a
	// non-positive one; below two heartbeats the sweep evicts the fleet between beats.
	if cfg.Controller.AgentTTL <= 0 {
		return fmt.Errorf("controller.agentTtl must be > 0, got %v", cfg.Controller.AgentTTL)
	}
	if cfg.Controller.AgentTTL < minAgentTTL {
		return fmt.Errorf("controller.agentTtl (%v) must be at least %v — two agent heartbeats — "+
			"or the sweep evicts the whole fleet between beats", cfg.Controller.AgentTTL, minAgentTTL)
	}

	if p := cfg.Checkers.UDP.Packets; p < 1 || p > maxUDPPackets {
		return fmt.Errorf("checkers.udp.packets must be between 1 and %d, got %d", maxUDPPackets, p)
	}
	if s := cfg.Checkers.PMTU.Size; s != 0 && (s < PMTUMinSize || s > PMTUMaxSize) {
		return fmt.Errorf("checkers.pmtu.size must be 0 (the MTU of the route to the peer) or between %d and %d, got %d",
			PMTUMinSize, PMTUMaxSize, s)
	}
	if h := cfg.Checkers.MTR.MaxHops; h < 1 || h > maxMTRHops {
		return fmt.Errorf("checkers.mtr.maxHops must be between 1 and %d, got %d", maxMTRHops, h)
	}
	if cfg.Checkers.MTR.Cooldown <= 0 {
		return fmt.Errorf("checkers.mtr.cooldown must be > 0, got %v", cfg.Checkers.MTR.Cooldown)
	}

	if cfg.Checkers.TCP.Enabled {
		if err := validateTiming(warn, "tcp", cfg.Checkers.TCP.Interval, cfg.Checkers.TCP.Timeout); err != nil {
			return err
		}
	}
	if udp := cfg.Checkers.UDP; udp.Enabled {
		if err := validateTiming(warn, "udp", udp.Interval, udp.Timeout); err != nil {
			return err
		}
		// Division rather than packets*timeout, which overflows for absurd timeouts.
		if udp.Timeout < udp.Interval && udp.Timeout >= udp.Interval/time.Duration(udp.Packets) {
			warn.Warn("checkers.udp.packets x checkers.udp.timeout >= checkers.udp.interval; a peer that "+
				"drops every packet holds a probe slot for the whole round", "checker", "udp",
				"packets", udp.Packets, "timeout", udp.Timeout, "interval", udp.Interval)
		}
	}
	if cfg.Checkers.ICMP.Enabled {
		if err := validateTiming(warn, "icmp", cfg.Checkers.ICMP.Interval, cfg.Checkers.ICMP.Timeout); err != nil {
			return err
		}
	}
	if cfg.Checkers.PMTU.Enabled {
		if err := validateTiming(warn, "pmtu", cfg.Checkers.PMTU.Interval, cfg.Checkers.PMTU.Timeout); err != nil {
			return err
		}
		warnPMTUInterval(warn, cfg.Checkers.PMTU)
	}
	if cfg.Checkers.DNS.Enabled {
		if err := validateTiming(warn, "dns", cfg.Checkers.DNS.Interval, cfg.Checkers.DNS.Timeout); err != nil {
			return err
		}
		if err := validateDNS(cfg.Checkers.DNS); err != nil {
			return err
		}
	}
	if cfg.Checkers.HTTP.Enabled {
		if err := validateTiming(warn, "http", cfg.Checkers.HTTP.Interval, cfg.Checkers.HTTP.Timeout); err != nil {
			return err
		}
		if err := validateHTTP(cfg.Checkers.HTTP); err != nil {
			return err
		}
	}
	if err := validateExternal(cfg.Checkers.External); err != nil {
		return err
	}

	return nil
}

/*
UnreachableAdvertiseAddress names the kind of ip when a peer cannot probe this agent at it, and
returns "" otherwise. A peer that dials the unspecified or a loopback address reaches its own agent,
so the pair reads healthy while the advertised host is down; multicast and broadcast name no single
host. Link-local and private addresses stay allowed: they are reachable on the right network.
*/
func UnreachableAdvertiseAddress(ip net.IP) string {
	switch {
	case ip.IsUnspecified():
		return "the unspecified address"
	case ip.IsLoopback():
		return "a loopback address"
	case ip.IsMulticast():
		return "a multicast address"
	case ip.Equal(net.IPv4bcast):
		return "the IPv4 broadcast address"
	}
	return ""
}

// validateAgentSecurity refuses the security half-configurations that would fail (or leak) only at
// runtime: a client cert without its key cannot handshake, and a bearer token over the plaintext
// in-cluster dial is a secret broadcast to anyone on the path.
func validateAgentSecurity(a *AgentConfig) error {
	if (a.TLS.CertFile == "") != (a.TLS.KeyFile == "") {
		return fmt.Errorf("agent.tls.certFile and agent.tls.keyFile must be set together " +
			"(a client certificate without its key cannot authenticate)")
	}
	if a.BootstrapTokenFile != "" && !a.TLS.InUse() {
		return fmt.Errorf("agent.bootstrapTokenFile requires TLS: set agent.tls.enabled (the system trust " +
			"pool), a caFile, a client certificate or a serverName; an empty caFile alone leaves the dial " +
			"plaintext, and a bearer token over plaintext gRPC is readable by anyone on the path")
	}
	return nil
}

// validateExternalGateway enforces that an enabled gateway can actually authenticate someone: a TLS
// server certificate and a bearer token are the floor, the client CA is the optional identity layer.
func validateExternalGateway(cfg *Config) error {
	gw := cfg.Controller.ExternalGateway
	if !gw.Enabled {
		return nil
	}
	if gw.Port < 1 || gw.Port > 65535 {
		return fmt.Errorf("controller.externalGateway.port must be between 1 and 65535, got %d", gw.Port)
	}
	if gw.Port == cfg.HTTPPort || gw.Port == cfg.GRPCPort || gw.Port == cfg.MetricsPort {
		return fmt.Errorf("controller.externalGateway.port (%d) must differ from httpPort, grpcPort and metricsPort",
			gw.Port)
	}
	if gw.TLS.CertFile == "" || gw.TLS.KeyFile == "" {
		return fmt.Errorf("controller.externalGateway.tls.certFile and .keyFile are required when the gateway " +
			"is enabled: the gateway exists to not expose plaintext gRPC outside the cluster")
	}
	if gw.BootstrapTokenFile == "" {
		return fmt.Errorf("controller.externalGateway.bootstrapTokenFile is required when the gateway is " +
			"enabled: without a token the gateway trusts network position alone")
	}
	return nil
}

const (
	// defaultExternalMaxTargets caps operator-defined external targets when the
	// feature is enabled and the operator left maxTargets unset. 100 is far more
	// than any realistic target list and still a bound.
	defaultExternalMaxTargets = 100
	// defaultExternalTimeout bounds resolution-and-authorisation of one external
	// destination when the operator left timeout unset. 10s is generous for a
	// DNS lookup and short enough that a hung resolver cannot pin a task slot.
	defaultExternalTimeout = 10 * time.Second
)

// applyDerivedDefaults fills in values that only make sense once an optional
// block is switched on. Nothing is defaulted while the block is disabled, so a
// disabled block stays byte-identical to what the operator wrote.
func applyDerivedDefaults(cfg *Config) {
	if cfg.Checkers.External.Enabled {
		if cfg.Checkers.External.MaxTargets == 0 {
			cfg.Checkers.External.MaxTargets = defaultExternalMaxTargets
		}
		if cfg.Checkers.External.Timeout == 0 {
			cfg.Checkers.External.Timeout = defaultExternalTimeout
		}
	}
}

// maxZoneChords bounds topology.sparse.zoneChords: the mesh plan sizes every agent's peer set by it,
// and a fleet that wants more cross-zone peers than this wants the full mesh.
const maxZoneChords = 64

// validateTopology refuses a sparse block that cannot plan a connected mesh. The sparse knobs are
// only checked in sparse mode, so a disabled block stays byte-identical to what the operator wrote.
func validateTopology(t TopologyConfig) error {
	switch strings.ToLower(t.Mode) {
	case "", TopologyModeFull:
		return nil
	case TopologyModeSparse:
		// The ring is the connectivity backbone of the sparse graph; without it nothing guarantees
		// every agent is probed at all.
		if t.Sparse.RingDegree < 1 {
			return fmt.Errorf("topology.sparse.ringDegree must be >= 1 in sparse mode, got %d",
				t.Sparse.RingDegree)
		}
		if t.Sparse.ZoneChords < 0 || t.Sparse.ZoneChords > maxZoneChords {
			return fmt.Errorf("topology.sparse.zoneChords must be between 0 and %d, got %d",
				maxZoneChords, t.Sparse.ZoneChords)
		}
		if t.Sparse.AutoThreshold < 0 {
			return fmt.Errorf("topology.sparse.autoThreshold must be >= 0, got %d", t.Sparse.AutoThreshold)
		}
		return nil
	default:
		return fmt.Errorf("topology.mode must be %q or %q, got %q",
			TopologyModeFull, TopologyModeSparse, t.Mode)
	}
}

// validateExternal fails startup on any external-checks misconfiguration that would otherwise widen
// what the agent may probe.
func validateExternal(e ExternalCheckerConfig) error {
	if !e.Enabled {
		return nil
	}
	if len(e.AllowedCIDRs) == 0 {
		return fmt.Errorf("checkers.external.allowedCidrs must be non-empty when checkers.external.enabled is true " +
			"(an empty list denies everything and is never read as allow-everything)")
	}
	if e.MaxTargets < 0 {
		return fmt.Errorf("checkers.external.maxTargets must be >= 0, got %d", e.MaxTargets)
	}
	if e.Timeout < 0 {
		return fmt.Errorf("checkers.external.timeout must be >= 0, got %v", e.Timeout)
	}
	if e.Timeout > 0 && e.Timeout < minCheckerTimeout {
		return fmt.Errorf("checkers.external.timeout must be 0 (the default) or at least %v, got %v",
			minCheckerTimeout, e.Timeout)
	}
	// Parse through the same constructor the agent enforces with, so a CIDR that
	// would be rejected at probe time is rejected at startup instead.
	if _, err := checker.NewAllowlist(e.AllowedCIDRs, e.DeniedCIDRs); err != nil {
		return fmt.Errorf("checkers.external.%w", err)
	}
	return nil
}

// minCheckerInterval and minCheckerTimeout refuse a unit typo such as 5ns for 5s, which a hot reload
// would otherwise hand to every agent at once; a timeout that short fails every probe.
const (
	minCheckerInterval = 100 * time.Millisecond
	minCheckerTimeout  = time.Millisecond
)

// validateTiming enforces a sane interval and a positive timeout for an enabled checker.
// Timeout >= Interval is intentionally only a warning: probes may be tuned
// tight and the operator may know what they are doing.
func validateTiming(warn *slog.Logger, name string, interval, timeout time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("checkers.%s.interval must be > 0 when the checker is enabled, got %v", name, interval)
	}
	if interval < minCheckerInterval {
		return fmt.Errorf("checkers.%s.interval must be at least %v when the checker is enabled, got %v",
			name, minCheckerInterval, interval)
	}
	if timeout <= 0 {
		return fmt.Errorf("checkers.%s.timeout must be > 0 when the checker is enabled, got %v", name, timeout)
	}
	if timeout < minCheckerTimeout {
		return fmt.Errorf("checkers.%s.timeout must be at least %v when the checker is enabled, got %v",
			name, minCheckerTimeout, timeout)
	}
	if timeout >= interval {
		warn.Warn("checker timeout >= interval; probes may overlap or starve",
			"checker", name, "timeout", timeout, "interval", interval)
	}
	return nil
}

const (
	// pmtuAlertWindow is the range PathMTUBlackHole reads (charts/kconmon-ng/templates/_rules.tpl).
	pmtuAlertWindow = 10 * time.Minute
	// pmtuFewProbesInterval leaves pmtuAlertWindow three or four probes, and the rule's sustained arm
	// (two failures in 30m, one in the last 10m) goes true and false between them.
	pmtuFewProbesInterval = 3 * time.Minute
)

// warnPMTUInterval warns rather than refuses, so a config that starts today keeps starting.
func warnPMTUInterval(warn *slog.Logger, p PMTUCheckerConfig) {
	if minInterval := checker.PMTUMinInterval(p.Timeout); p.Interval < minInterval {
		warn.Warn("checkers.pmtu.interval is short for its timeout: a black-hole search can run out of "+
			"budget and report no verdict", "checker", "pmtu", "interval", p.Interval, "timeout", p.Timeout,
			"minimum", minInterval)
	}
	switch {
	case p.Interval > pmtuAlertWindow:
		warn.Warn("checkers.pmtu.interval is longer than the window PathMTUBlackHole reads: the alert loses "+
			"its data between probes and its for: keeps resetting", "checker", "pmtu", "interval", p.Interval,
			"window", pmtuAlertWindow)
	case p.Interval >= pmtuFewProbesInterval:
		warn.Warn("checkers.pmtu.interval leaves PathMTUBlackHole a few probes per window: its sustained arm "+
			"catches a black hole on one of several ECMP paths late or intermittently", "checker", "pmtu",
			"interval", p.Interval, "window", pmtuAlertWindow)
	}
}

func validateDNS(dns DNSCheckerConfig) error {
	if len(dns.Hosts) == 0 {
		return fmt.Errorf("checkers.dns.hosts must not be empty when the dns checker is enabled")
	}
	for i, h := range dns.Hosts {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("checkers.dns.hosts[%d] must not be empty", i)
		}
	}
	for i, r := range dns.Resolvers {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("checkers.dns.resolvers[%d] must not be empty", i)
		}
		// Accept "host", "host:port" and a bare IP, IPv6 included: the checker joins the port onto a
		// bare address (checker.resolverDialAddr), so only a non-IP with a colon needs SplitHostPort.
		if strings.Contains(r, ":") && net.ParseIP(r) == nil {
			host, port, err := net.SplitHostPort(r)
			if err != nil {
				return fmt.Errorf("checkers.dns.resolvers[%d] %q is not a valid host, host:port or IP address: %w",
					i, r, err)
			}
			if host == "" {
				return fmt.Errorf("checkers.dns.resolvers[%d] %q has an empty host", i, r)
			}
			if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
				return fmt.Errorf("checkers.dns.resolvers[%d] %q has an invalid port %q (want 1-65535)", i, r, port)
			}
		}
	}
	return nil
}

// urlScheme is the "scheme://" a URL may start with.
var urlScheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// urlMask replaces the masked part of a target URL in validation errors.
const urlMask = "xxxxx"

/*
redactURL masks the password of a target URL, parsed or not: the validation errors are logged at
startup and on a failed reload, and the agent keeps passwords out of its logs. Everything from the
first colon after the scheme to the last @ goes. A malformed URL is exactly what reaches these errors,
so the mask does not trust url.Parse to find the userinfo: a missing scheme or a single slash puts
the credentials in the opaque part or the path, and a #, ? or / left unescaped in a password ends
the authority early.
*/
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	start := len(urlScheme.FindString(raw))
	if at < start {
		return raw
	}
	colon := strings.Index(raw[start:at], ":")
	if colon < 0 {
		return raw
	}
	return raw[:start+colon+1] + urlMask + raw[at:]
}

// urlParseReason is url.Parse's error without the raw URL it quotes. With a masked part the reason
// comes from the masked URL, since one from inside the mask may quote a password's bytes. When the
// masked URL parses, the fault is in the masked part, which can also be a port and path before an @.
func urlParseReason(raw string, err error) error {
	if redacted := redactURL(raw); redacted != raw {
		if _, err = url.Parse(redacted); err == nil {
			return fmt.Errorf("the fault is inside the part masked as %s (from the first colon after the scheme "+
				"to the last @)", urlMask)
		}
	}
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

func validateHTTP(h HTTPCheckerConfig) error {
	if len(h.Targets) == 0 {
		return fmt.Errorf("checkers.http.targets must not be empty when the http checker is enabled")
	}
	for i, t := range h.Targets {
		if strings.TrimSpace(t.URL) == "" {
			return fmt.Errorf("checkers.http.targets[%d].url must not be empty", i)
		}
		u, err := url.Parse(t.URL)
		if err != nil {
			return fmt.Errorf("checkers.http.targets[%d].url %q is not a valid URL: %w",
				i, redactURL(t.URL), urlParseReason(t.URL, err))
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("checkers.http.targets[%d].url %q must use scheme http or https, got %q",
				i, redactURL(t.URL), u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("checkers.http.targets[%d].url %q must include a host", i, redactURL(t.URL))
		}
		if s := t.ExpectStatus; s != 0 && (s < 100 || s > 599) {
			return fmt.Errorf("checkers.http.targets[%d].expectStatus must be 0 (unset) or between 100 and 599, got %d",
				i, s)
		}
		// The same check the checker's http.NewRequestWithContext applies on every probe. The URL has
		// parsed above, so the method is the only thing it can refuse, and its error would quote it again.
		if t.Method != "" {
			if _, err := http.NewRequestWithContext(context.Background(), t.Method, t.URL, http.NoBody); err != nil {
				return fmt.Errorf("checkers.http.targets[%d].method %q is not a valid HTTP method", i, t.Method)
			}
		}
		// The agent compiles it the same way (buildHTTPTargets); refused here, a typo cannot pass the
		// loader and then fail agent startup or a reload.
		if t.BodyPattern != "" {
			if _, err := regexp.Compile(t.BodyPattern); err != nil {
				return fmt.Errorf("checkers.http.targets[%d].bodyPattern %q is not a valid regular expression: %w",
					i, t.BodyPattern, err)
			}
		}
	}
	return nil
}
