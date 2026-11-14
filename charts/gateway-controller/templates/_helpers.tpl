{{- define "gc.name" -}}{{ .Release.Name | trunc 50 | trimSuffix "-" }}{{- end -}}
{{- define "gc.labels" -}}
app.kubernetes.io/name: gateway-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}
{{- /* (list $ .Values.image) */ -}}
{{- define "gc.image" -}}
{{- $i := index . 1 -}}
{{- if $i.digest -}}{{ $i.repository }}@{{ $i.digest }}{{- else -}}{{ $i.repository }}:{{ default (index . 0).Chart.Version $i.tag }}{{- end -}}
{{- end -}}
