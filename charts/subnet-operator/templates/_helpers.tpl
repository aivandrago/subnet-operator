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
The API groups and versions the webhooks serve.
*/}}
{{- define "subnet-operator.webhook.apis" -}}
- group: network.hypersurgery.dev
  version: v1
  path: network-hypersurgery-dev-v1
{{- end -}}

{{/*
The providers the operator runs with, as the --providers flag takes them. At least one must be
enabled: the operator refuses to start without.
*/}}
{{- define "subnet-operator.providers" -}}
{{- $enabled := list -}}
{{- if .Values.providers.aws.enabled -}}
{{- $enabled = append $enabled "aws" -}}
{{- end -}}
{{- if not $enabled -}}
{{- fail "enable at least one provider under providers (only providers.aws exists in this release)" -}}
{{- end -}}
{{- join "," $enabled -}}
{{- end -}}

{{/* AWS settings, all under providers.aws. */}}
{{- define "subnet-operator.aws.eventsQueueUrl" -}}
{{- .Values.providers.aws.events.queueUrl | default "" -}}
{{- end -}}
{{- define "subnet-operator.aws.eventsDebounce" -}}
{{- .Values.providers.aws.events.debounce | default "10s" -}}
{{- end -}}
{{- define "subnet-operator.aws.region" -}}
{{- .Values.providers.aws.region | default "" -}}
{{- end -}}
{{- define "subnet-operator.aws.podIdentity" -}}
{{- $p := default dict .Values.providers.aws.podIdentity -}}
enabled: {{ hasKey $p "enabled" | ternary $p.enabled true }}
cidr: {{ $p.cidr | default "169.254.170.23/32" }}
port: {{ $p.port | default 80 }}
{{- end -}}
{{- define "subnet-operator.aws.endpointURL" -}}
{{- .Values.providers.aws.endpointURL | default "" -}}
{{- end -}}

{{/*
Values and manager flags deprecated in 0.9 and removed in 1.0. Rendered as they are, they would
be ignored without a word: the operator would run without its event queue, in another region
or against AWS instead of an emulator, or the NetworkPolicy would cut it off from the Pod
Identity agent. Refused instead, naming the replacement. Empty strings are what 0.9's own
values.yaml had, which `helm upgrade --reuse-values` carries over, so only a value that is set
counts.
*/}}
{{- define "subnet-operator.removedValues" -}}
{{- $moved := dict
    "events.queueUrl" (dig "queueUrl" "" (default dict .Values.events))
    "events.debounce" (dig "debounce" "" (default dict .Values.events))
    "aws.region" (dig "region" "" (default dict .Values.aws))
    "aws.endpointURL" (dig "endpointURL" "" (default dict .Values.aws)) -}}
{{- range $old := keys $moved | sortAlpha -}}
{{- if get $moved $old -}}
{{- fail (printf "%s was removed in 1.0; use providers.aws.%s" $old (trimPrefix "aws." $old)) -}}
{{- end -}}
{{- end -}}
{{- if dig "egress" "podIdentity" nil (default dict .Values.networkPolicy) -}}
{{- fail "networkPolicy.egress.podIdentity was removed in 1.0; use providers.aws.podIdentity (the same keys: enabled, cidr, port)" -}}
{{- end -}}
{{- range .Values.extraArgs -}}
{{- $arg := toString . -}}
{{- range $old := list "--events-queue-url" "--events-debounce" -}}
{{- if or (eq $arg $old) (hasPrefix (printf "%s=" $old) $arg) -}}
{{- fail (printf "extraArgs: %s was removed in 1.0; use providers.aws.events.%s (or --aws-%s)" $old (eq $old "--events-queue-url" | ternary "queueUrl" "debounce") (trimPrefix "--" $old)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range .Values.extraEnv -}}
{{- if eq (toString .name) "EVENTS_QUEUE_URL" -}}
{{- fail "extraEnv: EVENTS_QUEUE_URL was removed in 1.0; use providers.aws.events.queueUrl" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Service account annotations: each enabled provider's identity for the operator, then
serviceAccount.annotations, which win.
*/}}
{{- define "subnet-operator.serviceAccountAnnotations" -}}
{{- $annotations := dict -}}
{{- if and .Values.providers.aws.enabled .Values.providers.aws.irsaRoleARN -}}
{{- $_ := set $annotations "eks.amazonaws.com/role-arn" .Values.providers.aws.irsaRoleARN -}}
{{- end -}}
{{- $annotations = merge (deepCopy (default dict .Values.serviceAccount.annotations)) $annotations -}}
{{- if $annotations -}}
{{- toYaml $annotations | trim -}}
{{- end -}}
{{- end -}}
