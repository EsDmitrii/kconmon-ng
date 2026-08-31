package main

import (
	"fmt"
	"io"
	"runtime"
	"text/tabwriter"
	"time"
)

type PhaseReport struct {
	Name       string
	Span       time.Duration
	CPU        time.Duration
	Changes    uint64
	Flushes    uint64
	Callbacks  uint64
	Resubs     uint64
	Rereg      uint64
	Queued     float64 // controller-side peer updates queued (scraped counter delta)
	HeapInuse  uint64
	Sys        uint64
	Goroutines int
	Registered float64
	Conns      float64
}

func phaseReport(name string, before, after *resSnap) PhaseReport {
	return PhaseReport{
		Name:       name,
		Span:       after.When.Sub(before.When),
		CPU:        after.CPUClose - before.CPUOpen,
		Changes:    after.Changes - before.Changes,
		Flushes:    after.Flushes - before.Flushes,
		Callbacks:  after.Callbacks - before.Callbacks,
		Resubs:     after.Resubs - before.Resubs,
		Rereg:      after.Rereg - before.Rereg,
		Queued:     after.Scrape.PeerUpdatesTotal - before.Scrape.PeerUpdatesTotal,
		HeapInuse:  after.HeapInuse,
		Sys:        after.Sys,
		Goroutines: after.Goroutines,
		Registered: after.Scrape.Registered,
		Conns:      after.Scrape.GRPCConnections,
	}
}

type Report struct {
	Config RigConfig

	ColdWall         time.Duration
	Registered       int64
	RegisterFailures uint64
	RegisterRetries  uint64
	RegP50           time.Duration
	RegP95           time.Duration
	RegMax           time.Duration

	Phases      []PhaseReport
	ChurnEvents int
	ChurnWall   time.Duration
	SteadyWall  time.Duration

	ProbeCount            int
	ProbeDone             int
	ProbeIncomplete       int
	ProbeRegisterFailures int
	PropP50               time.Duration
	PropP95               time.Duration
	PropMax               time.Duration

	FullSyncPeers  int
	FullSyncBytes  int
	DeliveryP50    time.Duration
	DeliveryP95    time.Duration
	ObserverResubs uint64

	MaxRSS          uint64
	QuiesceTimeouts int
	LogLines        []string
}

func ratio(changes, flushes uint64) string {
	if flushes == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f:1", float64(changes)/float64(flushes))
}

func cpuPct(cpu, span time.Duration) float64 {
	if span <= 0 {
		return 0
	}
	return 100 * float64(cpu) / float64(span)
}

