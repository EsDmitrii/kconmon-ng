{{/* Built-in alert rules, emitted as YAML and parsed back by templates/observability/prometheusrule.yaml; see README.md. */}}

{{/* Threshold ratio as a percentage, for annotation text. */}}
{{- define "kconmon-ng.prometheusRule.pct" -}}
{{- printf "%g" (round (mulf . 100) 3) -}}
{{- end -}}

{{- define "kconmon-ng.prometheusRule.builtinRules" -}}
{{- $prefix := .Values.config.metricsPrefix -}}
{{- $pr := .Values.prometheusRule -}}
{{- with $pr.udpLossHigh }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
- alert: UDPLossHigh
  expr: {{ $prefix }}_udp_packet_loss_ratio > {{ $t }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      UDP loss {{`{{ $labels.source_node }}`}} -> {{`{{ $labels.destination_node }}`}}
      at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      UDP packet loss from {{`{{ $labels.source_node }}`}} (zone
      {{`{{ $labels.source_zone }}`}}) to {{`{{ $labels.destination_node }}`}} (zone
      {{`{{ $labels.destination_zone }}`}}) has held at
      {{`{{ $value | humanizePercentage }}`}} for {{ .for }}, over the
      {{ include "kconmon-ng.prometheusRule.pct" $t }}% threshold.
      Drill this exact pair on the kconmon-ng console Investigate page, or
      open the "kconmon-ng / Node Detail" Grafana dashboard with
      node={{`{{ $labels.source_node }}`}} to see whether the same node is losing
      packets to its other peers or only to this one.
{{- end }}
{{- end }}
{{- with $pr.tcpChecksFailing }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
- alert: TCPChecksFailing
  expr: >-
    sum by (source_node, destination_node, source_zone, destination_zone)
    (rate({{ $prefix }}_tcp_results_total{result="fail"}[5m]))
    /
    sum by (source_node, destination_node, source_zone, destination_zone)
    (rate({{ $prefix }}_tcp_results_total[5m]))
    > {{ $t }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      TCP checks failing {{`{{ $labels.source_node }}`}} ->
      {{`{{ $labels.destination_node }}`}} at {{`{{ $value | humanizePercentage }}`}} of
      probes
    description: >-
      {{`{{ $value | humanizePercentage }}`}} of TCP probes from
      {{`{{ $labels.source_node }}`}} (zone {{`{{ $labels.source_zone }}`}}) to
      {{`{{ $labels.destination_node }}`}} (zone {{`{{ $labels.destination_zone }}`}})
      failed over the last 5m, above the
      {{ include "kconmon-ng.prometheusRule.pct" $t }}% failure-ratio threshold. Open
      the pair on the kconmon-ng console Investigate page, or the worst-pairs
      table on the "kconmon-ng / Overview" Grafana dashboard to see whether
      UDP and ICMP fail on the same link (a path problem) or TCP fails
      alone (a listener or policy problem).
{{- end }}
{{- end }}
{{- with $pr.pairWentSilent }}
{{- if .enabled }}
{{/* Fires only for pairs the topology plan assigns (probe_intended == 1): under a sparse mesh
     every trimmed pair would otherwise read as "went silent" for the hour its results age out.
     The fallback joins per SOURCE, not globally: a source_node exporting NO probe_intended at all
     is a pre-2.3.0 agent (or one that stopped being scraped — the case this alert exists for),
     and its pairs keep the old two-window behaviour; a mixed fleet mid-rollout gets each
     behaviour exactly where it applies. The two halves are disjoint by construction, so the
     union never yields a duplicate label set. */}}
- alert: PairWentSilent
  expr: >-
    (
    (
    sum by (source_node, destination_node)
    (rate({{ $prefix }}_tcp_results_total[1h] offset 5m)) > 0
    unless
    sum by (source_node, destination_node)
    (rate({{ $prefix }}_tcp_results_total[5m])) > 0
    )
    and on (source_node, destination_node)
    ({{ $prefix }}_probe_intended == 1)
    )
    or
    (
    (
    sum by (source_node, destination_node)
    (rate({{ $prefix }}_tcp_results_total[1h] offset 5m)) > 0
    unless
    sum by (source_node, destination_node)
    (rate({{ $prefix }}_tcp_results_total[5m])) > 0
    )
    unless on (source_node)
    {{ $prefix }}_probe_intended
    )
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      No probe results at all from {{`{{ $labels.source_node }}`}} ->
      {{`{{ $labels.destination_node }}`}} for 15m
    description: >-
      {{`{{ $labels.source_node }}`}} was probing
      {{`{{ $labels.destination_node }}`}} within the last hour and has reported
      nothing for the last 15m, so no failure ratio can be computed for
      this link and the other rules in this group have gone quiet about it
      rather than healthy. Either the source agent stopped running or
      stopped being scraped, or the pair left the topology. Check the agent
      pod on {{`{{ $labels.source_node }}`}} and its scrape target first, then
      the controller's peer list -- a node that was drained or removed
      produces this too, and the alert clears on its own an hour after the
      last result. A pair the sparse topology plan dropped does NOT fire:
      the rule only matches pairs the source agent still marks in its
      probe_intended series, and falls back to the plain two-window
      comparison for agents that do not export that family yet. Zone
      labels are absent by design: this rule compares
      label sets across two time windows and pairs are matched on node
      names only.
{{- end }}
{{- end }}
{{- with $pr.dnsChecksFailing }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
- alert: DNSChecksFailing
  expr: >-
    sum by (source_node, source_zone, host, resolver)
    (rate({{ $prefix }}_dns_results_total{result="fail"}[5m]))
    /
    sum by (source_node, source_zone, host, resolver)
    (rate({{ $prefix }}_dns_results_total[5m]))
    > {{ $t }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      DNS failing on {{`{{ $labels.source_node }}`}} for {{`{{ $labels.host }}`}} via
      {{`{{ $labels.resolver }}`}} at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      {{`{{ $value | humanizePercentage }}`}} of lookups of {{`{{ $labels.host }}`}}
      through resolver {{`{{ $labels.resolver }}`}} from {{`{{ $labels.source_node }}`}}
      (zone {{`{{ $labels.source_zone }}`}}) failed over the last 5m, above the
      {{ include "kconmon-ng.prometheusRule.pct" $t }}%
      threshold. This is resolver-side, not a peer link, so check
      CoreDNS/kube-dns and the node's resolv.conf before the network. The
      kconmon-ng console Investigate page scoped to
      {{`{{ $labels.source_node }}`}} shows whether its peer probes degraded at
      the same moment.
{{- end }}
{{- end }}
{{- with $pr.externalChecksFailing }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
- alert: ExternalChecksFailing
  expr: >-
    sum by (source_node, source_zone, target, target_kind, check_type)
    (rate({{ $prefix }}_external_results_total{result="fail"}[5m]))
    /
    sum by (source_node, source_zone, target, target_kind, check_type)
    (rate({{ $prefix }}_external_results_total[5m]))
    > {{ $t }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      External target {{`{{ $labels.target }}`}} failing from
      {{`{{ $labels.source_node }}`}} at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      {{`{{ $value | humanizePercentage }}`}} of probes to external target
      {{`{{ $labels.target }}`}} (kind {{`{{ $labels.target_kind }}`}}) from
      {{`{{ $labels.source_node }}`}} (zone {{`{{ $labels.source_zone }}`}}) failed over
      the last 5m, above the {{ include "kconmon-ng.prometheusRule.pct" $t }}% threshold. A probe the allowlist refused
      never reaches this counter, so if the target looks untested rather
      than failing, read the external_denied_total counter for its reason
      (cidr, resolve or disabled) before suspecting the network.
{{- end }}
{{- end }}
{{- with $pr.zoneChecksFailing }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
{{/* One rule across all three protocols; a disabled checker's family is simply an empty
     branch of the union. The label_replace/or shape is load-bearing: rate() over a bare
     __name__ union drops __name__ and collapses the three families into duplicate labelsets,
     which the engine refuses at EVALUATION time ("vector cannot contain metrics with the same
     labelset") — helm template and promtool syntax checks never see it. v2.3.0 shipped that
     and every rule evaluation failed on both production clusters. */}}
- alert: ZoneChecksFailing
  expr: >-
    sum by (source_zone, destination_zone) (
    label_replace(rate({{ $prefix }}_zone_tcp_results_total{result="fail"}[5m]), "proto", "tcp", "", "")
    or label_replace(rate({{ $prefix }}_zone_udp_results_total{result="fail"}[5m]), "proto", "udp", "", "")
    or label_replace(rate({{ $prefix }}_zone_icmp_results_total{result="fail"}[5m]), "proto", "icmp", "", "")
    )
    /
    sum by (source_zone, destination_zone) (
    label_replace(rate({{ $prefix }}_zone_tcp_results_total[5m]), "proto", "tcp", "", "")
    or label_replace(rate({{ $prefix }}_zone_udp_results_total[5m]), "proto", "udp", "", "")
    or label_replace(rate({{ $prefix }}_zone_icmp_results_total[5m]), "proto", "icmp", "", "")
    )
    > {{ $t }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      Zone checks failing {{`{{ $labels.source_zone }}`}} ->
      {{`{{ $labels.destination_zone }}`}} at {{`{{ $value | humanizePercentage }}`}} of
      probes
    description: >-
      {{`{{ $value | humanizePercentage }}`}} of all TCP, UDP and ICMP probes from zone
      {{`{{ $labels.source_zone }}`}} to zone {{`{{ $labels.destination_zone }}`}} failed
      over the last 5m, above the
      {{ include "kconmon-ng.prometheusRule.pct" $t }}% threshold. This is the
      zone-level aggregate, so it keeps firing under the agent.metrics.detail
      scrape modes that drop per-pair series. Open the "kconmon-ng / Zone
      Heatmap" Grafana dashboard to see which protocol carries the failures
      and whether the whole fabric between the zones or only one direction is
      affected, then the kconmon-ng console Matrix or Investigate page to find
      the node pairs pulling the ratio up — in zone-only mode the per-pair
      evidence lives in the console, not in Prometheus.
    {{- /* Console-RELATIVE on purpose: the chart cannot know the console's external URL (ingress
         is optional), and every console parses "->" into its canonical pair arrow. Prepend your
         console origin in the notification template. */}}
    investigateUrl: >-
      /investigate?kind=zone-pair&scope={{`{{ $labels.source_zone }}`}}->{{`{{ $labels.destination_zone }}`}}
{{- end }}
{{- end }}
{{- with $pr.zoneLossHigh }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
{{/* Loss is (sent - received) / sent from the zone counters: averaging the per-pair loss-ratio
     gauges into a zone would weight an idle pair the same as a busy one, so the chart never does. */}}
- alert: ZoneLossHigh
  {{- /* Same label_replace/or shape as ZoneChecksFailing above, for the same engine-level
       reason: rate() over a __name__ union collides the udp and icmp families. */}}
  expr: >-
    (sum by (source_zone, destination_zone) (
    label_replace(rate({{ $prefix }}_zone_udp_packets_sent_total[5m]), "proto", "udp", "", "")
    or label_replace(rate({{ $prefix }}_zone_icmp_packets_sent_total[5m]), "proto", "icmp", "", "")
    )
    -
    sum by (source_zone, destination_zone) (
    label_replace(rate({{ $prefix }}_zone_udp_packets_received_total[5m]), "proto", "udp", "", "")
    or label_replace(rate({{ $prefix }}_zone_icmp_packets_received_total[5m]), "proto", "icmp", "", "")
    ))
    /
    sum by (source_zone, destination_zone) (
    label_replace(rate({{ $prefix }}_zone_udp_packets_sent_total[5m]), "proto", "udp", "", "")
    or label_replace(rate({{ $prefix }}_zone_icmp_packets_sent_total[5m]), "proto", "icmp", "", "")
    )
    > {{ $t }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      Packet loss {{`{{ $labels.source_zone }}`}} -> {{`{{ $labels.destination_zone }}`}}
      at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      UDP and ICMP probes from zone {{`{{ $labels.source_zone }}`}} to zone
      {{`{{ $labels.destination_zone }}`}} have lost
      {{`{{ $value | humanizePercentage }}`}} of their packets over the last 5m,
      above the {{ include "kconmon-ng.prometheusRule.pct" $t }}% threshold.
      The ratio is packet-weighted across every pair between the zones, so a
      single broken link is diluted here and belongs to UDPLossHigh; this
      firing means the fabric between the zones is losing traffic. Open the
      "kconmon-ng / Zone Heatmap" Grafana dashboard to see whether the loss is
      one direction or both, then the kconmon-ng console Matrix or Investigate
      page scoped to these zones for the pair-level picture.
    {{- /* Same contract as ZoneChecksFailing's investigateUrl: console-relative, "->" normalised
         by the console itself. */}}
    investigateUrl: >-
      /investigate?kind=zone-pair&scope={{`{{ $labels.source_zone }}`}}->{{`{{ $labels.destination_zone }}`}}
{{- end }}
{{- end }}
{{- with $pr.kconmonAgentsMissing }}
{{- if .enabled }}
{{/* Standbys hold no agents by design, so only the lease holder's counts are evidence. */}}
- alert: KconmonAgentsMissing
  expr: >-
    ({{ $prefix }}_controller_expected_agents
    - {{ $prefix }}_controller_registered_agents > 0)
    and ({{ $prefix }}_controller_leader == 1)
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: >-
      {{`{{ $value }}`}} kconmon-ng agent(s) missing on the leading controller
      {{`{{ $labels.instance }}`}}
    description: >-
      The leading controller on {{`{{ $labels.instance }}`}} expects one agent per
      schedulable node, and {{`{{ $value }}`}} of them have not registered for
      {{ .for }}. The usual causes are a DaemonSet that cannot schedule (taints or
      resources), crash-looping agent pods, or agent-to-controller gRPC
      being blocked. Every pair involving a missing node simply stops being
      probed, so the other rules in this group go quiet rather than firing.
      The kconmon-ng console topology view lists the nodes it does know.
{{- end }}
{{- end }}
{{- with $pr.kconmonControllerDown }}
{{- if .enabled }}
- alert: KconmonControllerDown
  expr: absent({{ $prefix }}_controller_leader == 1)
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
  annotations:
    summary: No kconmon-ng controller has reported itself leader for {{ .for }}
    description: >-
      Every controller replica is reporting leader=0, or Prometheus is
      scraping none of them. Peer lists stop being distributed, so agents
      keep probing a frozen topology and every other rule in this group
      quietly stops describing reality. Check the controller Deployment, its
      lease in the release namespace, and the controller scrape target in
      Prometheus.
{{- end }}
{{- end }}
{{- end -}}
