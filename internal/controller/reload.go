package controller

import (
	"log/slog"
	"reflect"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
	"github.com/EsDmitrii/kconmon-ng/internal/controller/meshplan"
	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// controllerHotKeys are applied by ApplyConfig without a restart.
var controllerHotKeys = []string{
	"logLevel",
	"topology.",
	"controller.agentTtl",
	"checkers.external.enabled",
	"checkers.external.allowedCidrs",
}

// controllerIgnoredKeys are read by the agents only, or by nothing (mode, observability.*); the
// controller shares the ConfigMap and says nothing about them. Both lists only keep a key out of the
// restart warning; ApplyConfig below is what applies the hot ones.
var controllerIgnoredKeys = []string{"agent.", "controllerAddress", "checkers.", "mode", "observability."}

func (c *Controller) appliedConfig() *config.Config {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg
}

/*
ApplyConfig is the controller's config.Loader OnChange subscriber. It applies live what a running
replica can change without a new listener or a new stream: logLevel, the topology (the mesh is
replanned and pushed like after any registry change), controller.agentTtl, and the external
allowlist it publishes on GET /api/v1/version. Every other key it reads is restart-bound: one warning
names the changed ones and nothing they govern moves.
*/
func (c *Controller) ApplyConfig(next *config.Config) {
	applied := c.appliedConfig()
	var restart []string
	for _, key := range config.Diff(applied, next) {
		if config.KeyMatches(key, controllerHotKeys...) || config.KeyMatches(key, controllerIgnoredKeys...) {
			continue
		}
		restart = append(restart, key)
	}
	if len(restart) > 0 {
		slog.Warn("config changed in keys the controller reads only at startup; restart the controller to apply them",
			"keys", restart)
	}

	config.SetLogLevel(next.LogLevel)
	effective := *applied
	effective.LogLevel = next.LogLevel
	effective.Topology = next.Topology
	effective.Controller.AgentTTL = next.Controller.AgentTTL
	effective.Checkers.External.Enabled = next.Checkers.External.Enabled
	effective.Checkers.External.AllowedCIDRs = next.Checkers.External.AllowedCIDRs
	c.cfgMu.Lock()
	c.cfg = &effective
	c.cfgMu.Unlock()

	if !reflect.DeepEqual(applied.Topology, next.Topology) {
		topology := next.Topology
		c.topology.Store(&topology)
		// A standby holds no agents and no plan; it plans with the stored topology once it leads.
		if c.IsLeader() {
			c.registry.WithSnapshot(func(agents []model.AgentInfo) {
				c.grpcServer.SetPeerPlan(meshplan.Build(agents, topology))
				c.grpcServer.SchedulePeerBroadcast(agents)
			})
		}
		slog.Info("topology reloaded", "mode", topology.Mode)
	}

	if ttl := next.Controller.AgentTTL; ttl != applied.Controller.AgentTTL {
		c.registry.SetTTL(ttl)
		c.setEvictEvery(ttl / 2)
		slog.Info("agent TTL reloaded", "agentTtl", ttl)
	}

	if !reflect.DeepEqual(applied.Checkers.External, effective.Checkers.External) {
		// Only when the checker is on, as at startup: an allowlist nobody probes by is not a promise.
		if next.Checkers.External.Enabled {
			c.httpServer.SetExternalAllowedCIDRs(next.Checkers.External.AllowedCIDRs)
		} else {
			c.httpServer.SetExternalAllowedCIDRs(nil)
		}
	}
}

// setEvictEvery hands the sweep period to Run, replacing one it has not picked up yet.
func (c *Controller) setEvictEvery(every time.Duration) {
	for {
		select {
		case c.evictEvery <- every:
			return
		default:
		}
		select {
		case <-c.evictEvery:
		default:
		}
	}
}
