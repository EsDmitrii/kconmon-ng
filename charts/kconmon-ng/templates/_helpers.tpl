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

{{/* Console fully qualified name. */}}
{{- define "kconmon-ng.console.fullname" -}}
{{- printf "%s-console" (include "kconmon-ng.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Bundled Valkey fully qualified name. */}}
{{- define "kconmon-ng.console.valkeyFullname" -}}
{{- printf "%s-valkey" (include "kconmon-ng.console.fullname" . | trunc 56 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Service of the Valkey subchart (console.valkey.mode=dependency). */}}
{{- define "kconmon-ng.console.valkeyDependencyFullname" -}}
{{- $vk := .Values.valkey | default dict -}}
{{- $vk.fullnameOverride | default (printf "%s-valkey" .Release.Name) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/* Port the Valkey subchart's Service listens on. */}}
{{- define "kconmon-ng.console.valkeyDependencyPort" -}}
{{- dig "service" "port" 6379 (.Values.valkey | default dict) -}}
{{- end }}

{{/* Valkey address for the console config; empty (disabled) means the in-process bus. */}}
{{- define "kconmon-ng.console.valkeyAddress" -}}
{{- $v := .Values.console.valkey -}}
{{- if eq $v.mode "bundled" -}}
{{- printf "%s:%d" (include "kconmon-ng.console.valkeyFullname" .) (int $v.port) -}}
{{- else if eq $v.mode "dependency" -}}
{{- if not (dig "enabled" false (.Values.valkey | default dict)) -}}
{{- fail "console.valkey.mode=dependency requires valkey.enabled=true (the Valkey subchart is not being installed)" -}}
{{- end -}}
{{- /* The subchart's ACL auth is off by default; when it IS on the console needs the same password or every publish dies with NOAUTH. */ -}}
{{- if and (dig "auth" "enabled" false (.Values.valkey | default dict)) (not (include "kconmon-ng.console.valkeySecretName" .)) -}}
{{- fail "console.valkey.mode=dependency with valkey.auth.enabled=true needs the console to hold the same password: point valkey.auth.usersExistingSecret and console.valkey.existingSecret at ONE Secret, with console.valkey.existingSecretKey set to the ACL username (the subchart keys passwords by username, so the default user means key \"default\")" -}}
{{- end -}}
{{- printf "%s:%d" (include "kconmon-ng.console.valkeyDependencyFullname" .) (int (include "kconmon-ng.console.valkeyDependencyPort" .)) -}}
{{- else if eq $v.mode "external" -}}
{{- if not $v.address -}}
{{- fail "console.valkey.mode=external requires console.valkey.address (host:port)" -}}
{{- end -}}
{{- $v.address -}}
{{- end -}}
{{- end }}

{{/* Port the Valkey egress rule opens: the dialled one, so mode=external reads it out of the address. */}}
{{- define "kconmon-ng.console.valkeyEgressPort" -}}
{{- $v := .Values.console.valkey -}}
{{- $port := "" -}}
{{- if eq $v.mode "dependency" -}}
{{- $port = include "kconmon-ng.console.valkeyDependencyPort" . -}}
{{- else if eq $v.mode "external" -}}
{{- $last := last (splitList ":" $v.address) -}}
{{- if regexMatch "^[0-9]+$" $last -}}
{{- $port = $last -}}
{{- end -}}
{{- end -}}
{{- if $port -}}
{{- $port -}}
{{- else -}}
{{- $v.port | int64 -}}
{{- end -}}
{{- end }}

{{/* CNPG Cluster name; capped at 60 so CNPG's "<cluster>-N" instance label still fits 63. */}}
{{- define "kconmon-ng.console.databaseClusterName" -}}
{{- printf "%s-db" (include "kconmon-ng.console.fullname" . | trunc 57 | trimSuffix "-") | trunc 60 | trimSuffix "-" }}
{{- end }}

{{/* Resolve a renamed value: the new key wins, the deprecated one is honoured alone, both set fails, and a new key still equal to .newDefault counts as unset. */}}
{{- define "kconmon-ng.renamed" -}}
{{- $old := .old -}}
{{- $new := .new -}}
{{- $oldSet := and (not (kindIs "invalid" $old)) (ne (printf "%v" $old) "") -}}
{{- $newSet := and (not (kindIs "invalid" $new)) (ne (printf "%v" $new) "") -}}
{{- if and $newSet (hasKey . "newDefault") -}}
{{- if eq (printf "%v" $new) (printf "%v" .newDefault) -}}
{{- $newSet = false -}}
{{- end -}}
{{- end -}}
{{- if and $oldSet $newSet -}}
{{- fail (printf "%s is deprecated and %s is also set — remove the deprecated key" .oldPath .newPath) -}}
{{- end -}}
{{- if $newSet -}}{{- $new -}}{{- else if $oldSet -}}{{- $old -}}{{- else -}}{{- $new -}}{{- end -}}
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

{{/* Name of the DSN Secret: CNPG's generated <cluster>-app, an existing one, or a chart-created one. */}}
{{- define "kconmon-ng.console.databaseSecretName" -}}
{{- $db := .Values.console.database -}}
{{- if eq $db.mode "cnpg" -}}
{{- printf "%s-app" (include "kconmon-ng.console.databaseClusterName" .) -}}
{{- else if eq $db.mode "external" -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $db.existingSecret "secret" $db.secret "path" "console.database" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "dsn"))) -}}
{{- if not $n -}}
{{- fail "console.database.mode=external requires console.database.existingSecret (a Secret holding a full postgres:// DSN) or console.database.secret.create=true" -}}
{{- end -}}
{{- $n -}}
{{- end -}}
{{- end }}

{{/* Key inside the DSN Secret: CNPG writes "uri". */}}
{{- define "kconmon-ng.console.databaseSecretKey" -}}
{{- if eq .Values.console.database.mode "cnpg" -}}
uri
{{- else -}}
{{- .Values.console.database.existingSecretKey -}}
{{- end -}}
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

{{/* Key inside the webhook Secret; the deprecated encryptionKeySecret.key still wins when alone. */}}
{{- define "kconmon-ng.console.webhooksKeyName" -}}
{{- $w := .Values.console.webhooks -}}
{{- $k := include "kconmon-ng.renamed" (dict "old" (dig "encryptionKeySecret" "key" nil $w) "new" $w.existingSecretKey "newDefault" "console-webhooks-encryption-key" "oldPath" "console.webhooks.encryptionKeySecret.key" "newPath" "console.webhooks.existingSecretKey") -}}
{{- if not $k -}}
{{- fail "console.webhooks.existingSecretKey is empty (it names the key holding the base64 AES-256-GCM key)" -}}
{{- end -}}
{{- $k -}}
{{- end }}

{{/* Webhook encryption-key Secret, or empty for the supported keyless state. */}}
{{- define "kconmon-ng.console.webhooksKeySecretName" -}}
{{- $w := .Values.console.webhooks -}}
{{- $existing := include "kconmon-ng.renamed" (dict "old" (dig "encryptionKeySecret" "name" nil $w) "new" $w.existingSecret "oldPath" "console.webhooks.encryptionKeySecret.name" "newPath" "console.webhooks.existingSecret") -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $existing "secret" $w.secret "path" "console.webhooks" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "webhooks-key"))) -}}
{{- if $n -}}
{{- $_ := include "kconmon-ng.console.webhooksKeyName" . -}}
{{- $n -}}
{{- end -}}
{{- end }}

