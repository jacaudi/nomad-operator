{{- define "nomad-operator.labels" -}}
app.kubernetes.io/name: nomad-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: Helm
app.kubernetes.io/component: operator
{{- end -}}
