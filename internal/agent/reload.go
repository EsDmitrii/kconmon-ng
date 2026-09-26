package agent

import (
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"regexp"
	"slices"
	"time"

	pb "github.com/EsDmitrii/kconmon-ng/api/proto"
	"github.com/EsDmitrii/kconmon-ng/internal/checker"
	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// agentHotKeys are applied by ApplyConfig without a restart.
var agentHotKeys = []string{"logLevel", "checkers."}

// agentIgnoredKeys are read by the controller only, or by nothing (mode, observability.*); the agent
// shares the ConfigMap and says nothing.
var agentIgnoredKeys = []string{"controller.", "topology.", "failureDomainLabel", "mode", "observability."}

// probeSet is everything the checkers block builds. It is published whole and never mutated.
type probeSet struct {
	checkers        map[model.CheckType]checker.Checker
	schedule        []ScheduledChecker
	mtr             *checker.MTRChecker
	external        ExternalPolicy
	externalChecker *checker.ExternalChecker
}

/*
buildProbes builds the probe set cfg.Checkers describes. On a reload prevCfg and prev are what is
running: a checker whose block did not change is reused as is, so its loop keeps its cadence and its
state (the pmtu clamp warnings, the external targets' due times, the MTR cooldowns).
*/
func buildProbes(cfg, prevCfg *config.Config, prev *probeSet) (*probeSet, error) {
	c := &cfg.Checkers
	var p config.CheckersConfig
	if prevCfg != nil {
		p = prevCfg.Checkers
	}
	reuse := func(t model.CheckType, sameBlock bool) checker.Checker {
		if prev == nil || !sameBlock {
			return nil
		}
		return prev.checkers[t]
	}
	set := &probeSet{checkers: make(map[model.CheckType]checker.Checker)}
	add := func(t model.CheckType, sc SchedulerConfig, reused checker.Checker, build func() checker.Checker) {
		ch := reused
		if ch == nil {
			ch = build()
			slog.Info("checker enabled", "type", t, "interval", sc.Interval)
		}
		set.checkers[t] = ch
		set.schedule = append(set.schedule, ScheduledChecker{Checker: ch, Config: sc})
	}

	if c.TCP.Enabled {
		add(model.CheckTCP, SchedulerConfig{Interval: c.TCP.Interval}, reuse(model.CheckTCP, c.TCP == p.TCP),
			func() checker.Checker { return checker.NewTCPChecker(c.TCP.Timeout) })
	}
	// grpcPort is restart-bound, so on a reload cfg still carries the port the echo server bound.
	samePort := prevCfg != nil && cfg.GRPCPort == prevCfg.GRPCPort
	if c.UDP.Enabled {
		add(model.CheckUDP, SchedulerConfig{Interval: c.UDP.Interval}, reuse(model.CheckUDP, samePort && c.UDP == p.UDP),
			func() checker.Checker { return checker.NewUDPChecker(c.UDP.Timeout, c.UDP.Packets, cfg.GRPCPort) })
	}
	if c.ICMP.Enabled {
		add(model.CheckICMP, SchedulerConfig{Interval: c.ICMP.Interval}, reuse(model.CheckICMP, c.ICMP == p.ICMP),
			func() checker.Checker { return checker.NewICMPChecker(c.ICMP.Timeout) })
	}
	if c.PMTU.Enabled {
		add(model.CheckPMTU, SchedulerConfig{Interval: c.PMTU.Interval}, reuse(model.CheckPMTU, samePort && c.PMTU == p.PMTU),
			func() checker.Checker {
				return checker.NewPMTUChecker(c.PMTU.Timeout, c.PMTU.Interval, c.PMTU.Size, cfg.GRPCPort)
			})
	}
	if c.DNS.Enabled && len(c.DNS.Hosts) > 0 {
		add(model.CheckDNS, SchedulerConfig{Interval: c.DNS.Interval, NodeLocal: true},
			reuse(model.CheckDNS, reflect.DeepEqual(c.DNS, p.DNS)),
			func() checker.Checker { return checker.NewDNSChecker(c.DNS.Hosts, c.DNS.Resolvers, c.DNS.Timeout) })
	}
	if c.HTTP.Enabled && len(c.HTTP.Targets) > 0 {
		reused := reuse(model.CheckHTTP, reflect.DeepEqual(c.HTTP, p.HTTP))
		var httpTargets []checker.HTTPCheckTarget
		if reused == nil {
			var err error
			if httpTargets, err = buildHTTPTargets(c.HTTP.Targets); err != nil {
				return nil, err
			}
		}
		add(model.CheckHTTP, SchedulerConfig{Interval: c.HTTP.Interval, NodeLocal: true}, reused,
			func() checker.Checker { return checker.NewHTTPChecker(c.HTTP.Timeout, httpTargets) })
	}

	if c.External.Enabled {
		if prev != nil && prev.externalChecker != nil && reflect.DeepEqual(c.External, p.External) {
			set.external, set.externalChecker = prev.external, prev.externalChecker
		} else {
			allowlist, err := checker.NewAllowlist(c.External.AllowedCIDRs, c.External.DeniedCIDRs)
			if err != nil {
				return nil, fmt.Errorf("checkers.external.%w", err)
			}
			set.external = ExternalPolicy{
				Enabled:   true,
				Allowlist: allowlist,
				// The system resolver: external destinations are named in cluster DNS terms like
				// everything else the agent probes.
				Resolver:   net.DefaultResolver,
				Timeout:    c.External.Timeout,
				MaxTargets: c.External.MaxTargets,
			}
			set.externalChecker = checker.NewExternalChecker(allowlist, net.DefaultResolver, c.External.Timeout)
			slog.Info("external destination checks enabled",
				"allowedCidrs", len(c.External.AllowedCIDRs),
				"deniedCidrs", len(c.External.DeniedCIDRs),
				"maxTargets", c.External.MaxTargets,
				"authTimeout", c.External.Timeout,
				"tick", checker.ExternalTick,
			)
		}
		set.schedule = append(set.schedule, ScheduledChecker{
			Checker: set.externalChecker,
			Config:  SchedulerConfig{Interval: checker.ExternalTick, NodeLocal: true},
		})
	}

	if prev != nil && prev.mtr != nil && c.MTR == p.MTR {
		set.mtr = prev.mtr
	} else {
		set.mtr = checker.NewMTRChecker(c.MTR.MaxHops, 1*time.Second, c.MTR.Cooldown)
		slog.Info("mtr checker enabled", "maxHops", c.MTR.MaxHops, "cooldown", c.MTR.Cooldown)
	}
	return set, nil
}

func buildHTTPTargets(targets []config.HTTPTarget) ([]checker.HTTPCheckTarget, error) {
	out := make([]checker.HTTPCheckTarget, 0, len(targets))
	for _, t := range targets {
		ht := checker.HTTPCheckTarget{
			URL:                t.URL,
			Method:             t.Method,
			ExpectStatus:       t.ExpectStatus,
			InsecureSkipVerify: t.InsecureSkipVerify,
		}
		if t.BodyPattern != "" {
			re, err := regexp.Compile(t.BodyPattern)
			if err != nil {
				return nil, fmt.Errorf("invalid bodyPattern %q for target %s: %w", t.BodyPattern, checker.RedactURL(t.URL), err)
			}
			ht.BodyPattern = re
		}
		out = append(out, ht)
	}
	return out, nil
}

// probes returns the probe set in force. The maps it carries are replaced, never modified, so the
// caller may keep reading them after the lock is released.
func (a *Agent) probes() probeSet {
	a.probeMu.RLock()
	defer a.probeMu.RUnlock()
	return probeSet{
		checkers:        a.checkers,
		mtr:             a.mtrChecker,
		external:        a.external,
		externalChecker: a.externalChecker,
	}
}

func (a *Agent) currentExternalChecker() *checker.ExternalChecker {
	a.probeMu.RLock()
	defer a.probeMu.RUnlock()
	return a.externalChecker
}

// appliedConfig is the config in force: restart-bound keys as the process started, hot keys as last
// reloaded.
func (a *Agent) appliedConfig() *config.Config {
	a.probeMu.RLock()
	defer a.probeMu.RUnlock()
	return a.cfg
}

/*
ApplyConfig is the agent's config.Loader OnChange subscriber. logLevel and the whole checkers block
are applied live: changed checkers are rebuilt and rescheduled, unchanged ones keep running, peers
and metrics carry over, and a change to the set of enabled planes is re-advertised to the controller.
Every other key the agent reads is restart-bound: one warning names the changed ones and nothing they
govern moves. Keys only the controller reads, and keys nothing reads, are ignored.
*/
func (a *Agent) ApplyConfig(next *config.Config) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()

	applied := a.appliedConfig()
	var restart []string
	for _, key := range config.Diff(applied, next) {
		if !config.KeyMatches(key, agentHotKeys...) && !config.KeyMatches(key, agentIgnoredKeys...) {
			restart = append(restart, key)
		}
	}
	if len(restart) > 0 {
		slog.Warn("config changed in keys the agent reads only at startup; restart the agent to apply them",
			"keys", restart)
	}

	config.SetLogLevel(next.LogLevel)
	effective := *applied
	effective.LogLevel = next.LogLevel
	effective.Checkers = next.Checkers
	if reflect.DeepEqual(applied.Checkers, next.Checkers) {
		a.probeMu.Lock()
		a.cfg = &effective
		a.probeMu.Unlock()
		return
	}

	prev := a.probes()
	set, err := buildProbes(&effective, applied, &prev)
	if err != nil {
		slog.Error("the reloaded checkers block cannot be applied, the running checkers stay", "error", err)
		return
	}

	// Stop what changed before anything reads the new set, so a disabled checker is silent from here on.
	a.scheduler.SetMTRChecker(set.mtr)
	a.scheduler.SetCheckers(set.schedule)

	a.probeMu.Lock()
	a.cfg = &effective
	a.checkers = set.checkers
	a.mtrChecker = set.mtr
	a.external = set.external
	a.externalChecker = set.externalChecker
	a.probeMu.Unlock()

	a.reconcileExternal(prev.externalChecker, set.externalChecker)
	for t := range prev.checkers {
		if _, still := set.checkers[t]; !still {
			a.metrics.ForgetPlane(string(t))
		}
	}
	preinitSelfMetrics(a.metrics, set.checkers, set.externalChecker != nil)
	a.peerMu.Lock()
	a.syncPeerMetrics()
	caps := agentCapabilities(&effective)
	capsChanged := !slices.Equal(caps, a.info.Capabilities)
	if capsChanged {
		a.info.Capabilities = caps
	}
	a.peerMu.Unlock()

	if capsChanged {
		select {
		case a.readvertise <- struct{}{}:
		default:
		}
	}
	slog.Info("checker config reloaded", "checkers", len(set.checkers), "external", set.externalChecker != nil,
		"capabilities", caps)
}

