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
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "scale rig failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := defaultRigConfig(100)
	flag.IntVar(&cfg.N, "n", cfg.N, "fleet size (number of in-process agent clients)")
	flag.DurationVar(&cfg.ColdSpread, "cold", cfg.ColdSpread, "cold-start window: N registrations spread over this")
	flag.Float64Var(&cfg.ChurnFraction, "churn-frac", cfg.ChurnFraction, "fraction of the fleet restarted during the churn phase")
	flag.DurationVar(&cfg.ChurnSpread, "churn", cfg.ChurnSpread, "churn window: restarts spread over this")
	flag.DurationVar(&cfg.Steady, "steady", cfg.Steady, "steady-state heartbeat phase duration")
	flag.IntVar(&cfg.Probes, "probes", cfg.Probes, "sequential propagation probes after the scenarios")
	flag.DurationVar(&cfg.ProbeSpacing, "probe-spacing", cfg.ProbeSpacing, "gap between probes (> the 200ms coalescing window)")
	flag.DurationVar(&cfg.HeartbeatInterval, "heartbeat", cfg.HeartbeatInterval, "agent heartbeat interval (internal/agent.Run uses 5s)")
	flag.Parse()

	if cfg.N < 1 {
		return fmt.Errorf("-n must be >= 1, got %d", cfg.N)
	}

	raiseNoFile()

	logs := newLogCounter(os.Stderr)
	slog.SetDefault(slog.New(logs))

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
