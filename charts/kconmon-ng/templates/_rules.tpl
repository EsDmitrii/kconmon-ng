{{/* Built-in alert rules, emitted as YAML and parsed back by templates/observability/prometheusrule.yaml; see README.md. */}}

{{/* Threshold ratio as a percentage, for annotation text. */}}
{{- define "kconmon-ng.prometheusRule.pct" -}}
{{- printf "%g" (round (mulf . 100) 3) -}}
{{- end -}}

{{- define "kconmon-ng.prometheusRule.runbook" -}}
https://esdmitrii.github.io/kconmon-ng/reference/alerts/#{{ lower . }}
{{- end -}}

{{/* A rule block over its defaults, as JSON for fromJson. `helm upgrade --reuse-values` renders the
     templates over the OLD release's values, where a block added since is absent, so a block new in
     a release carries its values.yaml defaults here too. Per key and not sprig merge: merge
     overwrites a user's `enabled: false` with the default true. */}}
{{- define "kconmon-ng.prometheusRule.block" -}}
{{- $out := dict -}}
{{- range $k, $v := .defaults }}{{ $_ := set $out $k $v }}{{ end -}}
{{- range $k, $v := (.block | default dict) }}{{ $_ := set $out $k $v }}{{ end -}}
{{- toJson $out -}}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      UDP loss {{`{{ $labels.source_node }}`}} -> {{`{{ $labels.destination_node }}`}}
      at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      Check whether {{`{{ $labels.source_node }}`}} loses packets to its other peers too or only to
      this one.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "UDPLossHigh" }}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      TCP checks failing {{`{{ $labels.source_node }}`}} ->
      {{`{{ $labels.destination_node }}`}} at {{`{{ $value | humanizePercentage }}`}} of
      probes
    description: >-
      If UDP and ICMP fail on the same pair, suspect the path; if only TCP fails, a listener or
      network policy.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "TCPChecksFailing" }}
{{- end }}
{{- end }}
{{- with include "kconmon-ng.prometheusRule.block" (dict "block" $pr.pathMtuBlackHole "defaults" (dict "enabled" true "threshold" 0.5 "sustainedThreshold" 0.1 "for" "5m" "severity" "warning")) | fromJson }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
{{- $st := float64 .sustainedThreshold }}
{{/* The value is the path MTU itself, not the ratio: `and on` keeps the left side, so the alert
     says how many bytes still cross. It is the window's minimum because behind ECMP the gauge flips
     back to the full size whenever the last probe took a good path. unreachable probes write no pmtu
     series, so a peer that is down leaves both counters flat, the ratio is 0/0 and nothing fires.
     The second arm is that ECMP case: each probe dials a fresh source port, so one bad next hop of N
     fails about 1/N of probes for good. A lossy path fakes a black hole only when one probe loses
     the full size three times, far below 10% until UDPLossHigh already pages that path. The floor of
     two losses in 30m is for windows with few probes (a new pair, a pmtu interval of minutes), where
     a single loss alone reads above 10%. */}}
- alert: PathMTUBlackHole
  expr: >-
    min by (source_node, destination_node, source_zone, destination_zone) (min_over_time({{ $prefix }}_pmtu_bytes[10m]))
    and on (source_node, destination_node, source_zone, destination_zone)
    (
      (
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate({{ $prefix }}_pmtu_results_total{result="fail"}[10m]))
        /
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate({{ $prefix }}_pmtu_results_total[10m]))
        > {{ $t }}
      )
      or
      (
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate({{ $prefix }}_pmtu_results_total{result="fail"}[30m]))
        /
        sum by (source_node, destination_node, source_zone, destination_zone)
        (rate({{ $prefix }}_pmtu_results_total[30m]))
        > {{ $st }}
        and on (source_node, destination_node, source_zone, destination_zone)
        sum by (source_node, destination_node, source_zone, destination_zone)
        (increase({{ $prefix }}_pmtu_results_total{result="fail"}[30m]))
        >= 2
        and on (source_node, destination_node, source_zone, destination_zone)
        sum by (source_node, destination_node, source_zone, destination_zone)
        (increase({{ $prefix }}_pmtu_results_total{result="fail"}[10m]))
        > 0
      )
    )
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      Path MTU black hole {{`{{ $labels.source_node }}`}} -> {{`{{ $labels.destination_node }}`}}:
      only packets up to {{`{{ $value }}`}} bytes get through
    description: >-
      Larger packets are dropped without ICMP frag-needed, so big transfers stall while pings pass.
      Compare the MTU of the interfaces on {{`{{ $labels.source_node }}`}} with the CNI overhead.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "PathMTUBlackHole" }}
{{/* The same verdict from the zone family, for scrapes that drop every per-pair series
     (agent.metrics.detail=zone-only, or a hand-written relabel). `unless` keeps it quiet wherever
     per-pair pmtu series exist, so it never doubles PathMTUBlackHole. */}}
