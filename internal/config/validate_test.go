package config

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// pristineSlogDefault is slog's built-in logger as the process starts, before any test replaces it.
var pristineSlogDefault = slog.Default()

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLog routes the default slog logger into a buffer until the test ends.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestDefaultConfigLoadsWithoutWarnings(t *testing.T) {
	logs := captureLog(t)
	if err := NewLoader("").Load(); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Fatalf("the default config logs a warning about itself:\n%s", out)
	}
}

func TestSetupLoggerIsCaseInsensitive(t *testing.T) {
	prev, prevLevel := slog.Default(), logLevel.Level()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		logLevel.Set(prevLevel)
	})

	SetupLogger("DEBUG", "TEXT")
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("logLevel DEBUG passed validation but debug logging is off")
	}
	if _, ok := slog.Default().Handler().(*slog.TextHandler); !ok {
		t.Errorf("logFormat TEXT passed validation but the handler is %T", slog.Default().Handler())
	}
}

func TestLoadNormalizesLogLevelAndFormatCase(t *testing.T) {
	loader := NewLoader(writeConfig(t, "logLevel: DEBUG\nlogFormat: Text\n"))
	if err := loader.Load(); err != nil {
		t.Fatal(err)
	}
	cfg := loader.Get()
	if cfg.LogLevel != "debug" || cfg.LogFormat != "text" {
		t.Fatalf("logLevel/logFormat = %q/%q, want debug/text", cfg.LogLevel, cfg.LogFormat)
	}
}

func TestDecodeRefusesSecondYAMLDocument(t *testing.T) {
	loader := NewLoader(writeConfig(t, "httpPort: 8081\n---\nbogusKey: 1\nhttpPort: 1\n"))
	err := loader.Load()
	if err == nil || !strings.Contains(err.Error(), "single YAML document") {
		t.Fatalf("a second YAML document must be refused, got err=%v", err)
	}

	for _, content := range []string{"---\nhttpPort: 8081\n", "httpPort: 8081\n---\n", "httpPort: 8081\n...\n"} {
		loader := NewLoader(writeConfig(t, content))
		if err := loader.Load(); err != nil {
			t.Errorf("%q: a single document with markers must load, got %v", content, err)
			continue
		}
		if got := loader.Get().HTTPPort; got != 8081 {
			t.Errorf("%q: httpPort = %d, want 8081", content, got)
		}
	}
}