{{/* Whether the console gets the projected secrets volume: a database or a webhook key. */}}
{{- define "kconmon-ng.console.secretsVolumeEnabled" -}}
{{- if or (ne .Values.console.database.mode "disabled") (include "kconmon-ng.console.webhooksKeySecretName" .) (include "kconmon-ng.console.valkeySecretName" .) -}}
true
{{- end -}}
{{- end }}

{{/* Whether the console needs a Kubernetes identity: the event reader or the alerting reconciler. */}}
{{- define "kconmon-ng.console.k8sIdentity" -}}
{{- if or .Values.console.kubernetesContext.enabled .Values.console.alerting.enabled -}}
true
{{- end -}}
{{- end }}

{{/* Console-only ServiceAccount name, never the shared agent/controller one. */}}
{{- define "kconmon-ng.console.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- include "kconmon-ng.console.fullname" . -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/* Console -> controller gRPC target; controllerService already carries the port. */}}
{{- define "kconmon-ng.console.controllerGRPCAddr" -}}
{{- $c := .Values.console.controller -}}
{{- $addr := include "kconmon-ng.renamed" (dict "old" $c.grpcAddr "new" $c.grpcAddress "oldPath" "console.controller.grpcAddr" "newPath" "console.controller.grpcAddress") -}}
{{- if $addr -}}
{{- $addr -}}
{{- else if .Values.controller.events.enabled -}}
{{- include "kconmon-ng.controllerService" . -}}
{{- end -}}
{{- end }}

{{/* CNPG backup object-store credentials Secret: an existing one or a chart-created one. */}}
{{- define "kconmon-ng.console.backupSecretName" -}}
{{- $b := (.Values.console.database.cnpg | default dict).backup | default dict -}}
{{- $existing := include "kconmon-ng.renamed" (dict "old" $b.credentialsSecret "new" $b.existingSecret "oldPath" "console.database.cnpg.backup.credentialsSecret" "newPath" "console.database.cnpg.backup.existingSecret") -}}
{{- $n := include "kconmon-ng.secretRef" (dict "existing" $existing "secret" $b.secret "path" "console.database.cnpg.backup" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "backup-creds"))) -}}
{{- if not $n -}}
{{- fail "console.database.cnpg.backup.enabled requires console.database.cnpg.backup.existingSecret or console.database.cnpg.backup.secret.create=true" -}}
{{- end -}}
{{- $n -}}
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

{{/* Whether the geoip sources are live at all (enrichment on and a mode that provides files). */}}
{{- define "kconmon-ng.console.geoipEnabled" -}}
{{- $e := .Values.console.mtr.enrichment -}}
{{- if ne (include "kconmon-ng.console.geoipMode" .) "disabled" -}}
{{- if or (include "kconmon-ng.console.geoipPath" (dict "ctx" . "edition" "GeoLite2-ASN" "override" $e.geoip.asnPath)) (include "kconmon-ng.console.geoipPath" (dict "ctx" . "edition" "GeoLite2-City" "override" $e.geoip.cityPath)) -}}
true
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

{{/* Valkey password Secret, or empty when the target needs no AUTH. */}}
{{- define "kconmon-ng.console.valkeySecretName" -}}
{{- $v := .Values.console.valkey -}}
{{- include "kconmon-ng.secretRef" (dict "existing" $v.existingSecret "secret" $v.secret "path" "console.valkey" "default" (include "kconmon-ng.console.secretName" (dict "ctx" . "suffix" "valkey"))) -}}
{{- end }}