/*
reconcileExternal moves the continuous external assignment onto a rebuilt checker, or retires it
when external checks were switched off, and starts or stops the WatchExternalChecks subscription to
match. Caller holds reloadMu.
*/
func (a *Agent) reconcileExternal(old, cur *checker.ExternalChecker) {
	if old == cur {
		return
	}
	switch {
	case cur == nil:
		a.scheduler.whileNoDelivery(func() { retireDepartedExternalTargets(a.metrics, old, nil) })
		a.lastAssignment = nil
	case a.lastAssignment != nil:
		a.applyExternalAssignmentLocked(a.lastAssignment, old)
	}
	if a.externalWatch != nil {
		a.externalWatch(cur != nil)
	}
}

// newTaskExecutor builds the on-demand executor over the probe set in force at each task.
func (a *Agent) newTaskExecutor(reporter taskReporter) *TaskExecutor {
	p := a.probes()
	cfg := a.appliedConfig()
	e := NewTaskExecutor(
		p.checkers,
		p.mtr,
		checker.Target{
			AgentID:  a.info.ID,
			NodeName: a.info.NodeName,
			PodIP:    a.info.PodIP,
			Zone:     a.scheduler.sourceZone(),
			Port:     cfg.HTTPPort,
		},
		a.ownPorts(),
		reporter,
		maxConcurrentTasks,
		p.external,
	)
	e.zone = a.scheduler.sourceZone
	e.current = func() (map[model.CheckType]checker.Checker, *checker.MTRChecker, ExternalPolicy) {
		p := a.probes()
		return p.checkers, p.mtr, p.external
	}
	return e
}

// applyExternalAssignment is the WatchExternalChecks handler; see applyExternalAssignmentLocked.
func (a *Agent) applyExternalAssignment(assignment *pb.ExternalCheckAssignment) {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	if a.currentExternalChecker() == nil {
		// A message still in flight from a subscription a reload just stopped.
		return
	}
	a.lastAssignment = assignment
	a.applyExternalAssignmentLocked(assignment, nil)
}
