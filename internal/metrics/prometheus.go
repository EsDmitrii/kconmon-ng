package metrics //nolint:revive // intentional: "metrics" is clearer than alternatives for this package

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/EsDmitrii/kconmon-ng/internal/config"
)

var defaultBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0,
}

type PrometheusMetrics struct {
	prefix string
	reg    prometheus.Registerer

	// BuildInfo mirrors the console's *_console_build_info: labels carry the
	// ldflags-injected version/commit, the value is always 1.
	BuildInfo *prometheus.GaugeVec

	TCPConnectDuration *prometheus.HistogramVec
	TCPTotalDuration   *prometheus.HistogramVec
	TCPResults         *prometheus.CounterVec

	UDPRtt       *prometheus.HistogramVec
	UDPJitter    *prometheus.GaugeVec
	UDPLossRatio *prometheus.GaugeVec
	UDPResults   *prometheus.CounterVec

	ICMPRtt       *prometheus.HistogramVec
	ICMPLossRatio *prometheus.GaugeVec
	ICMPResults   *prometheus.CounterVec

	// Path MTU: the largest datagram that crossed the pair on the last probe, and whether the
	// full-size one did. fail is a black hole; a reduced path counts as success.
	PMTUBytes   *prometheus.GaugeVec
	PMTUResults *prometheus.CounterVec
	// PMTUProbeBytes is the size the source probed the pair at, from its route to that peer. Write it
	// with SetPMTUProbe, which also keeps AgentPMTUProbeBytes at the max over the pairs.
	PMTUProbeBytes *prometheus.GaugeVec
	pmtuProbeMu    sync.Mutex
	pmtuProbe      map[[4]string]float64

	/* ProbeIntended is the topology PLAN, scrapable: 1 for every directed pair this agent is
	   assigned to probe. Under a sparse mesh "no results for a pair" is either a failure or the
	   plan, and only this family tells them apart; PairWentSilent joins on it. Only the two node
	   names: the join needs exactly the labels the results families group by. */
	ProbeIntended *prometheus.GaugeVec

	/* The zone family is the SECOND write of every peer probe, aggregated at the source into
	   {source_zone, destination_zone}. It exists so the per-pair family can be dropped by metric
	   relabeling at scale while zone alerts and dashboards keep their data — recording rules were
	   declined because they cut query cost, not scrape cardinality. Loss is counters ONLY:
	   averaging per-pair ratio gauges weights every pair equally regardless of traffic, so
	   sum(rate(received))/sum(rate(sent)) is the only honest zone-level loss. Zones outlive
	   peers: nothing ever deletes these series (see ForgetPeer). */
	ZoneTCPConnect *prometheus.HistogramVec
	ZoneTCPTotal   *prometheus.HistogramVec
	ZoneTCPResults *prometheus.CounterVec

	ZoneUDPRtt             *prometheus.HistogramVec
	ZoneUDPResults         *prometheus.CounterVec
	ZoneUDPPacketsSent     *prometheus.CounterVec
	ZoneUDPPacketsReceived *prometheus.CounterVec

	ZoneICMPRtt             *prometheus.HistogramVec
	ZoneICMPResults         *prometheus.CounterVec
	ZoneICMPPacketsSent     *prometheus.CounterVec
	ZoneICMPPacketsReceived *prometheus.CounterVec

	ZonePMTUResults *prometheus.CounterVec

	DNSDuration *prometheus.HistogramVec
	DNSResults  *prometheus.CounterVec

	HTTPDNSDuration     *prometheus.HistogramVec
	HTTPConnectDuration *prometheus.HistogramVec
	HTTPTLSDuration     *prometheus.HistogramVec
	HTTPTTFBDuration    *prometheus.HistogramVec
	HTTPTotalDuration   *prometheus.HistogramVec
	HTTPResults         *prometheus.CounterVec

	// The external family is labelled by the operator's target NAME, never by its address.
	ExternalDuration       *prometheus.HistogramVec
	ExternalRtt            *prometheus.HistogramVec
	ExternalPacketLoss     *prometheus.GaugeVec
	ExternalResults        *prometheus.CounterVec
	ExternalHTTPStatusCode *prometheus.GaugeVec
	ExternalDenied         *prometheus.CounterVec
	/* ExternalSpecsRejected counts assignment entries THIS agent could not parse: a definition the
	   Console schedules but every agent refuses (checkType=http against a host target, dns without
	   params.query) otherwise shows only as an agent-local WARN while the Console calls it healthy.
	   It is re-dropped on every assignment push, so the counter climbs while the definition stays
	   broken. */
	ExternalSpecsRejected *prometheus.CounterVec

	MTRHops      *prometheus.GaugeVec
	MTRHopRTT    *prometheus.GaugeVec
	MTRTriggered *prometheus.CounterVec

	/* The agent's SELF-observation family: how the probing machinery itself behaves, not what it
	   measures. Cardinality is O(enabled checkers), never O(peers), so these stay ~a dozen series on
	   any fleet size. The controller binary shares this struct; unused Vecs export nothing there. */
	AgentProbeCycleDuration *prometheus.HistogramVec
	AgentProbeCycleOverruns *prometheus.CounterVec
	// AgentControllerReconnects counts re-registrations forced by a lost
	// controller stream or a heartbeat rejection, not the initial registration.
	AgentControllerReconnects *prometheus.CounterVec
	AgentMTRReactiveInflight  *prometheus.GaugeVec
	// AgentMTRReactiveCoalesced counts failed probes that started NO trace,
	// by reason: "cooldown" (a trace for that destination already ran or runs
	// inside the cooldown window) or "saturated" (all reactive-trace slots busy).
	AgentMTRReactiveCoalesced *prometheus.CounterVec
	// AgentPMTUProbeBytes is the largest size this agent probes any peer at: the one probe-size series
	// agent.metrics.detail=zone-only keeps. A reduced path is told per pair, against PMTUProbeBytes.
	AgentPMTUProbeBytes *prometheus.GaugeVec

	ControllerRegisteredAgents *prometheus.GaugeVec
	// ControllerExternalAgents is the bare-host subset of registered agents. KconmonAgentsMissing
	// subtracts it: registered counts external agents too, so one of them masked one missing node.
	ControllerExternalAgents   *prometheus.GaugeVec
	ControllerExpectedAgents   *prometheus.GaugeVec
	ControllerPeerUpdates      *prometheus.CounterVec
	ControllerGRPCConnections  *prometheus.GaugeVec
	ControllerLeader           *prometheus.GaugeVec
	ControllerDiagnostics      *prometheus.CounterVec
	ControllerEventSubscribers *prometheus.GaugeVec
	ControllerEventsPublished  *prometheus.CounterVec

	ControllerExternalSubscribers *prometheus.GaugeVec
	ControllerExternalAssignments *prometheus.GaugeVec
}

