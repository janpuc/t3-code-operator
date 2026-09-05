{{- define "t3-code-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "t3-code-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "t3-code-operator.name" . -}}
{{- if eq .Release.Name $name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
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

{{- define "t3-code-operator.operatorRoleName" -}}
{{- printf "%s-operator" (include "t3-code-operator.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "t3-code-operator.image" -}}
{{- if .Values.operator.image.digest -}}
{{- printf "%s@%s" .Values.operator.image.repository .Values.operator.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.operator.image.repository (required "operator.image.tag is required when digest is empty" .Values.operator.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "t3-code-operator.smbImage" -}}
{{- if .Values.smb.image.digest -}}
{{- printf "%s@%s" .Values.smb.image.repository .Values.smb.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.smb.image.repository (required "smb.image.tag is required when digest is empty" .Values.smb.image.tag) -}}
{{- end -}}
{{- end -}}

{{- define "t3-code-operator.workstationImage" -}}
{{- if .Values.workstation.image.digest -}}
{{- printf "%s@%s" .Values.workstation.image.repository .Values.workstation.image.digest -}}
{{- end -}}
{{- end -}}

{{- define "t3-code-operator.secretResourceNames" -}}
{{- $names := list -}}
{{- range .Values.rbac.secretResourceNames -}}
{{- $names = append $names . -}}
{{- end -}}
{{- range .Values.workstations -}}
{{- $credential := dig "spec" "git" "credentialSecretRef" "name" "" . -}}
{{- if $credential -}}
{{- $names = append $names $credential -}}
{{- end -}}
{{- $signing := dig "spec" "git" "signingKeySecretRef" "name" "" . -}}
{{- if $signing -}}
{{- $names = append $names $signing -}}
{{- end -}}
{{- range $provider := (dig "spec" "providers" (dict) .) -}}
{{- range (dig "environment" (list) $provider) -}}
{{- $name := dig "valueFrom" "secretKeyRef" "name" "" . -}}
{{- if $name -}}
{{- $names = append $names $name -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.harnesses -}}
{{- range (dig "spec" "environment" (list) .) -}}
{{- $name := dig "valueFrom" "secretKeyRef" "name" "" . -}}
{{- if $name -}}
{{- $names = append $names $name -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.extensions -}}
{{- $git := dig "spec" "source" "git" "credentialSecretRef" "name" "" . -}}
{{- if $git -}}
{{- $names = append $names $git -}}
{{- end -}}
{{- $oci := dig "spec" "source" "oci" "credentialSecretRef" "name" "" . -}}
{{- if $oci -}}
{{- $names = append $names $oci -}}
{{- end -}}
{{- $marketplace := dig "spec" "source" "marketplace" "credentialSecretRef" "name" "" . -}}
{{- if $marketplace -}}
{{- $names = append $names $marketplace -}}
{{- end -}}
{{- $release := dig "spec" "source" "githubRelease" "credentialSecretRef" "name" "" . -}}
{{- if $release -}}
{{- $names = append $names $release -}}
{{- end -}}
{{- end -}}
{{- range .Values.mcpServers -}}
{{- $bearer := dig "spec" "bearerTokenSecretRef" "name" "" . -}}
{{- if $bearer -}}
{{- $names = append $names $bearer -}}
{{- end -}}
{{- range (dig "spec" "environment" (list) .) -}}
{{- $name := dig "valueFrom" "secretKeyRef" "name" "" . -}}
{{- if $name -}}
{{- $names = append $names $name -}}
{{- end -}}
{{- end -}}
{{- range (dig "spec" "headers" (list) .) -}}
{{- $name := dig "valueFrom" "secretKeyRef" "name" "" . -}}
{{- if $name -}}
{{- $names = append $names $name -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toYaml (sortAlpha (uniq $names)) -}}
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
