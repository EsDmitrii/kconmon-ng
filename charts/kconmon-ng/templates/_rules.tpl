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
- alert: PairWentSilent
  expr: >-
    sum by (source_node, destination_node)
    (rate({{ $prefix }}_tcp_results_total[1h] offset 5m)) > 0
    unless
    sum by (source_node, destination_node)
    (rate({{ $prefix }}_tcp_results_total[5m])) > 0
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
      last result. Zone labels are absent by design: this rule compares
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
