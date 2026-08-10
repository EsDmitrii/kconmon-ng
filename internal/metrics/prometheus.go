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

	// target is the operator's NAME for the destination and target_kind is the closed set host|url;
	// neither carries an address, and target_kind is derived from the check type rather than taken off
	// the wire.
	externalLabels := []string{"source_node", "source_zone", "target", "target_kind"}
	resultExternalLabels := []string{"source_node", "source_zone", "target", "target_kind", "result"}
	deniedExternalLabels := []string{"source_node", "source_zone", "target", "target_kind", "reason"}

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

// ResetPeerGauges drops every gauge whose series is keyed by a destination that can DISAPPEAR; a
// counter is cumulative and is never reset here.
func (m *PrometheusMetrics) ResetPeerGauges() {
	m.UDPLossRatio.Reset()
	m.UDPJitter.Reset()
	m.ICMPLossRatio.Reset()
	m.MTRHops.Reset()
	m.MTRHopRTT.Reset()
	m.ExternalPacketLoss.Reset()
	m.ExternalHTTPStatusCode.Reset()
}