func TestValidationGaps(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{"metricsPrefix empty", func(c *Config) { c.MetricsPrefix = "" }, true},
		{"metricsPrefix leading digit", func(c *Config) { c.MetricsPrefix = "1abc" }, true},
		{"metricsPrefix with dash", func(c *Config) { c.MetricsPrefix = "kconmon-ng" }, true},
		{"metricsPrefix with dot", func(c *Config) { c.MetricsPrefix = "kconmon.ng" }, true},
		{"metricsPrefix leading underscore", func(c *Config) { c.MetricsPrefix = "_kconmon" }, true},
		{"metricsPrefix custom", func(c *Config) { c.MetricsPrefix = "custom_prefix2" }, false},
		{"metricsPrefix mixed case", func(c *Config) { c.MetricsPrefix = "KConMon" }, false},

		{"dns resolver port above 65535", func(c *Config) { c.Checkers.DNS.Resolvers = []string{"1.1.1.1:99999"} }, true},
		{"dns resolver negative port", func(c *Config) { c.Checkers.DNS.Resolvers = []string{"1.1.1.1:-5"} }, true},
		{"dns resolver port zero", func(c *Config) { c.Checkers.DNS.Resolvers = []string{"1.1.1.1:0"} }, true},
		{"dns resolver port 5353", func(c *Config) { c.Checkers.DNS.Resolvers = []string{"1.1.1.1:5353"} }, false},
		{"dns resolver bracketed ipv6", func(c *Config) { c.Checkers.DNS.Resolvers = []string{"[2001:db8::1]:53"} }, false},

		{"http expectStatus 1000", httpTarget(HTTPTarget{ExpectStatus: 1000}), true},
		{"http expectStatus 20", httpTarget(HTTPTarget{ExpectStatus: 20}), true},
		{"http expectStatus negative", httpTarget(HTTPTarget{ExpectStatus: -1}), true},
		{"http expectStatus 404", httpTarget(HTTPTarget{ExpectStatus: 404}), false},
		{"http method with spaces", httpTarget(HTTPTarget{Method: "G E T"}), true},
		{"http method HEAD", httpTarget(HTTPTarget{Method: "HEAD"}), false},
		{"http bodyPattern not a regexp", httpTarget(HTTPTarget{BodyPattern: "(["}), true},
		{"http bodyPattern regexp", httpTarget(HTTPTarget{BodyPattern: `^ok\s*$`}), false},

		{"udp packets 101", func(c *Config) { c.Checkers.UDP.Packets = 101 }, true},
		{"udp packets 1e6", func(c *Config) { c.Checkers.UDP.Packets = 1_000_000 }, true},
		{"udp packets 100", func(c *Config) { c.Checkers.UDP.Packets = 100 }, false},
		{"mtr cooldown zero", func(c *Config) { c.Checkers.MTR.Cooldown = 0 }, true},
		{"mtr cooldown negative", func(c *Config) { c.Checkers.MTR.Cooldown = -time.Second }, true},
		{"mtr cooldown 1s", func(c *Config) { c.Checkers.MTR.Cooldown = time.Second }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)
			err := NewLoader("").validate(cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func httpTarget(target HTTPTarget) func(*Config) {
	return func(c *Config) {
		target.URL = "https://example.com/healthz"
		c.Checkers.HTTP.Enabled = true
		c.Checkers.HTTP.Targets = []HTTPTarget{target}
	}
}

// mode (KCONMON_NG_MODE) is read by nothing; it still parses so an old config keeps loading, and a
// warning says so.
func TestModeKeyLoadsAndWarnsItIsIgnored(t *testing.T) {
	logs := captureLog(t)
	loader := NewLoader(writeConfig(t, "mode: agent\n"))
	if err := loader.Load(); err != nil {
		t.Fatalf("a config that sets mode must keep loading: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "mode") {
		t.Fatalf("mode is ignored and must say so, got:\n%s", out)
	}
}

// observability.otel is read by nothing either: no tracer is ever created.
func TestObservabilityOTelLoadsAndWarnsItIsIgnored(t *testing.T) {
	logs := captureLog(t)
	loader := NewLoader(writeConfig(t, "observability:\n  otel:\n    enabled: true\n    endpoint: collector:4317\n"))
	if err := loader.Load(); err != nil {
		t.Fatalf("a config that sets observability.otel must keep loading: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "observability.otel") {
		t.Fatalf("observability.otel is ignored and must say so, got:\n%s", out)
	}
}

// The refusal quotes the value as the operator wrote it, so a search of the config finds it.
func TestLogLevelAndFormatErrorsQuoteTheValueAsWritten(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"logLevel: WARNING\n", `"WARNING"`},
		{"logFormat: YAML\n", `"YAML"`},
	} {
		err := NewLoader(writeConfig(t, tc.yaml)).Load()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: Load error = %v, want one quoting %s", tc.yaml, err, tc.want)
		}
	}
}

// Every checker refusal names its key by the full YAML path, the way the rest of validation does.
func TestCheckerRefusalsNameTheFullKeyPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modify  func(*Config)
		want    string
		notWant string
	}{
		{"udp packets", func(c *Config) { c.Checkers.UDP.Packets = 0 }, "checkers.udp.packets must be", ""},
		{"pmtu size", func(c *Config) { c.Checkers.PMTU.Size = 100 }, "checkers.pmtu.size must be", ""},
		{"mtr maxHops", func(c *Config) { c.Checkers.MTR.MaxHops = 65 }, "checkers.mtr.maxHops must be between 1 and 64", ""},
		{"mtr cooldown", func(c *Config) { c.Checkers.MTR.Cooldown = 0 }, "checkers.mtr.cooldown must be", ""},
		{"tcp interval", func(c *Config) { c.Checkers.TCP.Interval = 0 }, "checkers.tcp.interval must be", ""},
		{"tcp timeout", func(c *Config) { c.Checkers.TCP.Timeout = 0 }, "checkers.tcp.timeout must be", ""},
		{"dns hosts", func(c *Config) { c.Checkers.DNS.Hosts = nil }, "checkers.dns.hosts must not be empty", ""},
		{"dns host", func(c *Config) { c.Checkers.DNS.Hosts = []string{" "} }, "checkers.dns.hosts[0] must not be empty", ""},
		{"dns resolver", func(c *Config) { c.Checkers.DNS.Resolvers = []string{"1.1.1.1:0"} }, "checkers.dns.resolvers[0]", ""},
		{"http targets", func(c *Config) { c.Checkers.HTTP.Enabled = true }, "checkers.http.targets must not be empty", ""},
		{"http expectStatus", httpTarget(HTTPTarget{ExpectStatus: 20}), "checkers.http.targets[0].expectStatus", ""},
		{"http bodyPattern", httpTarget(HTTPTarget{BodyPattern: "(["}), "checkers.http.targets[0].bodyPattern", ""},
		// The method is quoted once, without the stdlib's own copy of it.
		{"http method", httpTarget(HTTPTarget{Method: "G E T"}),
			`checkers.http.targets[0].method "G E T" is not a valid HTTP method`, "net/http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.modify(cfg)
			err := NewLoader("").validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate() error = %v, want one containing %q", err, tc.want)
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Errorf("validate() error = %v, must not contain %q", err, tc.notWant)
			}
		})
	}
}

