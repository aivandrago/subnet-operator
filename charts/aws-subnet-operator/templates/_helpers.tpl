{{/* Chart name, overridable. */}}
{{- define "aws-subnet-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified release name. */}}
{{- define "aws-subnet-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "aws-subnet-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "aws-subnet-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: aws-subnet-operator
{{- end -}}

{{- define "aws-subnet-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aws-subnet-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "aws-subnet-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "aws-subnet-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Whether cert-manager issues the webhook certificate: "true" or empty.
"auto" (the default) looks for the cert-manager API in the cluster, which is what a chart can
honestly know about it. A plain true or false decides it without asking.
*/}}
{{- define "aws-subnet-operator.webhook.certManager" -}}
{{- $setting := .Values.webhook.certificate.certManager -}}
{{- if kindIs "bool" $setting -}}
{{- if $setting }}true{{ end -}}
{{- else if eq (lower (toString $setting)) "auto" -}}
{{- if .Capabilities.APIVersions.Has "cert-manager.io/v1" }}true{{ end -}}
{{- else if eq (lower (toString $setting)) "true" -}}
true
{{- end -}}
{{- end -}}

{{/*
The webhook serving certificate when cert-manager is not there, as ca/cert/key in base64.

An upgrade reads the certificate back out of the secret instead of signing a new one: the CA
is written into the webhook configurations, so regenerating it on every upgrade would leave
the API server trusting a CA the operator no longer serves until the next rollout.
*/}}
{{- define "aws-subnet-operator.webhook.selfSignedCert" -}}
{{- $root := .root -}}
{{- $service := .service -}}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace .secret -}}
{{- $data := default dict (get (default dict $existing) "data") -}}
{{- if and (get $data "ca.crt") (get $data "tls.crt") (get $data "tls.key") -}}
ca: {{ get $data "ca.crt" }}
cert: {{ get $data "tls.crt" }}
key: {{ get $data "tls.key" }}
{{- else -}}
{{- $altNames := list (printf "%s.%s.svc" $service $root.Release.Namespace) (printf "%s.%s.svc.cluster.local" $service $root.Release.Namespace) -}}
{{- $days := 3650 -}}
{{- $ca := genCA (printf "%s-ca" $service) $days -}}
{{- $cert := genSignedCert (first $altNames) nil $altNames $days $ca -}}
ca: {{ $ca.Cert | b64enc }}
cert: {{ $cert.Cert | b64enc }}
key: {{ $cert.Key | b64enc }}
{{- end -}}
{{- end -}}
