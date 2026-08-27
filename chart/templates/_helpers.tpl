{{/*
Expand the name of the chart.
*/}}
{{- define "firmirror.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "firmirror.fullname" -}}
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
{{- define "firmirror.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "firmirror.labels" -}}
helm.sh/chart: {{ include "firmirror.chart" . }}
{{ include "firmirror.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "firmirror.selectorLabels" -}}
app.kubernetes.io/name: {{ include "firmirror.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "firmirror.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "firmirror.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Get the image tag
*/}}
{{- define "firmirror.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
Build the firmirror command arguments
*/}}
{{- define "firmirror.args" -}}
- "refresh"
{{- if .Values.storage.s3.enabled }}
- "--s3.enable"
{{- if .Values.storage.s3.cleanup }}
- "--s3.cleanup"
{{- end }}
{{- if .Values.storage.s3.bucket }}
- {{ printf "--s3.bucket=%s" .Values.storage.s3.bucket | quote }}
{{- end }}
{{- if .Values.storage.s3.prefix }}
- {{ printf "--s3.prefix=%s" .Values.storage.s3.prefix | quote }}
{{- end }}
{{- if .Values.storage.s3.region }}
- {{ printf "--s3.region=%s" .Values.storage.s3.region | quote }}
{{- end }}
{{- if .Values.storage.s3.endpoint }}
- {{ printf "--s3.endpoint=%s" .Values.storage.s3.endpoint | quote }}
{{- end }}
{{- else }}
- {{ printf "--output-dir=%s" .Values.storage.outputDir | quote }}
{{- end }}
{{- if .Values.signing.enabled }}
- --sign.certificate=/secrets/signing.cert
- --sign.private-key=/secrets/signing.key
{{- end }}
{{- if .Values.vendors.dell.enabled }}
- "--dell.enable"
{{- if .Values.vendors.dell.machinesId }}
- {{ printf "--dell.machines-id=%s" .Values.vendors.dell.machinesId | quote }}
{{- end }}
{{- end }}
{{- if .Values.vendors.hpe.enabled }}
- "--hpe.enable"
{{- if .Values.vendors.hpe.gens }}
- {{ printf "--hpe.gens=%s" .Values.vendors.hpe.gens | quote }}
{{- end }}
{{- end }}
{{- end }}
