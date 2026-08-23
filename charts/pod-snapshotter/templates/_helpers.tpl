{{/*
Fully-qualified image reference for a component.

Every image the chart ships lives in one registry in any real deployment, but
the per-component repository fields carry only the path ("pod-snapshotter/agent").
Without a shared prefix the chart renders references that resolve against Docker
Hub, which is not where these images are -- so a `helm upgrade` against a
cluster that was bootstrapped by hand would swap working images for ones that
do not exist. imageRegistry is that prefix, applied once, here.

Usage: {{ include "pod-snapshotter.image" (dict "img" .Values.agent.image "root" $) }}
*/}}
{{- define "pod-snapshotter.image" -}}
{{- $reg := .root.Values.imageRegistry | default "" -}}
{{- if $reg -}}
{{ trimSuffix "/" $reg }}/{{ .img.repository }}:{{ .img.tag }}
{{- else -}}
{{ .img.repository }}:{{ .img.tag }}
{{- end -}}
{{- end -}}
