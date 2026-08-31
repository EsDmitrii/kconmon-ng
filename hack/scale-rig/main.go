// Command scale-rig measures the kconmon-ng CONTROL PLANE on one machine: a real in-process
// controller (wired like cmd/controller, plaintext localhost listeners) against N real agent gRPC
// clients that register, watch peers and heartbeat — but run no probe schedulers or sockets. It
// answers, with numbers instead of promises: registrations/s sustained, broadcast coalescing under
// churn, p95 registration->fleet propagation, controller CPU/heap, and FULL_SYNC wire size at N.
// See README.md in this directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "scale rig failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	n := flag.Int("n", 100, "fleet size (number of in-process agent clients)")
	cold := flag.Duration("cold", 10*time.Second, "cold-start window: N registrations spread over this")
	churnFrac := flag.Float64("churn-frac", 0.10, "fraction of the fleet restarted during the churn phase")
	churnSpread := flag.Duration("churn", 30*time.Second, "churn window: restarts spread over this")
	steady := flag.Duration("steady", 60*time.Second, "steady-state heartbeat phase duration")
	probes := flag.Int("probes", 40, "sequential propagation probes after the scenarios")
	spacing := flag.Duration("probe-spacing", 300*time.Millisecond, "gap between probes (> the 200ms coalescing window)")
	hb := flag.Duration("heartbeat", 5*time.Second, "agent heartbeat interval (internal/agent.Run uses 5s)")
	flag.Parse()

	if *n < 1 {
		return fmt.Errorf("-n must be >= 1, got %d", *n)
	}

	raiseNoFile()

	logs := newLogCounter(os.Stderr)
	slog.SetDefault(slog.New(logs))

	cfg := defaultRigConfig(*n)
	cfg.ColdSpread = *cold
	cfg.ChurnFraction = *churnFrac
	cfg.ChurnSpread = *churnSpread
	cfg.Steady = *steady
	cfg.Probes = *probes
	cfg.ProbeSpacing = *spacing
	cfg.HeartbeatInterval = *hb

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	rig := newRig(cfg, logs)
	rep, err := rig.Run(ctx)
	if err != nil {
		return err
	}
	printReport(os.Stdout, rep)
	return nil
}