func printReport(w io.Writer, rep *Report) {
	// The report goes to a terminal or a test buffer; a write error has nowhere useful to go.
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

	c := rep.Config
	p("\n=== kconmon-ng scale rig: N=%d ===\n", c.N)
	p("host: %s/%s, %d CPUs, %s; heartbeat=%s, coalescing window is the controller's 200ms\n",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), c.HeartbeatInterval)
	p("CPU/heap are PROCESS-wide: the controller and %d thin agent clients share this process (see README).\n\n", c.N)

	regRate := 0.0
	if rep.ColdWall > 0 {
		regRate = float64(rep.Registered) / rep.ColdWall.Seconds()
	}
	p("cold start: %d/%d agents registered in %s (offered over %s) -> %.1f registrations/s sustained\n",
		rep.Registered, c.N, rep.ColdWall.Round(time.Millisecond), c.ColdSpread, regRate)
	p("register RPC latency: p50=%s p95=%s max=%s (retries=%d, failures=%d)\n\n",
		rep.RegP50.Round(time.Microsecond), rep.RegP95.Round(time.Microsecond),
		rep.RegMax.Round(time.Microsecond), rep.RegisterRetries, rep.RegisterFailures)

	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	tp := func(format string, a ...any) { _, _ = fmt.Fprintf(tw, format, a...) }
	tp("phase\tspan\tCPU\tCPU%%\tchanges\tbroadcasts\tcoalesce\tqueued\tcallbacks\tresubs\theap-inuse\tgoroutines\n")
	for i := range rep.Phases {
		ph := &rep.Phases[i]
		tp("%s\t%s\t%s\t%.1f%%\t%d\t%d\t%s\t%.0f\t%d\t%d\t%s\t%d\n",
			ph.Name, ph.Span.Round(time.Millisecond), ph.CPU.Round(time.Millisecond), cpuPct(ph.CPU, ph.Span),
			ph.Changes, ph.Flushes, ratio(ph.Changes, ph.Flushes), ph.Queued, ph.Callbacks, ph.Resubs,
			mib(ph.HeapInuse), ph.Goroutines)
	}
	_ = tw.Flush()

	p("\nchurn: %d restarts (deregister+register) over %s\n", rep.ChurnEvents, rep.ChurnWall.Round(time.Millisecond))
	if c.Steady > 0 && rep.SteadyWall > 0 {
		hbRate := float64(rep.Registered) / c.HeartbeatInterval.Seconds()
		p("steady: %s of heartbeats at ~%.0f/s offered (%d agents / %s)\n",
			rep.SteadyWall.Round(time.Second), hbRate, rep.Registered, c.HeartbeatInterval)
	}

	p("\npropagation (registration -> EVERY watcher holds the updated list), %d/%d probes complete",
		rep.ProbeDone, rep.ProbeCount)
	if rep.ProbeIncomplete > 0 || rep.ProbeRegisterFailures > 0 {
		p(" (INCOMPLETE=%d, register failures=%d)", rep.ProbeIncomplete, rep.ProbeRegisterFailures)
	}
	p(":\n  p50=%s p95=%s max=%s (includes the intended <=200ms coalescing delay)\n",
		rep.PropP50.Round(time.Millisecond), rep.PropP95.Round(time.Millisecond), rep.PropMax.Round(time.Millisecond))
	p("observer flush->receive delivery delay: p50=%s p95=%s (resubs=%d)\n",
		rep.DeliveryP50.Round(time.Microsecond), rep.DeliveryP95.Round(time.Microsecond), rep.ObserverResubs)

	if rep.FullSyncBytes > 0 {
		p("\nFULL_SYNC wire size at %d peers: %d bytes (%.1f bytes/peer, narrow projection)\n",
			rep.FullSyncPeers, rep.FullSyncBytes, float64(rep.FullSyncBytes)/float64(rep.FullSyncPeers))
	} else {
		p("\nFULL_SYNC size at exactly %d peers was not observed (desync/resubscribe timing)\n", rep.FullSyncPeers)
	}

	p("process peak RSS: %s\n", mib(rep.MaxRSS))
	if rep.QuiesceTimeouts > 0 {
		p("WARNING: %d phase(s) did not quiesce within %s — the control plane was still churning\n",
			rep.QuiesceTimeouts, c.QuiesceMax)
	}
	if len(rep.LogLines) > 0 {
		p("\nwarn/error log messages (count  message):\n")
		for _, l := range rep.LogLines {
			p("  %s\n", l)
		}
	}

	// One machine-greppable line per run for the roadmap table.
	cold, churn, steady := &rep.Phases[0], &rep.Phases[1], &rep.Phases[2]
	p("\nSUMMARY n=%d reg/s=%.1f cold_coalesce=%s churn_coalesce=%s prop_p95=%s fullsync_bytes=%d steady_cpu=%.1f%% heap=%s rss=%s desync_resubs=%d\n",
		c.N, regRate,
		ratio(cold.Changes, cold.Flushes),
		ratio(churn.Changes, churn.Flushes),
		rep.PropP95.Round(time.Millisecond), rep.FullSyncBytes,
		cpuPct(steady.CPU, steady.Span),
		mib(steady.HeapInuse), mib(rep.MaxRSS),
		cold.Resubs+churn.Resubs+steady.Resubs)
}
