{{/*
EdgeFlow Chart 通用辅助模板
*/}}

{{/*
展开 Chart 名称：优先使用 nameOverride，否则使用 Chart.yaml 中的 name
*/}}
{{- define "edgeflow.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
生成完整资源名称：优先使用 fullnameOverride，否则由 release 名 + chart 名拼接
*/}}
{{- define "edgeflow.fullname" -}}
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
Chart 标签：helm.sh/chart 用于追踪资源来源
*/}}
{{- define "edgeflow.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
通用选择器标签：用于 Deployment 与 Service 之间的关联
*/}}
{{- define "edgeflow.selectorLabels" -}}
app.kubernetes.io/name: {{ include "edgeflow.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
通用标签集合：选择器标签 + 版本/托管信息
*/}}
{{- define "edgeflow.labels" -}}
{{ include "edgeflow.selectorLabels" . }}
helm.sh/chart: {{ include "edgeflow.chart" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
