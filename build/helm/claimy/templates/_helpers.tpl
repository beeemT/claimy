{{/* Expand the chart name. */}}
{{- define "claimy-chart.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Expand a release-qualified resource name. */}}
{{- define "claimy-chart.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := include "claimy-chart.name" . }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Labels shared by all chart-owned resources. */}}
{{- define "claimy-chart.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "claimy-chart.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Stable selector labels; changing these requires replacing the workload. */}}
{{- define "claimy-chart.selectorLabels" -}}
app.kubernetes.io/name: {{ include "claimy-chart.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Fully qualified image reference. */}}
{{- define "claimy-chart.image" -}}
{{- $tag := required "image.tag must name an image containing /app/claimy and /app/goose" .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{ printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else -}}
{{ printf "%s:%s" .Values.image.repository $tag }}
{{- end -}}
{{- end }}

{{/* Secret key used for the database password. */}}
{{- define "claimy-chart.passwordKey" -}}
{{- .Values.database.passwordKey | quote }}
{{- end }}