// main calls Load before SetupLogger. Until then startup warnings go through a logger built from the
// config being loaded, honouring its logLevel and logFormat, not slog's built-in text logger on stderr.
func TestStartupWarningsHonourLogLevelAndFormat(t *testing.T) {
	prevLogger, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(pristineSlogDefault)
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	load := func(level string) (stdout, builtin string) {
		t.Helper()
		var builtinOut syncBuffer
		log.SetOutput(&builtinOut)
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		realStdout := os.Stdout
		os.Stdout = w
		loadErr := NewLoader(writeConfig(t, "logLevel: "+level+"\nlogFormat: json\n"+
			"checkers:\n  udp:\n    packets: 40\n")).Load()
		os.Stdout = realStdout
		_ = w.Close()
		out, _ := io.ReadAll(r)
		_ = r.Close()
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		return string(out), builtinOut.String()
	}

	stdout, builtin := load("warn")
	if builtin != "" {
		t.Errorf("a startup warning went through slog's built-in text logger:\n%s", builtin)
	}
	if !strings.Contains(stdout, `"level":"WARN"`) || !strings.Contains(stdout, `"checker":"udp"`) {
		t.Errorf("with logFormat json the startup warning must be a JSON line on stdout, got:\n%s", stdout)
	}

	stdout, builtin = load("error")
	if stdout != "" || builtin != "" {
		t.Errorf("logLevel error must silence startup warnings, got stdout %q, built-in logger %q", stdout, builtin)
	}
}

// The UDP probe waits for each packet in turn, so packets x timeout is what one partitioned peer
// costs a round.
func TestUDPPacketsTimesTimeoutWarns(t *testing.T) {
	logs := captureLog(t)
	cfg := DefaultConfig()
	cfg.Checkers.UDP.Packets = 40
	if err := NewLoader("").validate(cfg); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "checker=udp") {
		t.Fatalf("udp.packets 40 x 250ms against a 5s interval must warn, got:\n%s", out)
	}
}

// An interval below 100ms (a 5ns typo for 5s) is refused: the checkers block is hot, so it would
// reach every agent at once, and a reload that refuses it keeps the running config.
func TestLoadRefusesACheckerIntervalBelowTheFloor(t *testing.T) {
	for _, name := range []string{"tcp", "udp", "icmp", "pmtu", "dns", "http"} {
		for _, tc := range []struct {
			interval string
			wantErr  bool
		}{{"5ns", true}, {"99ms", true}, {"100ms", false}} {
			yaml := fmt.Sprintf("checkers:\n  %s:\n    enabled: true\n    interval: %s\n    timeout: 50ms\n", name, tc.interval)
			if name == "http" {
				yaml += "    targets:\n      - url: https://example.com/healthz\n"
			}
			err := NewLoader(writeConfig(t, yaml)).Load()
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "checkers."+name+".interval")) {
				t.Errorf("%s interval %s: Load error = %v, want one naming checkers.%s.interval", name, tc.interval, err, name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%s interval %s: Load error = %v, want none", name, tc.interval, err)
			}
		}
	}
}

