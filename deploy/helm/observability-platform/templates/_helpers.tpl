{{- define "obs.labels" -}}
app.kubernetes.io/part-of: observability-platform
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}