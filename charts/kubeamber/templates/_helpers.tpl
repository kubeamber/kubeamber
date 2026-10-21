{{- define "kubeamber.labels" -}}
app.kubernetes.io/part-of: kubeamber
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "kubeamber.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/* The webhook's own identity, excluded from review. */}}
{{- define "kubeamber.selfUser" -}}
system:serviceaccount:{{ .Release.Namespace }}:kubeamber
{{- end }}

{{/*
The agents webhook's match condition. KubeAmber rewrites it from the policy
at runtime; reuse the live value on upgrade so agents are never unreviewed in
between. On first install it is "false" until KubeAmber starts.
*/}}
{{- define "kubeamber.agentCondition" -}}
{{- $expr := "false" -}}
{{- $live := lookup "admissionregistration.k8s.io/v1" "ValidatingWebhookConfiguration" "" "kubeamber" -}}
{{- if $live -}}
{{- range $live.webhooks -}}
{{- if eq .name "agents.kubeamber.dev" -}}
{{- range .matchConditions -}}
{{- if eq .name "kubeamber-agents" }}{{ $expr = .expression }}{{ end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $expr -}}
{{- end }}

{{/* The CA bundle: given, or the one KubeAmber published at runtime. */}}
{{- define "kubeamber.caBundle" -}}
{{- if .Values.webhook.tls.caBundle -}}
{{ .Values.webhook.tls.caBundle | b64enc }}
{{- else if not .Values.webhook.tls.certManagerCertificate -}}
{{- $live := lookup "admissionregistration.k8s.io/v1" "ValidatingWebhookConfiguration" "" "kubeamber" -}}
{{- if $live -}}{{- with (index $live.webhooks 0).clientConfig.caBundle }}{{ . }}{{ end -}}{{- end -}}
{{- end -}}
{{- end }}
