{{- define "boxy.fullname" -}}
{{- .Release.Name -}}
{{- end }}

{{- define "boxy.sandboxNamespace" -}}
{{- .Values.sandboxNamespace | default .Release.Namespace -}}
{{- end }}
