{{- define "tc.name" -}}{{ .Release.Name | trunc 50 | trimSuffix "-" }}{{- end -}}
{{- define "tc.stateSecret" -}}{{ default (printf "%s-state" (include "tc.name" .)) .Values.connector.stateSecret }}{{- end -}}
{{- define "tc.labels" -}}
app.kubernetes.io/name: tailnet-connector
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
{{- /* (list $ .Values.image) */ -}}
{{- define "tc.image" -}}
{{- $i := index . 1 -}}
{{- if $i.digest -}}{{ $i.repository }}@{{ $i.digest }}{{- else -}}{{ $i.repository }}:{{ default (index . 0).Chart.Version $i.tag }}{{- end -}}
{{- end -}}
{{- define "tc.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: { drop: [ALL] }
{{- end -}}
