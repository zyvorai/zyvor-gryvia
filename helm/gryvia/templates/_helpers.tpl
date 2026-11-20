{{/*
Expand the name of the chart.
*/}}
{{- define "gryvia.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "gryvia.fullname" -}}
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
{{- define "gryvia.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gryvia.labels" -}}
helm.sh/chart: {{ include "gryvia.chart" . }}
{{ include "gryvia.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "gryvia.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gryvia.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "gryvia.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "gryvia.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference for a component: <registry>/<name>:<tag>. The tag falls back to global.imageTag, then appVersion.
Usage: {{ include "gryvia.image" (dict "root" . "image" .Values.gpuOperator.image) }}
*/}}
{{- define "gryvia.image" -}}
{{- $tag := default (default .root.Chart.AppVersion .root.Values.global.imageTag) .image.tag -}}
{{- printf "%s/%s:%s" (trimSuffix "/" .root.Values.global.imageRegistry) .image.name $tag -}}
{{- end }}

{{/* Namespace for every namespaced resource. */}}
{{- define "gryvia.namespace" -}}
{{- .Values.namespace.name -}}
{{- end }}

{{/* Name of the API key Secret. */}}
{{- define "gryvia.apiKeySecret" -}}
{{- default "gryvia-api-key" .Values.auth.existingSecret -}}
{{- end }}

{{/*
Names for the GryviaAIJob admission webhook (Service and serving-cert Secret).
*/}}
{{- define "gryvia.webhookServiceName" -}}
{{- printf "%s-webhook" (include "gryvia.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "gryvia.webhookSecretName" -}}
{{- printf "%s-webhook-tls" (include "gryvia.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Names for the GryviaUsageRecord admission webhook (Service and serving-cert Secret). Separate from the
GryviaAIJob webhook so its failurePolicy and certificate are independent.
*/}}
{{- define "gryvia.usageWebhookServiceName" -}}
{{- printf "%s-usage-webhook" (include "gryvia.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "gryvia.usageWebhookSecretName" -}}
{{- printf "%s-usage-webhook-tls" (include "gryvia.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end }}
