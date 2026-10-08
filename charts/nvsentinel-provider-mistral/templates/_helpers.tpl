{{- define "provider.name" -}}
{{- default .Chart.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "provider.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}

{{- define "provider.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "provider.tlsSecret" -}}
{{- default (printf "%s-tls" (include "provider.name" .)) .Values.tls.existingSecret -}}
{{- end -}}

{{- define "provider.validate" -}}
{{- if le (.Values.timeouts.rpcSeconds | int) (.Values.timeouts.httpSeconds | int) -}}
  {{- fail "timeouts.rpcSeconds must exceed timeouts.httpSeconds" -}}
{{- end -}}
{{- if .Values.tls.certManager.enabled -}}
  {{- if .Values.tls.existingSecret -}}
    {{- fail "tls.existingSecret and tls.certManager.enabled are mutually exclusive" -}}
  {{- end -}}
  {{- if not .Values.tls.certManager.issuerRef.name -}}
    {{- fail "tls.certManager.issuerRef.name must name an existing trusted CA-backed issuer" -}}
  {{- end -}}
{{- else if not .Values.tls.existingSecret -}}
  {{- fail "TLS is required: set tls.existingSecret or enable cert-manager with an issuer" -}}
{{- end -}}
{{- end -}}
