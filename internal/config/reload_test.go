package config

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/checker"
)

// A pmtu interval too short for its timeout leaves the black-hole search no room: warn, never refuse.
func TestPMTUIntervalShortForTimeoutWarns(t *testing.T) {
	logs := captureLog(t)
	cfg := DefaultConfig()
	cfg.Checkers.PMTU.Enabled = true
	cfg.Checkers.PMTU.Timeout = 500 * time.Millisecond
	cfg.Checkers.PMTU.Interval = checker.PMTUMinInterval(cfg.Checkers.PMTU.Timeout) - time.Second
	if err := NewLoader("").validate(cfg); err != nil {
		t.Fatalf("a short pmtu interval must warn, not fail: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "checkers.pmtu.interval") ||
		!strings.Contains(out, "checker=pmtu") {
		t.Fatalf("pmtu interval %v at timeout %v must warn, got:\n%s",
			cfg.Checkers.PMTU.Interval, cfg.Checkers.PMTU.Timeout, out)
	}
}

func TestPMTUIntervalAtTheDefaultsDoesNotWarn(t *testing.T) {
	logs := captureLog(t)
	cfg := DefaultConfig()
	cfg.Checkers.PMTU.Enabled = true
	if err := NewLoader("").validate(cfg); err != nil {
		t.Fatal(err)
	}
	if out := logs.String(); strings.Contains(out, "checkers.pmtu.interval") {
		t.Fatalf("the default pmtu interval/timeout must not warn, got:\n%s", out)
	}
}

// PathMTUBlackHole reads a 10m window: an interval longer than that leaves the rule without data.
func TestPMTUIntervalLongerThanTheAlertWindowWarns(t *testing.T) {
	logs := captureLog(t)
	cfg := DefaultConfig()
	cfg.Checkers.PMTU.Enabled = true
	cfg.Checkers.PMTU.Interval = 15 * time.Minute
	if err := NewLoader("").validate(cfg); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, want := range []string{"level=WARN", "checkers.pmtu.interval", "checker=pmtu", "window=10m0s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a 15m pmtu interval must warn about the alert window (%s), got:\n%s", want, out)
		}
	}
}

// With an interval of minutes the 10m window holds a few probes: the sustained arm of PathMTUBlackHole
// (two failures in 30m, one in the last 10m) then catches a black hole on one of several paths late.
func TestPMTUIntervalOfMinutesWarnsAboutTheSustainedArm(t *testing.T) {
	for _, interval := range []time.Duration{3 * time.Minute, 5 * time.Minute} {
		logs := captureLog(t)
		cfg := DefaultConfig()
		cfg.Checkers.PMTU.Enabled = true
		cfg.Checkers.PMTU.Interval = interval
		if err := NewLoader("").validate(cfg); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		for _, want := range []string{"level=WARN", "sustained arm", "checker=pmtu", "window=10m0s"} {
			if !strings.Contains(out, want) {
				t.Errorf("a %v pmtu interval must warn about the sustained arm (%s), got:\n%s", interval, want, out)
			}
		}
	}
}

// logLevel is hot: a reload changes what the process logs without a new handler; logFormat is not.
func TestSetLogLevelChangesTheLiveLogger(t *testing.T) {
	prev, prevLevel := slog.Default(), logLevel.Level()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		logLevel.Set(prevLevel)
	})

	SetupLogger("info", "text")
	handler := slog.Default().Handler()
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug is enabled at logLevel info")
	}

	SetLogLevel("debug")
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("SetLogLevel(debug) did not enable debug logging on the running logger")
	}
	SetLogLevel("ERROR")
	if slog.Default().Enabled(context.Background(), slog.LevelWarn) {
		t.Error("SetLogLevel(ERROR) left warn logging on")
	}
	if slog.Default().Handler() != handler {
		t.Error("SetLogLevel replaced the handler; logFormat is restart-bound and the handler must stay")
	}
}

// The change is confirmed in the log whichever way it goes, warn <-> error included, where an INFO
// line is below both levels.
func TestSetLogLevelAnnouncesEveryChange(t *testing.T) {
	prev, prevLevel := slog.Default(), logLevel.Level()
	t.Cleanup(func() {
		slog.SetDefault(prev)
		logLevel.Set(prevLevel)
	})
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: &logLevel})))
	for _, tc := range [][2]string{{"warn", "error"}, {"error", "warn"}, {"info", "error"}, {"error", "debug"}, {"debug", "warn"}} {
		buf.Reset()
		logLevel.Set(parseLogLevel(tc[0]))
		SetLogLevel(tc[1])
		if !strings.Contains(buf.String(), "log level changed") {
			t.Errorf("%s -> %s: no 'log level changed' line", tc[0], tc[1])
		}
		if got, want := logLevel.Level(), parseLogLevel(tc[1]); got != want {
			t.Errorf("%s -> %s: level is %v, want %v", tc[0], tc[1], got, want)
		}
	}
}

func TestDiffNamesChangedLeafKeys(t *testing.T) {
	old := DefaultConfig()
	next := DefaultConfig()
	if got := Diff(old, next); len(got) != 0 {
		t.Fatalf("Diff of two default configs = %v, want none", got)
	}

	next.HTTPPort++
	next.Checkers.TCP.Interval = time.Minute
	next.Checkers.DNS.Hosts = append(slices.Clone(next.Checkers.DNS.Hosts), "example.com")
	next.Agent.TLS.CAFile = "/etc/ca.pem"
	next.Controller.ExternalGateway.Enabled = true

	want := []string{
		"agent.tls.caFile",
		"checkers.dns.hosts",
		"checkers.tcp.interval",
		"controller.externalGateway.enabled",
		"httpPort",
	}
	if got := Diff(old, next); !slices.Equal(got, want) {
		t.Fatalf("Diff = %v, want %v", got, want)
	}
}

func TestKeyMatchesExactKeysAndSubtrees(t *testing.T) {
	for _, tc := range []struct {
		key      string
		patterns []string
		want     bool
	}{
		{"checkers.tcp.interval", []string{"checkers."}, true},
		{"checkers", []string{"checkers."}, false},
		{"logLevel", []string{"logLevel"}, true},
		{"logLevelX", []string{"logLevel"}, false},
		{"checkers.external.allowedCidrs", []string{"checkers.external.enabled", "checkers.external.allowedCidrs"}, true},
		{"checkers.external.maxTargets", []string{"checkers.external.enabled", "checkers.external.allowedCidrs"}, false},
	} {
		if got := KeyMatches(tc.key, tc.patterns...); got != tc.want {
			t.Errorf("KeyMatches(%q, %v) = %v, want %v", tc.key, tc.patterns, got, tc.want)
		}
	}
}