// A timeout below 1ms is refused for the same reason: it fails every probe.
func TestLoadRefusesACheckerTimeoutBelowTheFloor(t *testing.T) {
	for _, name := range []string{"tcp", "udp", "icmp", "pmtu", "dns", "http"} {
		for _, tc := range []struct {
			timeout string
			wantErr bool
		}{{"5ns", true}, {"1us", true}, {"999us", true}, {"1ms", false}} {
			yaml := fmt.Sprintf("checkers:\n  %s:\n    enabled: true\n    interval: 10s\n    timeout: %s\n", name, tc.timeout)
			if name == "http" {
				yaml += "    targets:\n      - url: https://example.com/healthz\n"
			}
			err := NewLoader(writeConfig(t, yaml)).Load()
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "checkers."+name+".timeout")) {
				t.Errorf("%s timeout %s: Load error = %v, want one naming checkers.%s.timeout", name, tc.timeout, err, name)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%s timeout %s: Load error = %v, want none", name, tc.timeout, err)
			}
		}
	}
	for _, tc := range []struct {
		timeout string
		wantErr bool
	}{{"5ns", true}, {"999us", true}, {"1ms", false}} {
		yaml := "checkers:\n  external:\n    enabled: true\n    allowedCidrs: [10.0.0.0/8]\n    timeout: " + tc.timeout + "\n"
		err := NewLoader(writeConfig(t, yaml)).Load()
		if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "external.timeout")) {
			t.Errorf("external timeout %s: Load error = %v, want one naming external.timeout", tc.timeout, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("external timeout %s: Load error = %v, want none", tc.timeout, err)
		}
	}
}

// A password in an http target URL never reaches the validation error, which is logged on startup
// and on a failed reload.
func TestHTTPTargetErrorsMaskTheURLPassword(t *testing.T) {
	for _, raw := range []string{
		"ftp://svc:s3cret@host/health",
		"https://svc:s3cret@/health",
		"https://svc:s3cret@host/%zz",
		"https://svc:s3cret@host:port/health",
		"https://svc:s3%zzcret@host/health",
		"https://svc:s3c@ret@host:bad/health",
		"https://svc:s3cret@host/health\x7f",
		"svc:s3cret@host/health",            // no scheme: the credentials land in the opaque part
		"https:/svc:s3cret@host/health",     // one slash: they land in the path
		"https:svc:s3cret@host/health",      // no slash at all
		"https://svc:s3c#ret@host/health",   // an unescaped # ends the authority inside the password
		"https://svc:s3c?ret@host/health",   // so does ?
		"https://svc:s3c/ret@host:x/health", // and / (a base64 password)
		"https://svc:s3c ret@host/health",   // a space
	} {
		err := validateHTTP(HTTPCheckerConfig{Targets: []HTTPTarget{{URL: raw}}})
		if err == nil {
			t.Errorf("%q: validation passed, want an error", raw)
			continue
		}
		// Every password starts with s3; a bad escape inside one must not be quoted either.
		if strings.Contains(err.Error(), "s3") || (strings.Contains(raw, "s3%zz") && strings.Contains(err.Error(), "zz")) {
			t.Errorf("%q: the error carries the password: %v", raw, err)
		}
		if !strings.Contains(err.Error(), "checkers.http.targets[0].url") {
			t.Errorf("%q: the error does not name the target: %v", raw, err)
		}
	}
}

// The mask runs from the first colon to the last @, so a port followed by an @ in the path or query
// masks a URL that has no password: the error points at the masked part and does not invent one.
func TestHTTPTargetErrorPointsAtTheMaskedPart(t *testing.T) {
	for _, raw := range []string{
		"https://api.internal:8443/%zz/ops@corp.example",
		"https://api.internal:8443/v1/%zz?owner=ops@corp.example",
	} {
		err := validateHTTP(HTTPCheckerConfig{Targets: []HTTPTarget{{URL: raw}}})
		if err == nil {
			t.Errorf("%q: validation passed, want an error", raw)
			continue
		}
		if strings.Contains(err.Error(), "password") || !strings.Contains(err.Error(), "masked as xxxxx") {
			t.Errorf("%q: error = %v, want it to point at the masked part without naming a password", raw, err)
		}
	}
}