// peerLabels is the label order of every per-pair family, and of a pmtuProbe key.
var peerLabels = []string{"source_node", "destination_node", "source_zone", "destination_zone"}

func NewPrometheusMetrics(prefix string, reg prometheus.Registerer) *PrometheusMetrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	factory := promauto.With(reg)

	resultPeerLabels := []string{"source_node", "destination_node", "source_zone", "destination_zone", "result"}

	/* An agent without a zone carries source_zone="" on its per-pair series, and the zone family
	   mirrors that verbatim: a placeholder minted only here would make the aggregate disagree
	   with the very family it aggregates. */
	zoneLabels := []string{"source_zone", "destination_zone"}
	resultZoneLabels := []string{"source_zone", "destination_zone", "result"}

	/* target is the operator's NAME for the destination, target_kind is the closed set host|url and
	   check_type is the probe's own type; none carries an address, and both derived labels come from
	   the check rather than off the wire.

	   check_type separates checks that share a target: every check but http has target_kind host, so
	   without it an icmp, a tcp and a dns check on one target would write one series, and
	   ExternalChecksFailing, which sums by exactly these labels, would dilute a failing check with
	   healthy ones. */
	externalLabels := []string{"source_node", "source_zone", "target", "target_kind", "check_type"}
	resultExternalLabels := []string{"source_node", "source_zone", "target", "target_kind", "check_type", "result"}
	deniedExternalLabels := []string{"source_node", "source_zone", "target", "target_kind", "check_type", "reason"}

	m := &PrometheusMetrics{
		prefix: prefix,
		reg:    reg,

		BuildInfo: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_build_info",
			Help: "Build info; value is always 1.",
		}, []string{"version", "commit"}),

		TCPConnectDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_tcp_connect_duration_seconds",
			Help:    "TCP connect time in seconds",
			Buckets: defaultBuckets,
		}, peerLabels),
		TCPTotalDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_tcp_total_duration_seconds",
			Help:    "Total TCP probe round-trip time in seconds",
			Buckets: defaultBuckets,
		}, peerLabels),
		TCPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_tcp_results_total",
			Help: "Total TCP probe results",
		}, resultPeerLabels),

		UDPRtt: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_udp_rtt_seconds",
			Help:    "UDP round-trip time in seconds",
			Buckets: defaultBuckets,
		}, peerLabels),
		UDPJitter: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_udp_jitter_seconds",
			Help: "UDP inter-packet delay variation in seconds",
		}, peerLabels),
		UDPLossRatio: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_udp_packet_loss_ratio",
			Help: "UDP packet loss ratio (0.0-1.0)",
		}, peerLabels),
		UDPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_udp_results_total",
			Help: "Total UDP probe results",
		}, resultPeerLabels),

		ICMPRtt: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_icmp_rtt_seconds",
			Help:    "ICMP round-trip time in seconds",
			Buckets: defaultBuckets,
		}, peerLabels),
		ICMPLossRatio: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_icmp_packet_loss_ratio",
			Help: "ICMP packet loss ratio (0.0-1.0)",
		}, peerLabels),
		ICMPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_icmp_results_total",
			Help: "Total ICMP probe results",
		}, resultPeerLabels),
		PMTUBytes: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_pmtu_bytes",
			Help: "Largest IP datagram in bytes that crossed the pair on the last path MTU probe",
		}, peerLabels),
		PMTUResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_pmtu_results_total",
			Help: "Total path MTU probe results; fail means full-size datagrams are lost with no ICMP frag-needed (a black hole)",
		}, resultPeerLabels),
		PMTUProbeBytes: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_pmtu_probe_bytes",
			Help: "IP datagram size in bytes the source probes the pair's path MTU at: the MTU of its route to the peer (the route's mtu, never above the egress device's) or checkers.pmtu.size",
		}, peerLabels),
		pmtuProbe: map[[4]string]float64{},

		ProbeIntended: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_probe_intended",
			Help: "1 for every directed pair the topology plan assigns this agent to probe; absent means unplanned, never failing",
		}, []string{"source_node", "destination_node"}),

		ZoneTCPConnect: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_zone_tcp_connect_seconds",
			Help:    "TCP connect time in seconds, aggregated per zone pair",
			Buckets: defaultBuckets,
		}, zoneLabels),
		ZoneTCPTotal: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_zone_tcp_total_seconds",
			Help:    "Total TCP probe round-trip time in seconds, aggregated per zone pair",
			Buckets: defaultBuckets,
		}, zoneLabels),
		ZoneTCPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_tcp_results_total",
			Help: "Total TCP probe results, aggregated per zone pair",
		}, resultZoneLabels),

		ZoneUDPRtt: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_zone_udp_rtt_seconds",
			Help:    "UDP round-trip time in seconds, aggregated per zone pair",
			Buckets: defaultBuckets,
		}, zoneLabels),
		ZoneUDPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_udp_results_total",
			Help: "Total UDP probe results, aggregated per zone pair",
		}, resultZoneLabels),
		ZoneUDPPacketsSent: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_udp_packets_sent_total",
			Help: "Total UDP probe packets put on the wire, per zone pair; loss = 1 - received/sent",
		}, zoneLabels),
		ZoneUDPPacketsReceived: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_udp_packets_received_total",
			Help: "Total UDP probe packets answered, per zone pair",
		}, zoneLabels),

		ZoneICMPRtt: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_zone_icmp_rtt_seconds",
			Help:    "ICMP round-trip time in seconds, aggregated per zone pair",
			Buckets: defaultBuckets,
		}, zoneLabels),
		ZoneICMPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_icmp_results_total",
			Help: "Total ICMP probe results, aggregated per zone pair",
		}, resultZoneLabels),
		ZoneICMPPacketsSent: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_icmp_packets_sent_total",
			Help: "Total ICMP echo requests put on the wire, per zone pair; loss = 1 - received/sent",
		}, zoneLabels),
		ZoneICMPPacketsReceived: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_icmp_packets_received_total",
			Help: "Total ICMP echo replies received, per zone pair",
		}, zoneLabels),
		ZonePMTUResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_zone_pmtu_results_total",
			Help: "Total path MTU probe results, aggregated per zone pair",
		}, resultZoneLabels),

		DNSDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_dns_duration_seconds",
			Help:    "DNS resolution duration in seconds",
			Buckets: defaultBuckets,
		}, []string{"host", "resolver", "source_node", "source_zone"}),
		DNSResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_dns_results_total",
			Help: "Total DNS resolution results",
		}, []string{"host", "resolver", "source_node", "source_zone", "result"}),

		HTTPDNSDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_http_dns_duration_seconds",
			Help:    "HTTP check DNS resolution phase duration",
			Buckets: defaultBuckets,
		}, []string{"url", "source_node", "source_zone"}),
		HTTPConnectDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_http_connect_duration_seconds",
			Help:    "HTTP check TCP connect phase duration",
			Buckets: defaultBuckets,
		}, []string{"url", "source_node", "source_zone"}),
		HTTPTLSDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_http_tls_duration_seconds",
			Help:    "HTTP check TLS handshake phase duration",
			Buckets: defaultBuckets,
		}, []string{"url", "source_node", "source_zone"}),
		HTTPTTFBDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_http_ttfb_seconds",
			Help:    "HTTP check time to first byte",
			Buckets: defaultBuckets,
		}, []string{"url", "source_node", "source_zone"}),
		HTTPTotalDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_http_total_duration_seconds",
			Help:    "HTTP check total duration",
			Buckets: defaultBuckets,
		}, []string{"url", "source_node", "source_zone"}),
		HTTPResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_http_results_total",
			Help: "Total HTTP check results",
		}, []string{"url", "method", "status_code", "source_node", "source_zone", "result"}),

		// Every Vec below stays EMPTY until an external probe reports, and an empty Vec collects nothing.
		ExternalDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_external_duration_seconds",
			Help:    "External check probe duration in seconds",
			Buckets: defaultBuckets,
		}, externalLabels),
		ExternalRtt: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    prefix + "_external_rtt_seconds",
			Help:    "External check round-trip time in seconds",
			Buckets: defaultBuckets,
		}, externalLabels),
		ExternalPacketLoss: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_external_packet_loss_ratio",
			Help: "External check packet loss ratio (0.0-1.0)",
		}, externalLabels),
		ExternalResults: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_external_results_total",
			Help: "Total external check results that reached the network",
		}, resultExternalLabels),
		ExternalHTTPStatusCode: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_external_http_status_code",
			Help: "Last HTTP status code returned by an external http check",
		}, externalLabels),
		ExternalDenied: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_external_denied_total",
			Help: "Total external probes refused by the allowlist, by reason (cidr|resolve|disabled)",
		}, deniedExternalLabels),
		ExternalSpecsRejected: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_external_specs_rejected_total",
			Help: "Total external check specs this agent refused to parse, by check type",
		}, []string{"source_node", "check_type"}),

		MTRHops: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_mtr_hops",
			Help: "Number of hops in last MTR trace",
		}, peerLabels),
		MTRHopRTT: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_mtr_hop_rtt_seconds",
			Help: "RTT per hop in MTR trace",
		}, []string{"source_node", "destination_node", "hop_number", "hop_ip"}),
		MTRTriggered: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_mtr_triggered_total",
			Help: "Number of times MTR was triggered",
		}, peerLabels),

		AgentProbeCycleDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name: prefix + "_agent_probe_cycle_duration_seconds",
			Help: "Wall-clock duration of one checker's full probe round over all peers",
			// Not defaultBuckets: a round is many probes, and an overrunning one
			// is the reading that matters — the range must reach past the 5s
			// interval, not resolve sub-millisecond probes.
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"checker"}),
		AgentProbeCycleOverruns: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_agent_probe_cycle_overruns_total",
			Help: "Probe rounds that took longer than the checker's configured interval",
		}, []string{"checker"}),
		AgentControllerReconnects: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_agent_controller_reconnects_total",
			Help: "Times the agent lost its controller stream and entered re-registration",
		}, []string{}),
		AgentMTRReactiveInflight: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_agent_mtr_reactive_inflight",
			Help: "Reactive MTR traces currently running",
		}, []string{}),
		AgentMTRReactiveCoalesced: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_agent_mtr_reactive_coalesced_total",
			Help: "Failed probes that triggered no new reactive MTR trace, by reason (cooldown|saturated)",
		}, []string{"reason"}),
		AgentPMTUProbeBytes: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_agent_pmtu_probe_bytes",
			Help: "Largest IP datagram size in bytes this agent probes the path MTU at, the max over its peers; compare pmtu_bytes with the per-pair pmtu_probe_bytes instead",
		}, []string{"source_node"}),

		ControllerRegisteredAgents: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_registered_agents",
			Help: "Number of currently registered agents",
		}, []string{}),
		ControllerExternalAgents: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_external_agents",
			Help: "Number of registered agents running outside the cluster (external gateway)",
		}, []string{}),
		ControllerExpectedAgents: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_expected_agents",
			Help: "Number of schedulable nodes expected to run an agent",
		}, []string{}),
		ControllerPeerUpdates: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_controller_peer_updates_total",
			Help: "Total peer list updates sent",
		}, []string{}),
		ControllerGRPCConnections: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_grpc_connections",
			Help: "Current number of gRPC connections",
		}, []string{}),
		ControllerLeader: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_leader",
			Help: "1 if this instance is the leader, 0 otherwise",
		}, []string{}),
		ControllerDiagnostics: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_controller_diagnostics_total",
			Help: "Total on-demand diagnostics requests by check type and outcome",
		}, []string{"type", "result"}),
		ControllerEventSubscribers: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_event_subscribers",
			Help: "Number of active Console WatchEvents subscriptions on this controller replica",
		}, []string{}),
		ControllerEventsPublished: factory.NewCounterVec(prometheus.CounterOpts{
			Name: prefix + "_controller_events_published_total",
			Help: "Total controller domain events published to WatchEvents subscribers, by type",
		}, []string{"type"}),
		ControllerExternalSubscribers: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_external_subscribers",
			Help: "Number of active agent WatchExternalChecks subscriptions on this controller replica",
		}, []string{}),
		// Agents, never specs, and deliberately unlabelled: a per-agent series
		// here would grow with the cluster for no operational gain.
		ControllerExternalAssignments: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_external_assignments",
			Help: "Number of agents with a non-empty continuous external-check assignment",
		}, []string{}),
	}

	// Populated here, not in cmd/main: both binaries create their registries
	// inside their own New(), and the ldflags land in internal/config anyway.
	m.BuildInfo.WithLabelValues(config.Version, config.Commit).Set(1)

	return m
}

