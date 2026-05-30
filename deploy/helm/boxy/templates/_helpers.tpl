{{- define "boxy.fullname" -}}
{{- .Release.Name -}}
{{- end }}

{{- define "boxy.sandboxNamespace" -}}
{{- .Values.global.sandboxNamespace | default .Release.Namespace -}}
{{- end }}

{{/*
Build a fully-qualified image reference from a component image config.
Usage: {{ include "boxy.image" (dict "root" . "image" .Values.router.image) }}

Resolution order:
  1. If digest is set:   [registry/]repository@digest
  2. Otherwise:          [registry/]repository:tag  (tag defaults to Chart.AppVersion)
  3. If global.imageRegistry is set it is prepended to repository, enabling
     a single override to redirect every image to a private mirror.
*/}}
{{- define "boxy.image" -}}
{{- $registry := .root.Values.global.imageRegistry -}}
{{- $repo     := .image.repository -}}
{{- $tag      := .image.tag | default .root.Chart.AppVersion -}}
{{- $digest   := .image.digest -}}
{{- $ref      := $repo -}}
{{- if $registry -}}
{{- $ref = printf "%s/%s" $registry $repo -}}
{{- end -}}
{{- if $digest -}}
{{- printf "%s@%s" $ref $digest -}}
{{- else -}}
{{- printf "%s:%s" $ref $tag -}}
{{- end -}}
{{- end }}

{{/*
Render imagePullSecrets block from global.imagePullSecrets list.
Usage: {{- include "boxy.imagePullSecrets" . | nindent 6 }}
Outputs nothing when the list is empty.
*/}}
{{- define "boxy.imagePullSecrets" -}}
{{- if .Values.global.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml .Values.global.imagePullSecrets | nindent 2 }}
{{- end -}}
{{- end }}

{{/*
OpenTelemetry env vars shared by all three binaries. Emits nothing unless the
SDK is disabled or an OTLP endpoint is configured (Prometheus /metrics is always
served regardless). Usage: {{- include "boxy.otelEnv" . | nindent 12 }}
*/}}
{{- define "boxy.otelEnv" -}}
{{- if .Values.metrics.disabled }}
- name: OTEL_SDK_DISABLED
  value: "true"
{{- end }}
{{- if .Values.metrics.otlpEndpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.metrics.otlpEndpoint | quote }}
- name: OTEL_EXPORTER_OTLP_INSECURE
  value: {{ .Values.metrics.otlpInsecure | default false | quote }}
{{- end }}
{{- end }}
