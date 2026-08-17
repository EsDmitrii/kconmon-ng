{{/* Chart name. */}}
{{- define "kconmon-ng.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified app name. */}}
{{- define "kconmon-ng.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Chart name and version, for the chart label. */}}
{{- define "kconmon-ng.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels. */}}
{{- define "kconmon-ng.labels" -}}
helm.sh/chart: {{ include "kconmon-ng.chart" . }}
{{ include "kconmon-ng.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Selector labels. */}}
{{- define "kconmon-ng.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kconmon-ng.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Controller service host:port, used as the agent controllerAddress. */}}
{{- define "kconmon-ng.controllerService" -}}
{{- printf "%s-controller:%d" (include "kconmon-ng.fullname" .) (int .Values.config.grpcPort) }}
{{- end }}

{{/* Agent/controller ServiceAccount name. */}}
{{- define "kconmon-ng.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kconmon-ng.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Component names, TRUNCATED SO THE SUFFIX SURVIVES.

`fullname` is capped at 63 and the templates used to append "-agent"/"-controller" to it, so a
release name of ~52 characters produced a Service name of 69-74 — and a Service name is a DNS-1035
label, hard-capped at 63 by the API server. The install failed at apply time on a name the chart had
already decided to use. Truncating the BASE keeps the component word, which is the half that carries
meaning; truncating the whole string would leave "…-controll".
*/}}
{{- define "kconmon-ng.agent.fullname" -}}
{{- printf "%s-agent" (include "kconmon-ng.fullname" . | trunc 57 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kconmon-ng.controller.fullname" -}}
{{- printf "%s-controller" (include "kconmon-ng.fullname" . | trunc 52 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Console fully qualified name. */}}
{{- define "kconmon-ng.console.fullname" -}}
{{- printf "%s-console" (include "kconmon-ng.fullname" . | trunc 55 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Non-empty when the console has a database: a DSN Secret referenced or chart-created. There is
     no mode any more — the chart does not install PostgreSQL, it dials the one you configured. */}}
{{- define "kconmon-ng.console.hasDatabase" -}}
{{- $db := .Values.database | default dict -}}
{{- if or $db.existingSecret (dig "secret" "create" false $db) -}}true{{- end -}}
{{- end }}

{{/* Non-empty when the console has a Redis-compatible bus: a DSN Secret referenced or chart-created.
     Empty means the in-process bus, which is single-replica only — the configmap guards enforce it.
     Bring your own server; the chart installs nothing here. */}}
{{- define "kconmon-ng.console.hasRedis" -}}
{{- $r := .Values.redis | default dict -}}
{{- if or $r.existingSecret (dig "secret" "create" false $r) -}}true{{- end -}}
{{- end }}

{{/* Name of the Redis DSN Secret: an existing one or a chart-created one; empty means no bus. */}}
{{- define "kconmon-ng.console.redisSecretName" -}}
{{- $r := .Values.redis -}}
{{- include "kconmon-ng.secretRef" (dict "existing" $r.existingSecret "secret" $r.secret "path" "redis" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "redis-dsn"))) -}}
{{- end }}

{{/* Port the console's Prometheus egress rule opens, read out of console.prometheus.url the way the
     Valkey one reads its address: an explicit :port wins, otherwise the scheme's own default. The
     rule used to hardcode 9090, so a Thanos on 10902 or an https endpoint on 443 was configured
     correctly and then blocked by the policy — every matrix, Explore and PromQL page timing out
     against a URL that was right.

     BOTH schemes have a default: https is 443 and http is 80. Only https was handled, so
     `http://prometheus.internal` — a Prometheus or Thanos behind an in-cluster ingress — fell through
     to the 9090 fallback and was blocked in exactly the way this helper exists to prevent. 9090 is
     kept for a URL with no scheme at all, where there is nothing to derive from. */}}
{{- define "kconmon-ng.console.prometheusEgressPort" -}}
{{- $url := .Values.console.prometheus.url | default "" -}}
{{- $rest := $url | replace "https://" "" | replace "http://" "" -}}
{{- $hostport := first (splitList "/" $rest) -}}
{{- $last := last (splitList ":" $hostport) -}}
{{- if regexMatch "^[0-9]+$" $last -}}
{{- $last -}}
{{- else if hasPrefix "https://" $url -}}
443
{{- else if hasPrefix "http://" $url -}}
80
{{- else -}}
9090
{{- end -}}
{{- end }}

{{/* Resolve a Secret reference: an existing one, a chart-created one, or empty. */}}
{{- define "kconmon-ng.secretRef" -}}
{{- $s := .secret | default dict -}}
{{- if and .existing $s.create -}}
{{- fail (printf "%s: existingSecret/name and secret.create are both set — pick one (existingSecret references a Secret you manage, secret.create makes this chart render it)" .path) -}}
{{- end -}}
{{- if $s.create -}}
{{- $s.name | default .default -}}
{{- else -}}
{{- .existing -}}
{{- end -}}
{{- end }}

{{/* Default name for each chart-created console Secret. */}}
{{- define "kconmon-ng.console.secretName" -}}
{{- printf "%s-%s" (include "kconmon-ng.console.fullname" .ctx | trunc 50 | trimSuffix "-") .suffix | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/* Name of the DSN Secret: an existing one or a chart-created one; empty means no database. */}}
{{- define "kconmon-ng.console.databaseSecretName" -}}
{{- $db := .Values.database -}}
{{- include "kconmon-ng.secretRef" (dict "existing" $db.existingSecret "secret" $db.secret "path" "database" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "dsn"))) -}}
{{- end }}

{{/* Key inside the DSN Secret. */}}
{{- define "kconmon-ng.console.databaseSecretKey" -}}
{{- .Values.database.existingSecretKey -}}
{{- end }}

{{/* Bootstrap-admin password Secret; only required when a bootstrapAdmin is set. */}}
{{- define "kconmon-ng.console.localAdminSecretName" -}}
{{- $local := .Values.console.auth.local -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $local.existingSecret "secret" $local.secret "path" "console.auth.local" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "local-admin"))) -}}
{{- if and $local.bootstrapAdmin (not $n) -}}
{{- fail "console.auth.local.bootstrapAdmin is set but no password Secret is configured (set console.auth.local.existingSecret or console.auth.local.secret.create=true)" -}}
{{- end -}}
{{- $n -}}
{{- end }}

{{/* OIDC client-secret Secret; always required for auth.mode=oidc. */}}
{{- define "kconmon-ng.console.oidcClientSecretName" -}}
{{- $oidc := .Values.console.auth.oidc -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $oidc.existingSecret "secret" $oidc.secret "path" "console.auth.oidc" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "oidc"))) -}}
{{- if not $n -}}
{{- fail "console.auth.mode=oidc requires console.auth.oidc.existingSecret (a Secret holding the OIDC client secret) or console.auth.oidc.secret.create=true" -}}
{{- end -}}
{{- $n -}}
{{- end }}

{{/* Key inside the webhook Secret. */}}
{{- define "kconmon-ng.console.webhooksKeyName" -}}
{{- $w := .Values.console.webhooks -}}
{{- $k := $w.existingSecretKey -}}
{{- if not $k -}}
{{- fail "console.webhooks.existingSecretKey is empty (it names the key holding the base64 AES-256-GCM key)" -}}
{{- end -}}
{{- $k -}}
{{- end }}

{{/* Webhook encryption-key Secret, or empty for the supported keyless state. */}}
{{- define "kconmon-ng.console.webhooksKeySecretName" -}}
{{- $w := .Values.console.webhooks -}}
{{- $existing := $w.existingSecret -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $existing "secret" $w.secret "path" "console.webhooks" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "webhooks-key"))) -}}
{{- if $n -}}
{{- $_ := include "kconmon-ng.console.webhooksKeyName" . -}}
{{- $n -}}
{{- end -}}
{{- end }}

{{/* Whether the console gets the projected secrets volume: a database or a webhook key. */}}
{{- define "kconmon-ng.console.secretsVolumeEnabled" -}}
{{- if or (include "kconmon-ng.console.hasDatabase" .) (include "kconmon-ng.console.webhooksKeySecretName" .) (include "kconmon-ng.console.hasRedis" .) -}}
true
{{- end -}}
{{- end }}

{{/* Whether the console needs a Kubernetes identity: the event reader or the alerting reconciler. */}}
{{- define "kconmon-ng.console.k8sIdentity" -}}
{{- if or .Values.console.kubernetesContext.enabled .Values.console.alerting.enabled -}}
true
{{- end -}}
{{- end }}

{{/*
Console-only ServiceAccount name, never the shared agent/controller one.

That was the CLAIM and not the behaviour: with serviceAccount.create=false the console fell back to
serviceAccount.name — the very account the controller Deployment and the agent DaemonSet mount — so
the console's cluster-wide events ClusterRoleBinding and its PrometheusRule write permissions were
granted to every agent pod in the fleet. An agent is the most exposed component there is: it holds
the token, and the console's RBAC came with it.

console.serviceAccount.name is the way to bring your own without sharing. With create=false, no
console name given, and a console feature that needs a Kubernetes identity, the chart refuses to
render rather than quietly widening the agent's RBAC.
*/}}
{{- define "kconmon-ng.console.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- include "kconmon-ng.console.fullname" . -}}
{{- else if .Values.console.serviceAccount.name -}}
{{- .Values.console.serviceAccount.name -}}
{{- else if include "kconmon-ng.console.k8sIdentity" . -}}
{{- fail "serviceAccount.create is false and a console Kubernetes feature is enabled (console.kubernetesContext.enabled or console.alerting.enabled), but console.serviceAccount.name is empty: the console would fall back to serviceAccount.name, which the agent DaemonSet and the controller also mount — granting the console's cluster-wide events read and its PrometheusRule write to every agent pod. Set console.serviceAccount.name to an account you manage for the console alone." -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/* Console -> controller gRPC target; controllerService already carries the port. */}}
{{- define "kconmon-ng.console.controllerGRPCAddr" -}}
{{- $c := .Values.console.controller -}}
{{- $addr := $c.grpcAddress -}}
{{- if $addr -}}
{{- $addr -}}
{{- else if .Values.controller.events.enabled -}}
{{- include "kconmon-ng.controllerService" . -}}
{{- end -}}
{{- end }}


{{/* Resolved geoip file paths: explicit overrides win, otherwise they follow the edition IDs. */}}
{{- define "kconmon-ng.console.geoipPath" -}}
{{- $g := .ctx.Values.console.mtr.enrichment.geoip -}}
{{- if eq (include "kconmon-ng.console.geoipMode" .ctx) "disabled" -}}
{{- else if .override -}}
{{- .override -}}
{{- else if has .edition $g.editions -}}
{{- printf "/geoip/%s.mmdb" .edition -}}
{{- end -}}
{{- end }}

{{/* Whether the geoip sources are live at all (enrichment on and a mode that provides files).

     mode=auto with editions this chart cannot map to a path — a commercial subscriber asking for
     GeoIP2-City and GeoIP2-ISP rather than the GeoLite2 pair — used to resolve to NOTHING here: no
     sidecar, no volume, empty asnPath/cityPath, and geoip enrichment silently off behind a mode that
     says otherwise. It fails the render instead, naming the three knobs that can fix it. */}}
{{- define "kconmon-ng.console.geoipEnabled" -}}
{{- $e := .Values.console.mtr.enrichment -}}
{{- if ne (include "kconmon-ng.console.geoipMode" .) "disabled" -}}
{{- $asn := include "kconmon-ng.console.geoipPath" (dict "ctx" . "edition" "GeoLite2-ASN" "override" $e.geoip.asnPath) -}}
{{- $city := include "kconmon-ng.console.geoipPath" (dict "ctx" . "edition" "GeoLite2-City" "override" $e.geoip.cityPath) -}}
{{- if or $asn $city -}}
true
{{- else -}}
{{- /* The SAME refusal under mode=volume. It used to fire for auto only, so the identical state —
       enrichment on, a mode that promises files, and no path the chart can derive — was a loud
       failure one way and silence the other: a volume-mode console mounted the operator's PVC,
       rendered empty asnPath/cityPath, and ran with hop enrichment off behind a mode that says it is
       on. Nothing in the release, the config or the UI said which of the two it was. The mode is
       named in the message so the operator knows which knob they set. */}}
{{- fail (printf "console.mtr.enrichment.geoip.mode=%s but no database path can be derived: the chart maps only GeoLite2-ASN and GeoLite2-City to default paths, and console.mtr.enrichment.geoip.editions names neither. Add one of those editions, or point console.mtr.enrichment.geoip.asnPath / .cityPath at the files you supply" (include "kconmon-ng.console.geoipMode" .)) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/* MaxMind credentials Secret for the geoipupdate sidecar. */}}
{{- define "kconmon-ng.console.geoipSecretName" -}}
{{- $g := .Values.console.mtr.enrichment.geoip -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $g.existingSecret "secret" $g.secret "path" "console.mtr.enrichment.geoip" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "maxmind"))) -}}
{{- if not $n -}}
{{- fail "console.mtr.enrichment.geoip.mode=auto needs MaxMind credentials: set console.mtr.enrichment.geoip.existingSecret or .secret.create=true (a free GeoLite2 account issues an account ID and licence key), or switch to mode=volume to supply the files yourself" -}}
{{- end -}}
{{- $n -}}
{{- end }}

{{/* Effective console image tag: an explicit override, else the chart appVersion. */}}
{{- define "kconmon-ng.console.imageTag" -}}
{{- .Values.console.image.tag | default .Chart.AppVersion -}}
{{- end }}

{{/* True when the console image is new enough for a config key added in .since; a non-semver tag (latest, a sha) counts as current. */}}
{{- define "kconmon-ng.console.supports" -}}
{{- $tag := include "kconmon-ng.console.imageTag" .ctx -}}
{{- $v := regexFind "^[0-9]+\\.[0-9]+\\.[0-9]+" (trimPrefix "v" $tag) -}}
{{- if not $v -}}
true
{{- else if semverCompare (printf ">=%s" .since) $v -}}
true
{{- end -}}
{{- end }}

{{/* Resolved geoip mode; "disabled" whenever enrichment is off, so callers need one check. */}}
{{- define "kconmon-ng.console.geoipMode" -}}
{{- $e := .Values.console.mtr.enrichment -}}
{{- if not $e.enabled -}}
disabled
{{- else -}}
{{- $g := $e.geoip -}}
{{- if $g.mode -}}
{{- $g.mode -}}
{{- else if $g.volume -}}
volume
{{- else if or $g.asnPath $g.cityPath -}}
volume
{{- else -}}
disabled
{{- end -}}
{{- end -}}
{{- end }}