/*
EnablePeerListAge registers the agent_peer_list_age_seconds gauge, computed at scrape time from the
last peer-list update the caller reports.

A GaugeFunc rather than a Set-on-update gauge, because the reading is an AGE: a value written once
at update time is correct for exactly one instant and then serves a stale number on every scrape —
the very failure mode (an agent quietly cut off from its controller) this series exists to expose.
It is a method, not part of NewPrometheusMetrics, because a GaugeFunc always exports: registered
unconditionally it would publish a meaningless age from the controller binary, which shares this
struct. Before the first update the age is measured from arming, i.e. process start — "we have
never had a peer list for N seconds" is exactly what an operator should see then.
*/
func (m *PrometheusMetrics) EnablePeerListAge(lastUpdate func() time.Time) {
	armedAt := time.Now()
	promauto.With(m.reg).NewGaugeFunc(prometheus.GaugeOpts{
		Name: m.prefix + "_agent_peer_list_age_seconds",
		Help: "Seconds since the last peer list update from the controller (process age until the first one)",
	}, func() float64 {
		t := lastUpdate()
		if t.IsZero() {
			t = armedAt
		}
		return time.Since(t).Seconds()
	})
}

/*
PeerResultCounter returns the *_results_total counter for a check type whose series are keyed by a
PAIR of nodes, and nil for one whose series are not.

The caller pre-creates both result="success" and result="fail" for every peer, and it can only do
that for the check types that carry destination_node at all: tcp, udp, icmp and pmtu. DNS keys on
host/resolver and HTTP on url — a pair pre-init means nothing there, and MTR is not in the checker
map to begin with.
*/
func (m *PrometheusMetrics) PeerResultCounter(checkType string) *prometheus.CounterVec {
	switch checkType {
	case "tcp":
		return m.TCPResults
	case "udp":
		return m.UDPResults
	case "icmp":
		return m.ICMPResults
	case "pmtu":
		return m.PMTUResults
	default:
		return nil
	}
}

