{{/* Chart name, overridable. */}}
{{- define "subnet-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified release name. */}}
{{- define "subnet-operator.fullname" -}}
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

{{- define "subnet-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "subnet-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: subnet-operator
{{- end -}}

{{/*
The app.kubernetes.io/name value of the selector labels. A Deployment's selector cannot change,
and until 0.7 this chart was called aws-subnet-operator: a release whose name keeps its
Deployment's name across the rename (a release called aws-subnet-operator, or one installed with
fullnameOverride) keeps the name label its Deployment was created with, read back from the
cluster, so that helm upgrade does not fail on an immutable field. A fresh install, a release
whose objects are renamed anyway, and helm template use the chart's name.
*/}}
{{- define "subnet-operator.selectorName" -}}
{{- include "subnet-operator.existingSelectorName" (dict "root" . "deployment" (include "subnet-operator.fullname" .) "name" (include "subnet-operator.name" .)) -}}
{{- end -}}

{{- define "subnet-operator.existingSelectorName" -}}
{{- $name := .name -}}
{{- if not .root.Values.nameOverride -}}
{{- $existing := lookup "apps/v1" "Deployment" .root.Release.Namespace .deployment -}}
{{- $selected := dig "spec" "selector" "matchLabels" "app.kubernetes.io/name" "" (default dict $existing) -}}
{{- if $selected -}}
{{- $name = $selected -}}
{{- end -}}
{{- end -}}
{{- $name -}}
{{- end -}}

{{- define "subnet-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "subnet-operator.selectorName" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "subnet-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "subnet-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Whether cert-manager issues the webhook certificate: "true" or empty.
"auto" (the default) looks for the cert-manager API in the cluster, which is what a chart can
honestly know about it. A plain true or false decides it without asking.
*/}}
{{- define "subnet-operator.webhook.certManager" -}}
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
{{- define "subnet-operator.webhook.selfSignedCert" -}}
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

{{/*
The dashboard is a workload of its own, with its own name label. Sharing the operator's
selector labels would put its pods behind the operator's metrics and webhook Services, under
the operator's PodDisruptionBudget and NetworkPolicy.
*/}}
{{- define "subnet-operator.dashboard.fullname" -}}
{{- printf "%s-dashboard" (include "subnet-operator.fullname" . | trunc 53 | trimSuffix "-") -}}
{{- end -}}

{{- define "subnet-operator.dashboard.selectorLabels" -}}
{{- $name := printf "%s-dashboard" (include "subnet-operator.name" . | trunc 53 | trimSuffix "-") -}}
app.kubernetes.io/name: {{ include "subnet-operator.existingSelectorName" (dict "root" . "deployment" (include "subnet-operator.dashboard.fullname" .) "name" $name) }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "subnet-operator.dashboard.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "subnet-operator.dashboard.selectorLabels" . }}
app.kubernetes.io/component: dashboard
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: subnet-operator
{{- end -}}

{{/*
The API groups the webhooks serve: the current one, and in 0.8 the deprecated one, whose
objects the operator migrates and whose webhooks keep the old promises until then.
*/}}
{{- define "subnet-operator.webhook.apis" -}}
- group: network.hypersurgery.dev
  version: v1beta1
  path: network-hypersurgery-dev-v1beta1
- group: aws.hypersurgery
  version: v1alpha1
  path: aws-hypersurgery-v1alpha1
{{- end -}}
