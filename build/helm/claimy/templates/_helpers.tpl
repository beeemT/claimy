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

{{/* Keep the hook-only ConfigMap distinct even with a maximum-length fullname. */}}
{{- define "claimy-chart.migrationConfigName" -}}
{{- $suffix := "-migration-config" -}}
{{- $fullname := include "claimy-chart.fullname" . -}}
{{- $limit := int (sub 63 (len $suffix)) -}}
{{- $prefix := $fullname | trunc $limit | trimSuffix "-" -}}
{{- $candidate := printf "%s%s" $prefix $suffix -}}
{{- if eq $candidate $fullname -}}
{{- $candidate | trunc (int (sub (len $candidate) 1)) -}}
{{- else -}}
{{- $candidate -}}
{{- end -}}
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
{{/* Render the same runtime config into the regular and pre-install ConfigMaps. */}}
{{- define "claimy-chart.config" -}}
{{- $managed := dict
      "httpserver" (dict "default" (dict
        "port" (printf "%d" (int .Values.service.port))
        "timeout" (dict "drain" "5s" "shutdown" "60s")
      ))
      "kernel" (dict "kill_timeout" "70s")
      "sqlc" (dict "default" (dict
        "driver" "mysql"
        "uri" (dict
          "host" .Values.database.host
          "port" (int .Values.database.port)
          "user" .Values.database.user
          "database" .Values.database.name
        )
        "parameters" .Values.database.parameters
        "migrations" (dict "enabled" false "path" "build/migrations/claimy")
      ))
   -}}
{{- $config := mustMergeOverwrite (deepCopy .Values.config) $managed -}}
{{- $sqlc := get $config "sqlc" | default dict -}}
{{- $defaultSQLC := get $sqlc "default" | default dict -}}
{{- $uri := get $defaultSQLC "uri" | default dict -}}
{{- $_ := unset $uri "password" -}}
{{- $_ := set $defaultSQLC "uri" $uri -}}
{{- $_ := set $defaultSQLC "parameters" (deepCopy .Values.database.parameters) -}}
{{- $_ := set $defaultSQLC "migrations" (dict "enabled" false "path" "build/migrations/claimy") -}}
{{- $_ := set $sqlc "default" $defaultSQLC -}}
{{- $_ := set $config "sqlc" $sqlc -}}
{{- toYaml $config -}}
{{- end }}

{{/* Keep the raw password Secret first and share all following env entries. */}}
{{- define "claimy-chart.runtimeEnv" -}}
- name: CLAIMY_DATABASE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret | quote }}
      key: {{ include "claimy-chart.passwordKey" . }}
{{ if .Values.extraEnv -}}
{{ toYaml .Values.extraEnv }}
{{- end }}
{{- end }}
