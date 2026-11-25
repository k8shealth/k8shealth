{{/*
Expand the name of the chart.
*/}}
{{- define "helm-chart.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "helm-chart.fullname" -}}
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
{{- define "helm-chart.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "helm-chart.labels" -}}
helm.sh/chart: {{ include "helm-chart.chart" . }}
{{ include "helm-chart.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "helm-chart.selectorLabels" -}}
app.kubernetes.io/name: {{ include "helm-chart.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "helm-chart.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "helm-chart.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The first-run setup code (proof that the first administrator controls this cluster). Made once per render and shared by
templates/setup-code.yaml and NOTES.txt, so the code printed at the end of the install is the one stored in the Secret.
Kept on upgrade (read back from the Secret). No look-alike characters (0/O, 1/I/L): it is typed from a terminal.
*/}}
{{- define "helm-chart.setupCode" -}}
{{- if not (hasKey .Values "_setupCode") -}}
{{- $name := printf "%s-setup" (include "helm-chart.fullname" .) -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace $name -}}
{{- $code := "" -}}
{{- if and $existing $existing.data (hasKey $existing.data "code") -}}
{{- $code = index $existing.data "code" | b64dec -}}
{{- else -}}
{{- $pool := upper (randAlphaNum 80) -}}
{{- $clean := regexReplaceAll "[01OIL]" $pool "" -}}
{{- $code = printf "%s-%s-%s" (substr 0 4 $clean) (substr 4 8 $clean) (substr 8 12 $clean) -}}
{{- end -}}
{{- $_ := set .Values "_setupCode" $code -}}
{{- end -}}
{{- index .Values "_setupCode" -}}
{{- end }}