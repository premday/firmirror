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
Storage, signing and blocklist arguments, shared by every subcommand.
*/}}
{{- define "firmirror.commonArgs" -}}
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
{{- end }}

{{/*
Build the firmirror command arguments
*/}}
{{- define "firmirror.args" -}}
- "refresh"
{{- include "firmirror.commonArgs" . }}
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

{{/*
The job spec shared by the refresh CronJob and the promote CronJobs.
Takes a dict with "root" (the chart context) and "args" (the rendered argument
list, one YAML sequence entry per line).
*/}}
{{- define "firmirror.jobSpec" -}}
{{- $root := .root -}}
backoffLimit: {{ $root.Values.cronjob.backoffLimit }}
{{- if $root.Values.cronjob.activeDeadlineSeconds }}
activeDeadlineSeconds: {{ $root.Values.cronjob.activeDeadlineSeconds }}
{{- end }}
{{- if $root.Values.cronjob.ttlSecondsAfterFinished }}
ttlSecondsAfterFinished: {{ $root.Values.cronjob.ttlSecondsAfterFinished }}
{{- end }}
template:
  metadata:
    labels:
      {{- include "firmirror.selectorLabels" $root | nindent 6 }}
      {{- with $root.Values.podLabels }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    {{- with $root.Values.podAnnotations }}
    annotations:
      {{- toYaml . | nindent 6 }}
    {{- end }}
  spec:
    {{- with $root.Values.imagePullSecrets }}
    imagePullSecrets:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    serviceAccountName: {{ include "firmirror.serviceAccountName" $root }}
    securityContext:
      {{- toYaml $root.Values.podSecurityContext | nindent 6 }}
    restartPolicy: {{ $root.Values.cronjob.restartPolicy }}
    {{- if $root.Values.cronjob.terminationGracePeriodSeconds }}
    terminationGracePeriodSeconds: {{ $root.Values.cronjob.terminationGracePeriodSeconds }}
    {{- end }}
    containers:
    - name: firmirror
      securityContext:
        {{- toYaml $root.Values.securityContext | nindent 8 }}
      image: "{{ $root.Values.image.repository }}:{{ include "firmirror.imageTag" $root }}"
      imagePullPolicy: {{ $root.Values.image.pullPolicy }}
      command: ["/bin/firmirror"]
      args:
        {{- .args | nindent 8 }}
      {{- if and $root.Values.storage.s3.enabled $root.Values.storage.s3.secretName }}
      env:
      - name: AWS_ACCESS_KEY_ID
        valueFrom:
          secretKeyRef:
            name: {{ $root.Values.storage.s3.secretName }}
            key: AWS_ACCESS_KEY_ID
      - name: AWS_SECRET_ACCESS_KEY
        valueFrom:
          secretKeyRef:
            name: {{ $root.Values.storage.s3.secretName }}
            key: AWS_SECRET_ACCESS_KEY
      {{- end }}
      resources:
        {{- toYaml $root.Values.resources | nindent 8 }}
      {{- if or (not $root.Values.storage.s3.enabled) $root.Values.signing.enabled }}
      volumeMounts:
      {{- if not $root.Values.storage.s3.enabled }}
      - name: data
        mountPath: {{ $root.Values.storage.outputDir }}
      {{- end }}
      {{- if $root.Values.signing.enabled }}
      - name: secrets
        mountPath: /secrets
        readOnly: true
      {{- end }}
      {{- end }}
    {{- if or (not $root.Values.storage.s3.enabled) $root.Values.signing.enabled }}
    volumes:
    {{- if not $root.Values.storage.s3.enabled }}
    - name: data
      {{- if $root.Values.persistence.enabled }}
      persistentVolumeClaim:
        claimName: {{ $root.Values.persistence.existingClaim | default (include "firmirror.fullname" $root) }}
      {{- else }}
      emptyDir: {}
      {{- end }}
    {{- end }}
    {{- if $root.Values.signing.enabled }}
    - name: secrets
      secret:
        secretName: {{ $root.Values.signing.secretName }}
        items:
        - key: {{ $root.Values.signing.certKey }}
          path: signing.cert
        - key: {{ $root.Values.signing.pkeyKey }}
          path: signing.key
    {{- end }}
    {{- end }}
{{- end }}
