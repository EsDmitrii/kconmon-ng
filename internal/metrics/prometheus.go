package metrics //nolint:revive // intentional: "metrics" is clearer than alternatives for this package

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var defaultBuckets = []float64{
	0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0,
}

type PrometheusMetrics struct {
	prefix string
	reg    prometheus.Registerer

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
	/* ExternalSpecsRejected counts assignment entries THIS agent could not parse.
	   A definition the Console accepts and schedules but every agent refuses (checkType=http against
	   a target of kind host, checkType=dns with no params.query) was dropped with nothing but an
	   agent-local WARN: no metric, no feedback to the controller, and the Console went on showing the
	   check as enabled and healthy. It is re-dropped on every assignment push, forever, so the
	   counter climbs for as long as the definition sits there broken. */
	ExternalSpecsRejected *prometheus.CounterVec

	MTRHops      *prometheus.GaugeVec
	MTRHopRTT    *prometheus.GaugeVec
	MTRTriggered *prometheus.CounterVec

	ControllerRegisteredAgents *prometheus.GaugeVec
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

func NewPrometheusMetrics(prefix string, reg prometheus.Registerer) *PrometheusMetrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	factory := promauto.With(reg)

	peerLabels := []string{"source_node", "destination_node", "source_zone", "destination_zone"}
	resultPeerLabels := []string{"source_node", "destination_node", "source_zone", "destination_zone", "result"}

	/* target is the operator's NAME for the destination, target_kind is the closed set host|url and
	   check_type is the probe's own type; none carries an address, and both derived labels come from
	   the check rather than off the wire.

	   check_type is here because target_kind alone could not separate two checks on one target:
	   everything that is not http collapses to "host", so an icmp, a tcp and a dns check on the same
	   target wrote ONE series between them. Their successes and failures were averaged together, and
	   the ExternalChecksFailing rule -- which sums by exactly these labels -- stayed silent while one
	   of the three was failing 100%, because the other two diluted it below the threshold. */
	externalLabels := []string{"source_node", "source_zone", "target", "target_kind", "check_type"}
	resultExternalLabels := []string{"source_node", "source_zone", "target", "target_kind", "check_type", "result"}
	deniedExternalLabels := []string{"source_node", "source_zone", "target", "target_kind", "check_type", "reason"}

	m := &PrometheusMetrics{
		prefix: prefix,
		reg:    reg,

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

		ControllerRegisteredAgents: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: prefix + "_controller_registered_agents",
			Help: "Number of currently registered agents",
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

	return m
}

/*
PeerResultCounter returns the *_results_total counter for a check type whose series are keyed by a
PAIR of nodes, and nil for one whose series are not.

The caller pre-creates both result="success" and result="fail" for every peer, and it can only do
that for the check types that carry destination_node at all: tcp, udp and icmp. DNS keys on
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
	default:
		return nil
	}
}

/*
ForgetPeer drops the gauge series for ONE departed destination; a counter is cumulative and is never
dropped here.

This used to be a single ResetPeerGauges() that called Reset() on every one of these vectors,
wholesale, from the peer-update callback. Two things were wrong with that, and both showed up as
holes in exactly the series alerts fire on:

  - It wiped peers that had not gone anywhere. Nothing repopulates a gauge except the next probe of
    that pair (syncPeerMetrics pre-creates counters, not gauges), so every peer update blanked the
    fleet's loss and jitter readings for up to a full check interval. A peer update fires per pod
    add and per pod delete — a rolling DaemonSet restart is one per node, back to back.
  - It wiped the EXTERNAL gauges too, which are keyed by (source_node, source_zone, target,
    target_kind) and have no destination_node at all. An external target's packet loss vanished
    because some unrelated agent pod restarted.

DeletePartialMatch takes the label the departure is actually about and leaves every other series
standing.
*/
func (m *PrometheusMetrics) ForgetPeer(destinationNode string) {
	labels := prometheus.Labels{"destination_node": destinationNode}
	m.UDPLossRatio.DeletePartialMatch(labels)
	m.UDPJitter.DeletePartialMatch(labels)
	m.ICMPLossRatio.DeletePartialMatch(labels)
	m.MTRHops.DeletePartialMatch(labels)
	m.MTRHopRTT.DeletePartialMatch(labels)
}

// ForgetExternalTarget drops the gauge series for one target that left the controller's assignment.
// Nothing did this before: the external gauges were only ever cleared as collateral damage from a
// peer update, so a target removed from the assignment kept reporting its last reading forever if
// the peer list happened to stay still.
func (m *PrometheusMetrics) ForgetExternalTarget(target string) {
	labels := prometheus.Labels{"target": target}
	m.ExternalPacketLoss.DeletePartialMatch(labels)
	m.ExternalHTTPStatusCode.DeletePartialMatch(labels)
}

/*
ForgetExternalCheck retires ONE check's gauges: the (target, target_kind, check_type) triple, not the
name alone.

A target NAME can carry more than one check — retireDepartedExternalTargets' own comment says so: a
host probe and a URL probe share the `target` label and differ in `target_kind`. So deleting by name
is right when the target leaves the assignment entirely (every check on it goes), and wrong when a
single probe is DENIED: the allowlist refusing the icmp check took the healthy http check's
packet-loss and status-code series with it, and those came back only on that check's next successful
probe. A denial is a fact about one check.
*/
func (m *PrometheusMetrics) ForgetExternalCheck(target, targetKind, checkType string) {
	labels := prometheus.Labels{"target": target, "target_kind": targetKind, "check_type": checkType}
	m.ExternalPacketLoss.DeletePartialMatch(labels)
	m.ExternalHTTPStatusCode.DeletePartialMatch(labels)
}

/*
ForgetPeerTrace retires the per-hop RTT series of ONE (source, destination) pair.

MTRHopRTT is keyed by (source_node, destination_node, hop_number, hop_ip) and was only ever Set. A
route change therefore left BOTH paths' series live and current: after a trace through 10.0.0.5 and
a later one through 10.0.0.9, both hop_ip values sat at hop_number=3 with a fresh reading, and a
panel grouping by hop_ip drew a path the packets have not taken since. Only ForgetPeer cleared them,
and that fires when the peer leaves the topology — not when its route changes, which is the event
the whole feature exists to show.

Called before publishing a trace's hops, so the series that remain are exactly the trace just taken.
*/
func (m *PrometheusMetrics) ForgetPeerTrace(sourceNode, destinationNode string) {
	m.MTRHopRTT.DeletePartialMatch(prometheus.Labels{
		"source_node":      sourceNode,
		"destination_node": destinationNode,
	})
}
