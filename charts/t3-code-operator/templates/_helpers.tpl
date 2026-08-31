{{- define "t3-code-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "t3-code-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "t3-code-operator.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "t3-code-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "t3-code-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "t3-code-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "t3-code-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: operator
{{- end -}}

{{- define "t3-code-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "t3-code-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "t3-code-operator.image" -}}
{{- if .Values.operator.image.digest -}}
{{- printf "%s@%s" .Values.operator.image.repository .Values.operator.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.operator.image.repository (required "operator.image.tag is required when digest is empty" .Values.operator.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "t3-code-operator.projectMetadata" -}}
name: {{ .resource.name | quote }}
namespace: {{ default .root.Release.Namespace .resource.namespace | quote }}
labels:
  {{- include "t3-code-operator.labels" .root | nindent 2 }}
  {{- with .resource.labels }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- with .resource.annotations }}
annotations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
