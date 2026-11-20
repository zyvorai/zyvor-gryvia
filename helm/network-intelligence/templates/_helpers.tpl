{{/*
Expand the name of the chart.
*/}}
{{- define "network-intelligence.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "network-intelligence.fullname" -}}
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
{{- define "network-intelligence.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "network-intelligence.labels" -}}
helm.sh/chart: {{ include "network-intelligence.chart" . }}
{{ include "network-intelligence.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "network-intelligence.selectorLabels" -}}
app.kubernetes.io/name: {{ include "network-intelligence.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the operator service account to use
*/}}
{{- define "network-intelligence.operatorServiceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-operator" (include "network-intelligence.fullname" .)) .Values.serviceAccount.operator.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.operator.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the collector service account to use
*/}}
{{- define "network-intelligence.collectorServiceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-collector" (include "network-intelligence.fullname" .)) .Values.serviceAccount.collector.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.collector.name }}
{{- end }}
{{- end }}

{{/*
Secret holding the collector listener certificate (tls.crt, tls.key, ca.crt).
*/}}
{{- define "network-intelligence.collectorTLSSecret" -}}
{{- default (printf "%s-collector-tls" (include "network-intelligence.fullname" . | trunc 45 | trimSuffix "-")) .Values.ebpf.security.tls.secretName }}
{{- end }}
