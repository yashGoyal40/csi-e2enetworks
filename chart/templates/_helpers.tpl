{{/* Common labels */}}
{{- define "csi-e2enetworks.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "csi-e2enetworks.controllerSelector" -}}
app: {{ .Release.Name }}-controller
{{- end }}

{{- define "csi-e2enetworks.nodeSelector" -}}
app: {{ .Release.Name }}-node
{{- end }}

{{- define "csi-e2enetworks.driverName" -}}
csi.e2enetworks.com
{{- end }}

{{/* Resolve credentials Secret name */}}
{{- define "csi-e2enetworks.secretName" -}}
{{- if .Values.existingSecret -}}
{{ .Values.existingSecret }}
{{- else -}}
{{ .Release.Name }}-creds
{{- end -}}
{{- end }}
