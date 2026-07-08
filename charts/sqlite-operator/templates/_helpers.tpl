{{/*
Expand the name of the chart.
*/}}
{{- define "sqlite-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "sqlite-operator.fullname" -}}
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
Create chart label.
*/}}
{{- define "sqlite-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to all resources.
*/}}
{{- define "sqlite-operator.labels" -}}
helm.sh/chart: {{ include "sqlite-operator.chart" . }}
{{ include "sqlite-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (used in Deployment selector and Service selector).
*/}}
{{- define "sqlite-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sqlite-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Service account name.
*/}}
{{- define "sqlite-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "sqlite-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Operator image reference: repository:tag (tag defaults to appVersion).
*/}}
{{- define "sqlite-operator.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Webhook service name (used by both Service and WebhookConfiguration).
*/}}
{{- define "sqlite-operator.webhookServiceName" -}}
{{- printf "%s-webhook" (include "sqlite-operator.fullname" .) }}
{{- end }}

{{/*
cert-manager Certificate/Issuer name.
*/}}
{{- define "sqlite-operator.certName" -}}
{{- printf "%s-serving-cert" (include "sqlite-operator.fullname" .) }}
{{- end }}

{{/*
TLS secret name for the webhook server.
*/}}
{{- define "sqlite-operator.webhookSecretName" -}}
{{- .Values.certManager.secretName }}
{{- end }}