- alert: ZonePathMTUBlackHole
  expr: >-
    (
      (
        sum by (source_zone, destination_zone) (rate({{ $prefix }}_zone_pmtu_results_total{result="fail"}[10m]))
        /
        sum by (source_zone, destination_zone) (rate({{ $prefix }}_zone_pmtu_results_total[10m]))
        > {{ $t }}
      )
      or
      (
        sum by (source_zone, destination_zone) (rate({{ $prefix }}_zone_pmtu_results_total{result="fail"}[30m]))
        /
        sum by (source_zone, destination_zone) (rate({{ $prefix }}_zone_pmtu_results_total[30m]))
        > {{ $st }}
        and on (source_zone, destination_zone)
        sum by (source_zone, destination_zone) (increase({{ $prefix }}_zone_pmtu_results_total{result="fail"}[30m]))
        >= 2
        and on (source_zone, destination_zone)
        sum by (source_zone, destination_zone) (increase({{ $prefix }}_zone_pmtu_results_total{result="fail"}[10m]))
        > 0
      )
    )
    unless on (source_zone, destination_zone)
    count by (source_zone, destination_zone) ({{ $prefix }}_pmtu_results_total)
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      Path MTU black hole zone {{`{{ $labels.source_zone }}`}} -> zone
      {{`{{ $labels.destination_zone }}`}} in {{`{{ $value | humanizePercentage }}`}} of probes
    description: >-
      Large packets between the zones are dropped without ICMP frag-needed. The console Matrix shows
      which node pairs.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "ZonePathMTUBlackHole" }}
    investigateUrl: >-
      /investigate?kind=zone-pair&scope={{`{{ $labels.source_zone }}`}}->{{`{{ $labels.destination_zone }}`}}
{{- end }}
{{- end }}
{{- with include "kconmon-ng.prometheusRule.block" (dict "block" $pr.nodeUnreachable "defaults" (dict "enabled" true "threshold" 0.5 "minPeers" 2 "for" "5m" "severity" "critical")) | fromJson }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
{{/* A pair counts as failing when most of its TCP probes fail (> 0.5, fixed: this rule is about
     how many peers, not how badly each one fails). Only pairs with traffic count toward the
     denominator, so a sparse topology's unplanned pairs never dilute it. */}}
- alert: NodeUnreachable
  expr: >-
    (
      count by (destination_node, destination_zone) (
        (
          sum by (source_node, destination_node, destination_zone) (rate({{ $prefix }}_tcp_results_total{result="fail"}[5m]))
          /
          sum by (source_node, destination_node, destination_zone) (rate({{ $prefix }}_tcp_results_total[5m]))
        ) > 0.5
      )
      /
      count by (destination_node, destination_zone) (
        sum by (source_node, destination_node, destination_zone) (rate({{ $prefix }}_tcp_results_total[5m])) > 0
      )
    ) > {{ $t }}
    and on (destination_node, destination_zone)
    count by (destination_node, destination_zone) (
      sum by (source_node, destination_node, destination_zone) (rate({{ $prefix }}_tcp_results_total[5m])) > 0
    ) >= {{ .minPeers }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      Node {{`{{ $labels.destination_node }}`}} unreachable from
      {{`{{ $value | humanizePercentage }}`}} of its peers
    description: >-
      Its agent still runs, but peers cannot reach it over TCP. Check the node itself first: the
      CNI agent, the host firewall, kubelet.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "NodeUnreachable" }}
{{- end }}
{{- end }}
{{- with include "kconmon-ng.prometheusRule.block" (dict "block" $pr.nodeIsolated "defaults" (dict "enabled" true "threshold" 0.5 "minPeers" 2 "for" "5m" "severity" "critical")) | fromJson }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
- alert: NodeIsolated
  expr: >-
    (
      count by (source_node, source_zone) (
        (
          sum by (source_node, destination_node, source_zone) (rate({{ $prefix }}_tcp_results_total{result="fail"}[5m]))
          /
          sum by (source_node, destination_node, source_zone) (rate({{ $prefix }}_tcp_results_total[5m]))
        ) > 0.5
      )
      /
      count by (source_node, source_zone) (
        sum by (source_node, destination_node, source_zone) (rate({{ $prefix }}_tcp_results_total[5m])) > 0
      )
    ) > {{ $t }}
    and on (source_node, source_zone)
    count by (source_node, source_zone) (
      sum by (source_node, destination_node, source_zone) (rate({{ $prefix }}_tcp_results_total[5m])) > 0
    ) >= {{ .minPeers }}
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      Node {{`{{ $labels.source_node }}`}} cannot reach
      {{`{{ $value | humanizePercentage }}`}} of its peers
    description: >-
      The node's own egress is broken: check its CNI agent, routes and network policy, not the
      peers.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "NodeIsolated" }}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      No probe results {{`{{ $labels.source_node }}`}} -> {{`{{ $labels.destination_node }}`}}
      for over {{ .for }}
    description: >-
      The other rules say nothing about this pair now. Check the agent pod on
      {{`{{ $labels.source_node }}`}} and its scrape target; a removed node clears within an hour.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "PairWentSilent" }}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      DNS failing on {{`{{ $labels.source_node }}`}} for {{`{{ $labels.host }}`}} via
      {{`{{ $labels.resolver }}`}} at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      A resolver problem, not a peer link: check CoreDNS and the node's resolv.conf first.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "DNSChecksFailing" }}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      External target {{`{{ $labels.target }}`}} failing from
      {{`{{ $labels.source_node }}`}} at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      Probes to the {{`{{ $labels.target_kind }}`}} target fail. A probe the allowlist refused is
      counted in external_denied_total instead.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "ExternalChecksFailing" }}
{{- end }}
{{- end }}
{{- with $pr.zoneChecksFailing }}
{{- if .enabled }}
{{- $t := float64 .threshold }}
{{/* One rule across all three protocols; a disabled checker's family is simply an empty
     branch of the union. The label_replace/or shape is load-bearing: rate() over a bare
     __name__ union drops __name__ and collapses the three families into duplicate labelsets,
     which the engine refuses at EVALUATION time ("vector cannot contain metrics with the same
     labelset") — helm template and promtool syntax checks never see it. */}}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      Zone checks failing {{`{{ $labels.source_zone }}`}} ->
      {{`{{ $labels.destination_zone }}`}} at {{`{{ $value | humanizePercentage }}`}} of
      probes
    description: >-
      The Zone Heatmap dashboard shows which protocol and direction fail; the console Matrix shows
      the node pairs.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "ZoneChecksFailing" }}
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
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      Packet loss {{`{{ $labels.source_zone }}`}} -> {{`{{ $labels.destination_zone }}`}}
      at {{`{{ $value | humanizePercentage }}`}}
    description: >-
      UDP and ICMP loss across all pairs between the zones; UDPLossHigh names a single broken pair.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "ZoneLossHigh" }}
    {{- /* Same contract as ZoneChecksFailing's investigateUrl: console-relative, "->" normalised
         by the console itself. */}}
    investigateUrl: >-
      /investigate?kind=zone-pair&scope={{`{{ $labels.source_zone }}`}}->{{`{{ $labels.destination_zone }}`}}
{{- end }}
{{- end }}
{{- with $pr.kconmonAgentsMissing }}
{{- if .enabled }}
{{/* Standbys hold no agents by design, so only the lease holder's counts are evidence. External
     agents register through the gateway with no node to expect them on, so they leave the
     registered count; `or registered * 0` stands in for the gauge on a controller image that
     predates it and carries the same labels, where a bare vector(0) has none and matches nothing. */}}