// ZoneResultCounter is PeerResultCounter's zone-family twin: only the pair-probing check types
// aggregate into zones, everything else returns nil.
func (m *PrometheusMetrics) ZoneResultCounter(checkType string) *prometheus.CounterVec {
	switch checkType {
	case "tcp":
		return m.ZoneTCPResults
	case "udp":
		return m.ZoneUDPResults
	case "icmp":
		return m.ZoneICMPResults
	case "pmtu":
		return m.ZonePMTUResults
	default:
		return nil
	}
}

// ZonePacketCounters returns the (sent, received) pair for a check type that counts packets on the
// wire; tcp is a single connect, not a packet train, so inventing a packet counter for it would
// fabricate a loss signal.
func (m *PrometheusMetrics) ZonePacketCounters(checkType string) (sent, received *prometheus.CounterVec) {
	switch checkType {
	case "udp":
		return m.ZoneUDPPacketsSent, m.ZoneUDPPacketsReceived
	case "icmp":
		return m.ZoneICMPPacketsSent, m.ZoneICMPPacketsReceived
	default:
		return nil, nil
	}
}

/*
ForgetPeer drops the gauge series of ONE departed destination; its counters and histograms wait for
RetirePeer. Every other pair keeps its gauges, since nothing but the next probe of a pair writes one
again, and so do the external gauges, which carry no destination_node.

The zone family is deliberately absent here: zones outlive peers, and its cumulative counters feed
the rate() expressions zone alerts evaluate, so deleting them on peer churn would reset those series
once per pod event.
*/
func (m *PrometheusMetrics) ForgetPeer(destinationNode string) {
	labels := prometheus.Labels{"destination_node": destinationNode}
	m.forgetPairGauges(labels)
	m.MTRHopRTT.DeletePartialMatch(labels)
	// The plan gauge goes with the peer: a departed destination is by definition no longer
	// assigned, and a stale 1 here keeps PairWentSilent armed for a pair nothing probes.
	m.ProbeIntended.DeletePartialMatch(labels)
}

