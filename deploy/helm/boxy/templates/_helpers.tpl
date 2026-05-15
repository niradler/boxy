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
