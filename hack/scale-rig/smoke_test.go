package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestRigSmoke runs the whole rig — real controller, real agent clients — at a tiny scale with
// shortened windows, so the measurement plumbing itself is exercised under the race detector.
func TestRigSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("integration smoke test")
	}

	logs := newLogCounter(io.Discard)
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := RigConfig{
		N:                 6,
		ColdSpread:        800 * time.Millisecond,
		ChurnFraction:     0.34, // 2 restarts
		ChurnSpread:       600 * time.Millisecond,
		Steady:            1500 * time.Millisecond,
		Probes:            3,
		ProbeSpacing:      250 * time.Millisecond,
		ProbeGrace:        5 * time.Second,
		HeartbeatInterval: 500 * time.Millisecond,
		QuiesceIdle:       600 * time.Millisecond,
		QuiesceMax:        5 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rig := newRig(cfg, logs)
	rep, err := rig.Run(ctx)
	if err != nil {
		t.Fatalf("rig run: %v", err)
	}

	if rep.Registered != int64(cfg.N) {
		t.Fatalf("registered=%d, want %d", rep.Registered, cfg.N)
	}
	if rep.RegisterFailures != 0 {
		t.Fatalf("register failures: %d", rep.RegisterFailures)
	}
	if rep.ChurnEvents != 2 {
		t.Fatalf("churn events=%d, want 2", rep.ChurnEvents)
	}

	cold, churn, steady := rep.Phases[0], rep.Phases[1], rep.Phases[2]
	if cold.Flushes == 0 {
		t.Fatalf("cold start produced no broadcasts")
	}
	if cold.Changes < uint64(cfg.N) {
		t.Fatalf("cold changes=%d, want >= %d", cold.Changes, cfg.N)
	}
	// Each churn event is one deregister plus one register.
	if churn.Changes != 2*uint64(rep.ChurnEvents) {
		t.Fatalf("churn changes=%d, want %d", churn.Changes, 2*rep.ChurnEvents)
	}
	if churn.Flushes == 0 {
		t.Fatalf("churn produced no broadcasts")
	}
	// Steady state is heartbeats only: after a real quiesce nothing may broadcast.
	if steady.Flushes != 0 {
		t.Fatalf("steady phase saw %d broadcasts, want 0", steady.Flushes)
	}

	if rep.ProbeIncomplete != 0 || rep.ProbeDone != cfg.Probes {
		t.Fatalf("probes done=%d incomplete=%d, want %d/0", rep.ProbeDone, rep.ProbeIncomplete, cfg.Probes)
	}
	if rep.PropP95 <= 0 {
		t.Fatalf("propagation p95 not measured")
	}
	if rep.FullSyncBytes <= 0 {
		t.Fatalf("no FULL_SYNC size at N=%d observed", cfg.N)
	}

	// The report must render without panicking and carry the summary line.
	var sb strings.Builder
	printReport(&sb, rep)
	if !strings.Contains(sb.String(), "SUMMARY n=6") {
		t.Fatalf("report is missing the summary line:\n%s", sb.String())
	}
}