// ForgetPlane drops the gauges of a probe plane a config reload switched off. Nothing writes them
// again, so a last loss ratio or path MTU would keep serving, and alerting, as a current reading.
// Counters and histograms stay: they only stop growing.
func (m *PrometheusMetrics) ForgetPlane(checkType string) {
	switch checkType {
	case "udp":
		m.UDPLossRatio.Reset()
		m.UDPJitter.Reset()
	case "icmp":
		m.ICMPLossRatio.Reset()
	case "pmtu":
		m.PMTUBytes.Reset()
		m.pmtuProbeMu.Lock()
		m.PMTUProbeBytes.Reset()
		m.AgentPMTUProbeBytes.Reset()
		clear(m.pmtuProbe)
		m.pmtuProbeMu.Unlock()
	}
}

// RetireExternalTarget drops every series of a target that left the assignment: gauges, counters and
// histograms alike.
func (m *PrometheusMetrics) RetireExternalTarget(target string) {
	labels := prometheus.Labels{"target": target}
	m.ExternalPacketLoss.DeletePartialMatch(labels)
	m.ExternalHTTPStatusCode.DeletePartialMatch(labels)
	m.ExternalDuration.DeletePartialMatch(labels)
	m.ExternalRtt.DeletePartialMatch(labels)
	m.ExternalResults.DeletePartialMatch(labels)
	m.ExternalDenied.DeletePartialMatch(labels)
}

