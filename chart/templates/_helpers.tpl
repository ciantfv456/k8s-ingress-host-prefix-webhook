{{/*
Expand the name of the chart.
*/}}
{{- define "ihpw.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "ihpw.fullname" -}}
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
Common labels.
*/}}
{{- define "ihpw.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "ihpw.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "ihpw.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ihpw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the TLS secret holding the webhook's serving cert/key + CA.
*/}}
{{- define "ihpw.tlsSecretName" -}}
{{- printf "%s-tls" (include "ihpw.fullname" .) }}
{{- end }}

{{/*
Image tag: falls back to the chart's appVersion when .Values.image.tag isn't set.
*/}}
{{- define "ihpw.appVersion" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}
