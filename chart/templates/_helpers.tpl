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
app.kubernetes.io/part-of: damga
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
app.kubernetes.io/part-of: damga
app.kubernetes.io/component: cache
{{- end -}}

{{/*
  An image reference, joined the way the reference grammar requires: ":" before
  a tag and "@" before a digest.

  The difference is not cosmetic — "repo:sha256:abc" is not a valid reference at
  all — and the colon used to be hardcoded at both call sites. That made the two
  images this pipeline publishes the only two in the chart that could not be
  pinned by digest; postgres and redis take the whole string from values and
  always could.

  Pinning matters more than it looks. Kyverno verifies the signature and rewrites
  the reference to the digest it verified, on the Deployment as well as on the
  Pod. Against a tag, git and the cluster then disagree permanently: Argo CD
  reports OutOfSync forever while being exactly what git asked for, rewriting the
  field every reconcile with Kyverno rewriting it straight back. A digest is the
  only form where what git asks for and what admission produces are one string.

  digest wins when both are set. tag stays for whoever runs helm by hand, and to
  say in readable form which release the digest belongs to.
*/}}
{{- define "app.image" -}}
{{- $img := .image -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository ($img.tag | default .defaultTag) -}}
{{- end -}}
{{- end -}}
