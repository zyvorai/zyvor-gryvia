{{- define "sovereign-aios.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: sovereign-aios
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Gryvia's namespace; the release must be installed into it. */}}
{{- define "sovereign-aios.namespace" -}}
{{- dig "namespace" "name" "gryvia-system" .Values.gryvia -}}
{{- end }}

{{/* One key: the value set in values, else the one already in the Secret, else a new one. */}}
{{- define "sovereign-aios.key" -}}
{{- $v := .set }}
{{- if not $v }}{{ $v = (index .data .field | default "" | b64dec) }}{{ end }}
{{- if not $v }}{{ $v = printf "%s%s" (.prefix | default "") (randAlphaNum 64 | sha256sum | trunc .len) }}{{ end }}
{{- $v }}
{{- end }}

{{/*
The shared credentials, generated once and read back from the existing Secret on upgrades.
Returns a dict with gryviaApiKey, llmKey, netraApiKey, netraAgentKey and agentToken.
*/}}
{{- define "sovereign-aios.credentials" -}}
{{- $c := .Values.credentials }}
{{- $existing := lookup "v1" "Secret" (include "sovereign-aios.namespace" .) $c.secretName }}
{{- $data := dict }}
{{- if and $existing $existing.data }}{{ $data = $existing.data }}{{ end }}
{{- $api := include "sovereign-aios.key" (dict "set" $c.gryviaApiKey "data" $data "field" "GRYVIA_API_KEY" "len" 32) }}
{{- $llm := include "sovereign-aios.key" (dict "set" $c.llmKey "data" $data "field" "ZYNTRA_AI_API_KEY" "len" 64 "prefix" "gk-") }}
{{- $netra := include "sovereign-aios.key" (dict "set" $c.netraApiKey "data" $data "field" "ZYNTRA_NETRA_TOKEN" "len" 48) }}
{{- $agent := include "sovereign-aios.key" (dict "set" $c.netraAgentKey "data" $data "field" "NETRA_AGENT_KEY" "len" 48) }}
{{- $zst := include "sovereign-aios.key" (dict "set" .Values.agentToken.token "data" $data "field" "AGENT_ZYNTRA_TOKEN" "len" 64 "prefix" "zst_") }}
{{- dict "gryviaApiKey" $api "llmKey" $llm "netraApiKey" $netra "netraAgentKey" $agent "agentToken" $zst | toJson }}
{{- end }}

{{/* An External Secrets template expression ({{ .field }}, or {{ .field | sha256sum }} with hash). */}}
{{- define "sovereign-aios.eso" -}}
{{- printf "%s .%s%s %s" "{{" .field (ternary " | sha256sum" "" (default false .hash)) "}}" -}}
{{- end }}

{{/*
A Secret this chart owns: a plain Secret, or with hardening.openbao.enabled an ExternalSecret that writes the same
Secret from OpenBao (data values are then External Secrets templates). Takes root, name, namespace, data and optional
labels and annotations.
*/}}
{{- define "sovereign-aios.secret" -}}
{{- $r := .root }}
{{- $bao := $r.Values.hardening.openbao }}
{{- $labels := merge (dict) (.labels | default dict) (include "sovereign-aios.labels" $r | fromYaml) }}
{{- if $bao.enabled }}
apiVersion: {{ $bao.esoApiVersion }}
kind: ExternalSecret
metadata:
  name: {{ .name }}
  namespace: {{ .namespace }}
  labels:
    {{- include "sovereign-aios.labels" $r | nindent 4 }}
spec:
  refreshInterval: {{ $bao.refreshInterval }}
  secretStoreRef: {kind: ClusterSecretStore, name: sovereign-aios-openbao}
  target:
    name: {{ .name }}
    creationPolicy: Owner
    template:
      engineVersion: v2
      metadata:
        labels:
          {{- toYaml $labels | nindent 10 }}
      data:
        {{- toYaml .data | nindent 8 }}
  dataFrom:
    - extract: {key: {{ $bao.key | quote }}}
{{- else }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ .name }}
  namespace: {{ .namespace }}
  labels:
    {{- toYaml $labels | nindent 4 }}
  {{- with .annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
type: Opaque
stringData:
  {{- toYaml .data | nindent 2 }}
{{- end }}
{{- end }}
