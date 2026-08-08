{{/*
Expand the name of the chart.
*/}}
{{- define "kconmon-ng.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "kconmon-ng.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "kconmon-ng.labels" -}}
helm.sh/chart: {{ include "kconmon-ng.chart" . }}
{{ include "kconmon-ng.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "kconmon-ng.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kconmon-ng.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Controller service name (for agent controllerAddress)
*/}}
{{- define "kconmon-ng.controllerService" -}}
{{- printf "%s-controller:%d" (include "kconmon-ng.fullname" .) (int .Values.config.grpcPort) }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "kconmon-ng.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kconmon-ng.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Console fully qualified name.
*/}}
{{- define "kconmon-ng.console.fullname" -}}
{{- printf "%s-console" (include "kconmon-ng.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Console bundled Valkey fully qualified name (console.valkey.mode=bundled).
*/}}
{{- define "kconmon-ng.console.valkeyFullname" -}}
{{- printf "%s-valkey" (include "kconmon-ng.console.fullname" . | trunc 56 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Console Valkey address written into the console ConfigMap.
Resolves console.valkey.mode (a Helm-only concept) to the Go-side
"empty string = disabled" convention:
  bundled  -> "<console fullname>-valkey:<port>"
  external -> console.valkey.address verbatim (validated non-empty)
  disabled -> "" (console falls back to the in-process bus, ADR-002)
*/}}
{{- define "kconmon-ng.console.valkeyAddress" -}}
{{- $v := .Values.console.valkey -}}
{{- if eq $v.mode "bundled" -}}
{{- printf "%s:%d" (include "kconmon-ng.console.valkeyFullname" .) (int $v.port) -}}
{{- else if eq $v.mode "external" -}}
{{- if not $v.address -}}
{{- fail "console.valkey.mode=external requires console.valkey.address (host:port)" -}}
{{- end -}}
{{- $v.address -}}
{{- end -}}
{{- end }}

{{/*
Port the console's Valkey egress rule must open.

For mode=bundled this is console.valkey.port: the chart owns both ends of that
connection, so the listener and the rule cannot disagree.

For mode=external it is the port inside console.valkey.address, because THAT is
the port the console actually dials (valkeyAddress above hands the address to
the config verbatim). console.valkey.port describes the BUNDLED listener and
has nothing to do with an external instance, so using it there renders an
egress rule for a port nothing connects to — `address: valkey.example.com:6380`
with the default port would open 6379 and drop every packet with no diagnostic
beyond a dial timeout.

Both address shapes end in the port: "host:6379" and "[2001:db8::1]:6379". A
trailing field that is not all digits means a malformed address; the rule falls
back to console.valkey.port rather than emitting a non-integer port and failing
the whole manifest — the console's own net.SplitHostPort rejects it at boot
with a message that names the field.
*/}}
{{- define "kconmon-ng.console.valkeyEgressPort" -}}
{{- $v := .Values.console.valkey -}}
{{- $port := "" -}}
{{- if eq $v.mode "external" -}}
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

{{/*
Console PostgreSQL: CNPG Cluster name (console.database.mode=cnpg).
*/}}
{{- define "kconmon-ng.console.databaseClusterName" -}}
{{- /* trunc 60, not 63: CNPG derives pod names "<cluster>-N" and stamps them
into the cnpg.io/instanceName LABEL (63-char cap) — a 63-char cluster name
makes instance creation fail. */ -}}
{{- printf "%s-db" (include "kconmon-ng.console.fullname" . | trunc 57 | trimSuffix "-") | trunc 60 | trimSuffix "-" }}
{{- end }}

{{/*
Console PostgreSQL: name of the Secret holding the DSN. Resolves
console.database.mode the same way valkeyAddress resolves valkey.mode:
  cnpg     -> "<cluster>-app" (the Secret CNPG generates for the app user)
  external -> console.database.existingSecret (validated non-empty)
The chart only ever references this Secret BY NAME; it never templates,
generates or reads credential material.
*/}}
{{- define "kconmon-ng.console.databaseSecretName" -}}
{{- $db := .Values.console.database -}}
{{- if eq $db.mode "cnpg" -}}
{{- printf "%s-app" (include "kconmon-ng.console.databaseClusterName" .) -}}
{{- else if eq $db.mode "external" -}}
{{- if not $db.existingSecret -}}
{{- fail "console.database.mode=external requires console.database.existingSecret (a Secret holding a full postgres:// DSN)" -}}
{{- end -}}
{{- $db.existingSecret -}}
{{- end -}}
{{- end }}

{{/*
Console PostgreSQL: key inside the DSN Secret.
  cnpg     -> "uri" (CNPG's generated key holding the full DSN)
  external -> console.database.existingSecretKey
*/}}
{{- define "kconmon-ng.console.databaseSecretKey" -}}
{{- if eq .Values.console.database.mode "cnpg" -}}
uri
{{- else -}}
{{- .Values.console.database.existingSecretKey -}}
{{- end -}}
{{- end }}

{{/*
Console local-mode bootstrap admin: name of the Secret holding the bootstrap
password (auth.local.existingSecret). Referenced by name only -- this chart
never creates or reads it, same as databaseSecretName above. Only required
(and only checked) when auth.local.bootstrapAdmin is actually set: an
operator running mode=local without a bootstrap admin (users provisioned
some other way) is not required to set existingSecret at all.
*/}}
{{- define "kconmon-ng.console.localAdminSecretName" -}}
{{- $local := .Values.console.auth.local -}}
{{- if and $local.bootstrapAdmin (not $local.existingSecret) -}}
{{- fail "console.auth.local.bootstrapAdmin is set but console.auth.local.existingSecret is empty (a Secret holding the bootstrap password is required to create it)" -}}
{{- end -}}
{{- $local.existingSecret -}}
{{- end }}

{{/*
Console oidc-mode: name of the Secret holding the OIDC client secret
(auth.oidc.existingSecret). Referenced by name only, same as
databaseSecretName above. REQUIRED whenever auth.mode=oidc -- unlike the
local-mode bootstrap secret, there is no "mode=oidc without a client secret"
degraded path (config.Config.Validate's own auth.oidc.clientSecretFile check
would reject it at console boot anyway; failing the render here is faster).
*/}}
{{- define "kconmon-ng.console.oidcClientSecretName" -}}
{{- $oidc := .Values.console.auth.oidc -}}
{{- if not $oidc.existingSecret -}}
{{- fail "console.auth.mode=oidc requires console.auth.oidc.existingSecret (a Secret holding the OIDC client secret)" -}}
{{- end -}}
{{- $oidc.existingSecret -}}
{{- end }}

{{/*
M6: name of the Secret holding the webhook AES-GCM encryption key
(console.webhooks.encryptionKeySecret.name), or empty when no key is
configured -- the documented keyless state, not a failure. Referenced BY NAME
only, exactly like databaseSecretName: the chart never templates the key.

The key FIELD is validated here rather than in the schema because "" is a
legal value for an unset block: an empty `key` with a name set would mount a
Secret item nobody can name, and the console would fatal on an unreadable
encryptionKeyFile at boot instead of failing the render.
*/}}
{{- define "kconmon-ng.console.webhooksKeySecretName" -}}
{{- $w := .Values.console.webhooks.encryptionKeySecret -}}
{{- if $w.name -}}
{{- if not $w.key -}}
{{- fail "console.webhooks.encryptionKeySecret.name is set but .key is empty (the key inside that Secret holding the base64 AES-256-GCM key)" -}}
{{- end -}}
{{- $w.name -}}
{{- end -}}
{{- end }}

{{/*
Whether the console Pod gets the /etc/kconmon-ng-console-secrets projected
volume at all. True when a database is configured (the DSN, and with it the
local-admin / OIDC sources that ride the same volume) OR when a webhook
encryption key is named.

The webhook key is the first secret file that does NOT imply a database: the
console's own posture for "key, no db" is a WARNING and a disabled dispatcher
(cmd/console), not a failure, so the chart must not be stricter than the binary
and refuse to render it. Without this OR the key would be named in the config
and mounted nowhere -- the console would then fatal on an unreadable
encryptionKeyFile, turning a warn-and-continue into a crashloop.
*/}}
{{- define "kconmon-ng.console.secretsVolumeEnabled" -}}
{{- if or (ne .Values.console.database.mode "disabled") (include "kconmon-ng.console.webhooksKeySecretName" .) -}}
true
{{- end -}}
{{- end }}

{{/*
M7: does the console pod need a Kubernetes identity at all? TWO features now
answer yes, and every gate that used to name kubernetesContext.enabled directly
(the SA, the Deployment's serviceAccountName, POD_NAMESPACE, the apiserver
egress rule) routes through here instead, so the two can never disagree:

  - console.kubernetesContext.enabled (M6) — the core/v1 Event reader.
  - console.alerting.enabled (M7) — the PrometheusRule reconciler, which SSAs
    the bundle object through a dynamic client against the same apiserver.

Both need POD_NAMESPACE for the same reason (an empty namespace value means
"where this Pod runs"), both need the apiserver egress rule, and both need a
subject to bind a grant to. What they do NOT share is the grant itself: M6's is
a cluster-scoped ClusterRole (Node events are written into whatever namespace
the kubelet likes), M7's is a namespaced Role (SECURITY.md §10.3). See
rbac.yaml.

Still off by default, so a chart 1.8.0 render is unchanged.
*/}}
{{- define "kconmon-ng.console.k8sIdentity" -}}
{{- if or .Values.console.kubernetesContext.enabled .Values.console.alerting.enabled -}}
true
{{- end -}}
{{- end }}

{{/*
M6: the console's OWN ServiceAccount name. It exists only when the console
needs a Kubernetes identity (kconmon-ng.console.k8sIdentity): those are the
features that need the console pod to hold one at all, and creating an SA (plus
binding a role to it) for every install would change the default render
and hand a token to a pod that never calls the apiserver.

Deliberately NOT kconmon-ng.serviceAccountName. That SA is shared by the agent
DaemonSet and the controller Deployment (both set serviceAccountName from it);
the console has never set serviceAccountName at all and runs as the namespace
`default` SA. Adding events/pods read to the shared ClusterRole would have
granted it to every agent Pod on every node -- the widening M6 was told not to
do -- and would not even have reached the console, which is not a subject of
that binding.

With serviceAccount.create=false the chart creates nothing and resolves to
serviceAccount.name (or "default"), so an operator who manages ServiceAccounts
out of band attaches the grant there. NOTE the asymmetry with the two siblings:
agent/daemonset.yaml and controller/deployment.yaml wrap their own
serviceAccountName in `{{- with .Values.serviceAccount.create }}`, and `with`
treats false as falsy, so those two OMIT the key entirely and ignore
serviceAccount.name in that branch. This template renders the resolved name in
both branches, which is the behaviour values.yaml documents ("Set false to use
an existing one"). Rendering "default" is equivalent to omitting the key, so the
difference only shows when serviceAccount.name is actually set.
*/}}
{{- define "kconmon-ng.console.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- include "kconmon-ng.console.fullname" . -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/*
Console -> controller gRPC dial target for the events ingester.
Empty (realtime off) unless controller.events.enabled. An explicit
console.controller.grpcAddr always wins; otherwise reuse the SAME
Service:port agents already dial. NOTE: kconmon-ng.controllerService
ALREADY includes ":<grpcPort>" — never append the port again.
*/}}
{{- define "kconmon-ng.console.controllerGRPCAddr" -}}
{{- if .Values.console.controller.grpcAddr -}}
{{- .Values.console.controller.grpcAddr -}}
{{- else if .Values.controller.events.enabled -}}
{{- include "kconmon-ng.controllerService" . -}}
{{- end -}}
{{- end }}