- alert: KconmonAgentsMissing
  expr: >-
    ({{ $prefix }}_controller_expected_agents
    - ({{ $prefix }}_controller_registered_agents
    - ({{ $prefix }}_controller_external_agents or {{ $prefix }}_controller_registered_agents * 0)) > 0)
    and ({{ $prefix }}_controller_leader == 1)
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      {{`{{ $value }}`}} kconmon-ng agent(s) missing
    description: >-
      Nodes without an agent are not probed at all. Check the agent DaemonSet: scheduling, crash
      loops, gRPC to the controller.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "KconmonAgentsMissing" }}
{{- end }}
{{- end }}
{{- with $pr.kconmonControllerDown }}
{{- if .enabled }}
- alert: KconmonControllerDown
  expr: absent({{ $prefix }}_controller_leader == 1)
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: No kconmon-ng controller has reported itself leader for {{ .for }}
    description: >-
      Agents keep probing a frozen topology. Check the controller Deployment, its lease and its
      scrape target.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "KconmonControllerDown" }}
{{- end }}
{{- end }}
{{- with $pr.externalAgentDown }}
{{- if .enabled }}
{{/* up is Prometheus' own series, so no metricsPrefix. The job regex, not only the resolved jobName,
     so the plain-Prometheus job from docs/external-agents.md is covered too; a custom
     scrapeConfig.externalAgents.jobName without "agent-external" joins it literally. The labels are
     the ones the controller's SD body attaches (node, zone, external, agent_id). */}}
{{- $job := ".*agent-external.*" }}
{{- if include "kconmon-ng.scrapeConfig.externalAgents.enabled" $ }}
{{- $name := include "kconmon-ng.scrapeConfig.externalAgents.jobName" $ }}
{{- if not (contains "agent-external" $name) }}
{{- $job = printf "%s|%s" (regexQuoteMeta $name) $job }}
{{- end }}
{{- end }}
- alert: KconmonExternalAgentDown
  expr: up{job=~{{ $job | quote }}} == 0
  for: {{ .for }}
  labels:
    severity: {{ .severity }}
    namespace: {{ $.Release.Namespace }}
  annotations:
    summary: >-
      External kconmon-ng agent {{`{{ $labels.node }}`}} is not answering scrapes
    description: >-
      It still registers with the gateway, so the host is up: usually its firewall blocks the
      metrics port from the cluster's node CIDR.
    runbook_url: {{ include "kconmon-ng.prometheusRule.runbook" "KconmonExternalAgentDown" }}
{{- end }}
{{- end }}
{{- end -}}
