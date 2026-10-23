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
The arguments shared by a refresh and a metadata regeneration: storage, signing,
base URL and rollout ring definitions.
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
{{- $bkc := .Values.bkc | default dict }}
{{- if and $bkc.url $bkc.allowHttp }}
- "--bkc.allow-http"
{{- end }}
{{- if .Values.firmwareBaseUrl }}
- {{ printf "--firmware-base-url=%s" .Values.firmwareBaseUrl | quote }}
{{- end }}
{{- if $bkc.url }}
- {{ printf "--bkc.file=%s" $bkc.url | quote }}
{{- end }}
{{- end }}

{{/*
The arguments choosing the vendors to mirror
*/}}
{{- define "firmirror.vendorArgs" -}}
{{- if .Values.vendors.dell.enabled }}
- "--dell.enable"
{{- if .Values.vendors.dell.machinesId }}
- {{ printf "--dell.machines-id=%s" .Values.vendors.dell.machinesId | quote }}
{{- end }}
{{- if .Values.vendors.dell.baseUrl }}
- {{ printf "--dell.base-url=%s" .Values.vendors.dell.baseUrl | quote }}
{{- end }}
{{- end }}
{{- if .Values.vendors.hpe.enabled }}
- "--hpe.enable"
{{- if .Values.vendors.hpe.gens }}
- {{ printf "--hpe.gens=%s" .Values.vendors.hpe.gens | quote }}
{{- end }}
{{- if .Values.vendors.hpe.baseUrl }}
- {{ printf "--hpe.base-url=%s" .Values.vendors.hpe.baseUrl | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The arguments of the refresh
*/}}
{{- define "firmirror.args" -}}
- "refresh"
{{- include "firmirror.commonArgs" . }}
{{- include "firmirror.vendorArgs" . }}
{{- range .Values.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end }}

{{/*
The arguments of the metadata regeneration.
*/}}
{{- define "firmirror.regenArgs" -}}
- "regen-metadata"
{{- include "firmirror.commonArgs" . }}
{{- $regen := (.Values.bkc | default dict).regen | default dict }}
{{- range $regen.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end }}

{{/*
Whether the regen-metadata CronJob exists: it needs storage that outlives a pod.
*/}}
{{- define "firmirror.regenEnabled" -}}
{{- if or .Values.storage.s3.enabled .Values.persistence.enabled }}true{{ end }}
{{- end }}

{{/*
The name of the regen-metadata CronJob. A CronJob name is limited to 52
characters, so the fullname is truncated to leave room for the suffix.
*/}}
{{- define "firmirror.regenName" -}}
{{- printf "%s-regen-metadata" (include "firmirror.fullname" . | trunc 37 | trimSuffix "-") }}
{{- end }}

{{/*
The spec of the Job a CronJob creates, called with the root context (root), the
command arguments (args), backoffLimit, restartPolicy, the ttl of the finished
Job (nil for none) and extra pod labels (labels).
*/}}
{{- define "firmirror.jobSpec" -}}
{{- $root := .root -}}
backoffLimit: {{ .backoffLimit }}
{{- if $root.Values.cronjob.activeDeadlineSeconds }}
activeDeadlineSeconds: {{ $root.Values.cronjob.activeDeadlineSeconds }}
{{- end }}
{{- if not (kindIs "invalid" .ttl) }}
ttlSecondsAfterFinished: {{ .ttl | int64 }}
{{- end }}
template:
  metadata:
    labels:
      {{- /* The labels the selectors use win over podLabels, which would otherwise repeat a key. */}}
      {{- merge (include "firmirror.selectorLabels" $root | fromYaml) (default (dict) .labels) (default (dict) $root.Values.podLabels) | toYaml | nindent 6 }}
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
    restartPolicy: {{ .restartPolicy }}
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
