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
