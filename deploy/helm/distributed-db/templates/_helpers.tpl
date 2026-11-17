{{/*
Chart name and fullname. Resource names are capped at 45 characters so that
suffixes such as "-meta-headless" stay below the 63-character DNS limit.
*/}}
{{- define "distributed-db.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "distributed-db.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 45 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 45 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 45 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "distributed-db.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "distributed-db.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "distributed-db.selectorLabels" -}}
app.kubernetes.io/name: {{ include "distributed-db.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "distributed-db.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "distributed-db.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "distributed-db.image" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}

{{- define "distributed-db.metaService" -}}
{{- printf "%s-meta" (include "distributed-db.fullname" .) -}}
{{- end -}}

{{- define "distributed-db.metaHeadless" -}}
{{- printf "%s-meta-headless" (include "distributed-db.fullname" .) -}}
{{- end -}}

{{- define "distributed-db.kvService" -}}
{{- printf "%s-kv" (include "distributed-db.fullname" .) -}}
{{- end -}}

{{- define "distributed-db.kvHeadless" -}}
{{- printf "%s-kv-headless" (include "distributed-db.fullname" .) -}}
{{- end -}}

{{- define "distributed-db.sqlsrvService" -}}
{{- printf "%s-sqlsrv" (include "distributed-db.fullname" .) -}}
{{- end -}}

{{- define "distributed-db.pgsrvService" -}}
{{- printf "%s-pgsrv" (include "distributed-db.fullname" .) -}}
{{- end -}}

{{/*
Fully qualified KV peer addresses. meta, sqlsrv and pgsrv all use this list.
*/}}
{{- define "distributed-db.kvNodes" -}}
{{- $root := . -}}
{{- $base := include "distributed-db.fullname" $root -}}
{{- $nodes := list -}}
{{- range $i := until (int $root.Values.kv.replicaCount) -}}
{{- $nodes = append $nodes (printf "%s-kv-%d.%s-kv-headless.%s.svc.%s:%d" $base (int $i) $base $root.Release.Namespace $root.Values.clusterDomain (int $root.Values.kv.port)) -}}
{{- end -}}
{{- join "," $nodes -}}
{{- end -}}

{{- define "distributed-db.tlsSecret" -}}
{{- required "tls.existingSecret is required when tls.enabled=true" .Values.tls.existingSecret -}}
{{- end -}}

{{- define "distributed-db.adminTokenSecret" -}}
{{- required "auth.adminToken.existingSecret is required when meta.adminToken.enabled=true or sqlsrv.auth.enabled=true" .Values.auth.adminToken.existingSecret -}}
{{- end -}}

{{- define "distributed-db.pgsrvAuthSecret" -}}
{{- required "auth.pgsrv.existingSecret is required when pgsrv.auth.enabled=true" .Values.auth.pgsrv.existingSecret -}}
{{- end -}}

{{/*
Configuration invariants. Called from configmap.yaml so it runs on every
render/install and fails before any manifest is applied.
*/}}
{{- define "distributed-db.validate" -}}
{{- $metaReplicas := int .Values.meta.replicaCount -}}
{{- if and (ne $metaReplicas 3) (ne $metaReplicas 5) -}}
{{- fail (printf "meta.replicaCount must be 3 or 5 (got %d); an even meta Raft group cannot tolerate the same failures" $metaReplicas) -}}
{{- end -}}
{{- $kvReplicas := int .Values.kv.replicaCount -}}
{{- if or (lt $kvReplicas 3) (gt $kvReplicas 5) -}}
{{- fail (printf "kv.replicaCount must be between 3 and 5 (got %d)" $kvReplicas) -}}
{{- end -}}
{{- if and .Values.tls.enabled (not .Values.tls.existingSecret) -}}
{{- fail "tls.existingSecret is required when tls.enabled=true" -}}
{{- end -}}
{{- if and .Values.tls.mtls (not .Values.tls.enabled) -}}
{{- fail "tls.mtls=true requires tls.enabled=true" -}}
{{- end -}}
{{- if and .Values.pgsrv.auth.enabled (not .Values.tls.enabled) -}}
{{- fail "pgsrv.auth.enabled=true requires tls.enabled=true: pgsrv refuses password auth without TLS" -}}
{{- end -}}
{{- if and (or .Values.meta.adminToken.enabled .Values.sqlsrv.auth.enabled) (not .Values.auth.adminToken.existingSecret) -}}
{{- fail "auth.adminToken.existingSecret is required when meta.adminToken.enabled=true or sqlsrv.auth.enabled=true" -}}
{{- end -}}
{{- if and (or .Values.meta.adminToken.enabled .Values.sqlsrv.auth.enabled) (not (regexMatch "^[-._a-zA-Z0-9]+$" (default "placeholder" .Values.auth.adminToken.key))) -}}
{{- fail "auth.adminToken.key must be a valid Secret key name" -}}
{{- end -}}
{{- if and .Values.sqlsrv.auth.enabled (not .Values.tls.enabled) -}}
{{- fail "sqlsrv.auth.enabled=true requires tls.enabled=true: sqlhttp refuses Bearer auth on a plaintext listener" -}}
{{- end -}}
{{- end -}}