// ForgetExternalCheck retires ONE check's gauges, keyed by (target, target_kind, check_type): a target
// name can carry several checks, and a denial is a fact about one of them. Deleting by name would drop
// a healthy sibling's packet-loss and status-code series until its next probe.
func (m *PrometheusMetrics) ForgetExternalCheck(target, targetKind, checkType string) {
	labels := prometheus.Labels{"target": target, "target_kind": targetKind, "check_type": checkType}
	m.ExternalPacketLoss.DeletePartialMatch(labels)
	m.ExternalHTTPStatusCode.DeletePartialMatch(labels)
}

// ForgetPeerTrace retires the per-hop RTT series of ONE (source, destination) pair. It runs before a
// trace's hops are published, so the series left are exactly that trace: after a route change the old
// path's hop_ip series would otherwise stay live next to the new one.
func (m *PrometheusMetrics) ForgetPeerTrace(sourceNode, destinationNode string) {
	m.MTRHopRTT.DeletePartialMatch(prometheus.Labels{
		"source_node":      sourceNode,
		"destination_node": destinationNode,
	})
}

// RetirePeer drops the per-pair counters and histograms of a destination that is gone for good
// (ForgetPeer drops the gauges). The zone family stays: zones outlive peers.
func (m *PrometheusMetrics) RetirePeer(destinationNode string) {
	m.retirePairs(prometheus.Labels{"destination_node": destinationNode})
}

