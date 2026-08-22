{{- define "app.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "app.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "app.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "app.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "app.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: k8s-lab
{{- end -}}

{{- define "app.selectorLabels" -}}
app.kubernetes.io/name: {{ include "app.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "app.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "app.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "postgres.fullname" -}}
{{- printf "%s-postgres" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "postgres.selectorLabels" -}}
app.kubernetes.io/name: postgres
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Stable DNS name of the primary database pod (headless Service). */}}
{{- define "postgres.host" -}}
{{- printf "%s-0.%s.%s.svc.cluster.local" (include "postgres.fullname" .) (include "postgres.fullname" .) .Release.Namespace -}}
{{- end -}}

{{- define "web.fullname" -}}
{{- printf "%s-web" (include "app.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
  The web tier needs its own label set. Reusing app.labels would stamp
  app.kubernetes.io/name: k8s-lab-app onto the interface, and the ServiceMonitor
  selects on exactly that — Prometheus would try to scrape /metrics from nginx.
*/}}
{{- define "web.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "web.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: k8s-lab
app.kubernetes.io/component: web
{{- end -}}

{{- define "web.selectorLabels" -}}
app.kubernetes.io/name: k8s-lab-web
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Secret holding the database credentials: either one we render or one the
     operator created out of band. */}}
{{- define "postgres.secretName" -}}
{{- if .Values.postgres.auth.existingSecret -}}
{{- .Values.postgres.auth.existingSecret -}}
{{- else -}}
{{- include "postgres.fullname" . -}}
{{- end -}}
{{- end -}}

{{- define "redis.fullname" -}}
{{- printf "%s-redis" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "redis.selectorLabels" -}}
app.kubernetes.io/name: redis
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "redis.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "redis.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: k8s-lab
app.kubernetes.io/component: cache
{{- end -}}
