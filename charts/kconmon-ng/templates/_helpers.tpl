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