// RetirePeerZone drops every series a destination has under a zone it no longer has, gauges,
// counters and histograms alike: the pair goes on under the new zone, and nothing writes the old
// series again.
func (m *PrometheusMetrics) RetirePeerZone(destinationNode, destinationZone string) {
	labels := prometheus.Labels{"destination_node": destinationNode, "destination_zone": destinationZone}
	m.forgetPairGauges(labels)
	m.retirePairs(labels)
}

// RetireSourceZone is RetirePeerZone for the zone this agent just left, and also takes its DNS, HTTP
// and external series, which carry the source zone too.
func (m *PrometheusMetrics) RetireSourceZone(sourceNode, sourceZone string) {
	labels := prometheus.Labels{"source_node": sourceNode, "source_zone": sourceZone}
	m.forgetPairGauges(labels)
	m.ExternalPacketLoss.DeletePartialMatch(labels)
	m.ExternalHTTPStatusCode.DeletePartialMatch(labels)
	m.retirePairs(labels)
	for _, c := range []*prometheus.CounterVec{m.DNSResults, m.HTTPResults, m.ExternalResults, m.ExternalDenied} {
		c.DeletePartialMatch(labels)
	}
	for _, h := range []*prometheus.HistogramVec{
		m.DNSDuration, m.HTTPDNSDuration, m.HTTPConnectDuration, m.HTTPTLSDuration, m.HTTPTTFBDuration,
		m.HTTPTotalDuration, m.ExternalDuration, m.ExternalRtt,
	} {
		h.DeletePartialMatch(labels)
	}
}

