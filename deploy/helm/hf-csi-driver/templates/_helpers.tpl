{{- define "hf-csi-driver.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "hf-csi-driver.fullname" -}}
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

{{- define "hf-csi-driver.labels" -}}
app.kubernetes.io/name: {{ include "hf-csi-driver.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{- define "hf-csi-driver.selectorLabels" -}}
app: hf-csi-node
{{- end }}

{{- define "hf-csi-driver.proxyEnv" -}}
{{- with .Values.proxy }}
{{- if .httpProxy }}
- name: HTTP_PROXY
  value: {{ .httpProxy | quote }}
{{- end }}
{{- if .httpsProxy }}
- name: HTTPS_PROXY
  value: {{ .httpsProxy | quote }}
{{- end }}
{{- if .noProxy }}
- name: NO_PROXY
  value: {{ .noProxy | quote }}
{{- end }}
{{- if .allProxy }}
- name: ALL_PROXY
  value: {{ .allProxy | quote }}
{{- end }}
{{- end }}
{{- end }}