func (m *PrometheusMetrics) retirePairs(labels prometheus.Labels) {
	for _, c := range []*prometheus.CounterVec{m.TCPResults, m.UDPResults, m.ICMPResults, m.PMTUResults, m.MTRTriggered} {
		c.DeletePartialMatch(labels)
	}
	for _, h := range []*prometheus.HistogramVec{m.TCPConnectDuration, m.TCPTotalDuration, m.UDPRtt, m.ICMPRtt} {
		h.DeletePartialMatch(labels)
	}
}

// forgetPairGauges drops the per-pair gauges, the ones carrying all four peer labels, that match
// labels, probe sizes included.
func (m *PrometheusMetrics) forgetPairGauges(labels prometheus.Labels) {
	for _, g := range []*prometheus.GaugeVec{m.UDPLossRatio, m.UDPJitter, m.ICMPLossRatio, m.PMTUBytes, m.MTRHops} {
		g.DeletePartialMatch(labels)
	}
	m.forgetPMTUProbe(labels)
}

// SetPMTUProbe records the size one pair was probed at, labels in PMTUProbeBytes order, and sets the
// source's AgentPMTUProbeBytes to the max over its pairs.
func (m *PrometheusMetrics) SetPMTUProbe(labels []string, size float64) {
	m.pmtuProbeMu.Lock()
	defer m.pmtuProbeMu.Unlock()
	m.PMTUProbeBytes.WithLabelValues(labels...).Set(size)
	m.pmtuProbe[[4]string(labels)] = size
	m.refreshAgentPMTUProbeLocked(labels[0])
}

// forgetPMTUProbe drops the per-pair probe sizes matching every label in match and recomputes the
// per-agent max of each source it touched.
func (m *PrometheusMetrics) forgetPMTUProbe(match prometheus.Labels) {
	m.pmtuProbeMu.Lock()
	defer m.pmtuProbeMu.Unlock()
	m.PMTUProbeBytes.DeletePartialMatch(match)
	touched := map[string]struct{}{}
	for key := range m.pmtuProbe {
		if pmtuProbeMatches(key, match) {
			delete(m.pmtuProbe, key)
			touched[key[0]] = struct{}{}
		}
	}
	for source := range touched {
		m.refreshAgentPMTUProbeLocked(source)
	}
}

// pmtuProbeMatches reports whether key carries every label in match; a label that is not a peer
// label never matches.
func pmtuProbeMatches(key [4]string, match prometheus.Labels) bool {
	matched := 0
	for i, name := range peerLabels {
		if value, ok := match[name]; ok {
			if key[i] != value {
				return false
			}
			matched++
		}
	}
	return matched == len(match)
}

func (m *PrometheusMetrics) refreshAgentPMTUProbeLocked(source string) {
	biggest, found := 0.0, false
	for key, size := range m.pmtuProbe {
		if key[0] == source && (!found || size > biggest) {
			biggest, found = size, true
		}
	}
	if found {
		m.AgentPMTUProbeBytes.WithLabelValues(source).Set(biggest)
		return
	}
	m.AgentPMTUProbeBytes.DeleteLabelValues(source)
}
