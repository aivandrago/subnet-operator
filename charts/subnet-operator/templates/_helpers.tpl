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
The Secret the webhook serving certificate is in: webhook.certificate.existingSecret, or the
one the chart (or cert-manager) writes. dig: values from before 2.0.1 have no existingSecret.
*/}}
{{- define "subnet-operator.webhook.secretName" -}}
{{- default (printf "%s-webhook-cert" (include "subnet-operator.fullname" .)) (dig "certificate" "existingSecret" "" .Values.webhook) -}}
{{- end -}}

{{/*
Seconds in a Go duration such as "8760h". mustDateModify parses it with time.ParseDuration,
which is the only duration parser a template has.
*/}}
{{- define "subnet-operator.durationSeconds" -}}
{{- $now := now -}}
{{- sub (mustDateModify (printf "+%s" (toString .)) $now).Unix $now.Unix -}}
{{- end -}}

{{/*
The first certificate of a PEM bundle.
*/}}
{{- define "subnet-operator.firstCertificate" -}}
{{- regexFind "(?s)-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----" . -}}
{{- end -}}

{{/*
The webhook serving certificate when cert-manager is not there, as ca/cert/key in base64 and
the annotations the Secret carries.

The certificate lives webhook.certificate.duration (whole days) and is signed again on the
first upgrade within renewBefore of its expiry. A template cannot parse a certificate, so the
chart records when it signed one in annotations on the Secret and decides from those:

- an upgrade reads the certificate back out of the Secret while it has more than renewBefore
  left and does not outlive the configured duration;
- otherwise, and when the Secret has no record (every certificate before 2.0.1, which lived
  10 years), it signs a new CA and certificate.

A new CA is written into the webhook configurations while the running pods may still serve
the old certificate for the minute or so the kubelet takes to update the mounted Secret. So
ca.crt carries the new CA followed by the previous one, and the previous one is dropped by the
first upgrade after previous-ca-until (an hour on). The operator reads the same ca.crt into
its CRDs' conversion webhook, so the CRDs follow both changes within a minute.

helm template (Argo CD, Flux) has no lookup: every render signs a new CA, as it always did.
Use cert-manager or existingSecret there.
*/}}
{{- define "subnet-operator.webhook.selfSignedCert" -}}
{{- $root := .root -}}
{{- $service := .service -}}
{{- $prefix := "network.hypersurgery.dev/webhook-" -}}
{{- $layout := "2006-01-02T15:04:05Z07:00" -}}
{{- $now := now -}}
{{- $days := max 1 (div (include "subnet-operator.durationSeconds" $root.Values.webhook.certificate.duration | atoi) 86400) -}}
{{- $validity := mul $days 86400 -}}
{{- $renewBefore := include "subnet-operator.durationSeconds" $root.Values.webhook.certificate.renewBefore | atoi -}}
{{- if ge $renewBefore $validity -}}
{{- fail (printf "webhook.certificate.renewBefore (%s) must be shorter than webhook.certificate.duration (%s, %d days for the chart-signed certificate)" (toString $root.Values.webhook.certificate.renewBefore) (toString $root.Values.webhook.certificate.duration) $days) -}}
{{- end -}}
{{- $existing := default dict (lookup "v1" "Secret" $root.Release.Namespace .secret) -}}
{{- $data := default dict (get $existing "data") -}}
{{- $annotations := default dict (dig "metadata" "annotations" dict $existing) -}}
{{- $notBefore := (toDate $layout (default "" (get $annotations (print $prefix "not-before")))).Unix -}}
{{- $notAfter := (toDate $layout (default "" (get $annotations (print $prefix "not-after")))).Unix -}}
{{- $reuse := and (get $data "ca.crt") (get $data "tls.crt") (get $data "tls.key") -}}
{{- $reuse = and $reuse (gt $notBefore 0) (gt $notAfter 0) -}}
{{- $reuse = and $reuse (gt (sub $notAfter $now.Unix) $renewBefore) -}}
{{- /* A day of slack for clocks: the record is written when the chart signs. */ -}}
{{- $reuse = and $reuse (le (sub $notAfter $notBefore) (add $validity 86400)) -}}
{{- $out := dict -}}
{{- if $reuse -}}
{{- $ca := get $data "ca.crt" -}}
{{- $previousUntil := get $annotations (print $prefix "previous-ca-until") -}}
{{- if and $previousUntil (gt (toDate $layout $previousUntil).Unix $now.Unix) -}}
{{- $_ := set $out "previousCAUntil" $previousUntil -}}
{{- else if $previousUntil -}}
{{- $ca = printf "%s\n" (include "subnet-operator.firstCertificate" (b64dec $ca)) | b64enc -}}
{{- end -}}
{{- $_ := set $out "ca" $ca -}}
{{- $_ := set $out "cert" (get $data "tls.crt") -}}
{{- $_ := set $out "key" (get $data "tls.key") -}}
{{- $_ := set $out "notBefore" (get $annotations (print $prefix "not-before")) -}}
{{- $_ := set $out "notAfter" (get $annotations (print $prefix "not-after")) -}}
{{- else -}}
{{- $altNames := list (printf "%s.%s.svc" $service $root.Release.Namespace) (printf "%s.%s.svc.cluster.local" $service $root.Release.Namespace) -}}
{{- $ca := genCA (printf "%s-ca" $service) (int $days) -}}
{{- $cert := genSignedCert (first $altNames) nil $altNames (int $days) $ca -}}
{{- $bundle := $ca.Cert -}}
{{- $previous := include "subnet-operator.firstCertificate" (b64dec (default "" (get $data "ca.crt"))) -}}
{{- if $previous -}}
{{- $bundle = printf "%s%s\n" $bundle $previous -}}
{{- $_ := set $out "previousCAUntil" (dateInZone $layout (dateModify "+1h" $now) "UTC") -}}
{{- end -}}
{{- $_ := set $out "ca" ($bundle | b64enc) -}}
{{- $_ := set $out "cert" ($cert.Cert | b64enc) -}}
{{- $_ := set $out "key" ($cert.Key | b64enc) -}}
{{- $_ := set $out "notBefore" (dateInZone $layout $now "UTC") -}}
{{- $_ := set $out "notAfter" (dateInZone $layout (dateModify (printf "+%dh" (mul $days 24)) $now) "UTC") -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
The CA bundle for a certificate in webhook.certificate.existingSecret, base64: caBundle when
set, else ca.crt from the Secret. helm template cannot read the Secret, so there caBundle is
required.
*/}}
{{- define "subnet-operator.webhook.existingCABundle" -}}
{{- $pem := dig "certificate" "caBundle" "" .Values.webhook -}}
{{- if $pem -}}
{{- $pem | b64enc -}}
{{- else -}}
{{- $secret := lookup "v1" "Secret" .Release.Namespace (include "subnet-operator.webhook.secretName" .) -}}
{{- $ca := dig "data" "ca.crt" "" (default dict $secret) -}}
{{- if not $ca -}}
{{- fail (printf "webhook.certificate.existingSecret: no ca.crt in Secret %s/%s to put into the webhook configurations; create the Secret with ca.crt first, or set webhook.certificate.caBundle (PEM), which helm template needs because it cannot read Secrets" .Release.Namespace (include "subnet-operator.webhook.secretName" .)) -}}
{{- end -}}
{{- $ca -}}
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
{{- if (.Values.providers.gcp).enabled -}}
{{- $enabled = append $enabled "gcp" -}}
{{- end -}}
{{- if not $enabled -}}
{{- fail "enable at least one provider under providers (providers.aws or providers.gcp)" -}}
{{- end -}}
{{- join "," $enabled -}}
{{- end -}}

{{/* GCP change events: the subscription, empty unless providers.gcp is enabled. */}}
{{- define "subnet-operator.gcp.eventsSubscription" -}}
{{- $gcp := default dict .Values.providers.gcp -}}
{{- $s := dig "events" "subscription" "" $gcp -}}
{{- if and $gcp.enabled $s -}}
{{- if not (regexMatch "^projects/[a-z][a-z0-9.:-]*[a-z0-9]/subscriptions/[A-Za-z][A-Za-z0-9._~+%-]{2,254}$" $s) -}}
{{- fail (printf "providers.gcp.events.subscription %q is not projects/<project>/subscriptions/<name>" $s) -}}
{{- end -}}
{{- $s -}}
{{- end -}}
{{- end -}}
{{- define "subnet-operator.gcp.eventsDebounce" -}}
{{- dig "events" "debounce" "" (default dict .Values.providers.gcp) | default "10s" -}}
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
GCP settings, all under providers.gcp: the operator's own Google identity, and the egress it
needs. Every helper is empty or off while providers.gcp.enabled is false.

subnet-operator.gcp.wif is the Workload Identity Federation setup as YAML: enabled, and when
it is, where the credential configuration comes from (render: the chart makes it from
wif.provider; configMap/secret: the user's), the key it is under, and the projected token's
audience, lifetime, directory and file name. Settings that cannot work are refused here.
*/}}
{{- define "subnet-operator.gcp.wif" -}}
{{- $gcp := default dict .Values.providers.gcp -}}
{{- $w := default dict $gcp.wif -}}
{{- $cc := default dict $w.credentialConfig -}}
{{- $provider := $w.provider | default "" -}}
{{- $external := or $cc.configMap $cc.secret -}}
{{- $enabled := and $gcp.enabled (or $provider $external) -}}
enabled: {{ if $enabled }}true{{ else }}false{{ end }}
{{- if $enabled }}
{{- if and $cc.configMap $cc.secret }}
{{- fail "providers.gcp.wif.credentialConfig: name a configMap or a secret, not both" }}
{{- end }}
{{- if and $provider (not (regexMatch "^projects/[0-9]+/locations/global/workloadIdentityPools/[a-z0-9-]+/providers/[a-z0-9-]+$" $provider)) }}
{{- fail (printf "providers.gcp.wif.provider %q is not projects/<project number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>" $provider) }}
{{- end }}
{{- if and $w.serviceAccount $external }}
{{- fail "providers.gcp.wif.serviceAccount only applies to the credential configuration the chart renders from wif.provider; a configuration of your own names its service account itself (service_account_impersonation_url)" }}
{{- end }}
{{- if and $w.serviceAccount (not (regexMatch `^[^@]+@[^@]+\.gserviceaccount\.com$` $w.serviceAccount)) }}
{{- fail (printf "providers.gcp.wif.serviceAccount %q is not a service account email" $w.serviceAccount) }}
{{- end }}
{{- $audience := $w.audience | default (and $provider (printf "https://iam.googleapis.com/%s" $provider)) | default "" }}
{{- if not $audience }}
{{- fail "providers.gcp.wif.credentialConfig needs providers.gcp.wif.audience (or wif.provider): the audience the workload identity pool provider accepts on the projected token" }}
{{- end }}
{{- $tokenPath := $w.tokenPath | default "/var/run/secrets/gcp-wif/token" }}
{{- if or (not (hasPrefix "/" $tokenPath)) (eq (base $tokenPath) "") (eq (dir $tokenPath) "/") }}
{{- fail (printf "providers.gcp.wif.tokenPath %q must be an absolute file path below a directory of its own" $tokenPath) }}
{{- end }}
render: {{ not $external }}
configMap: {{ $cc.configMap | default "" | quote }}
secret: {{ $cc.secret | default "" | quote }}
key: {{ $cc.key | default "credential-configuration.json" | quote }}
provider: {{ $provider | quote }}
serviceAccount: {{ $w.serviceAccount | default "" | quote }}
audience: {{ $audience | quote }}
expirationSeconds: {{ $w.tokenExpirationSeconds | default 3600 | int }}
tokenPath: {{ $tokenPath | quote }}
tokenDir: {{ dir $tokenPath | quote }}
tokenFile: {{ base $tokenPath | quote }}
{{- end }}
{{- end -}}

{{/* Where the operator reads the credential configuration of Workload Identity Federation. */}}
{{- define "subnet-operator.gcp.credentialConfigDir" -}}
/etc/gcp-wif
{{- end -}}

{{/*
GKE Workload Identity: the Google service account the Kubernetes service account acts as,
refused next to Workload Identity Federation, which replaces it.
*/}}
{{- define "subnet-operator.gcp.workloadIdentity" -}}
{{- $gcp := default dict .Values.providers.gcp -}}
{{- $sa := dig "workloadIdentity" "serviceAccount" "" $gcp -}}
{{- if and $gcp.enabled $sa -}}
{{- if not (regexMatch `^[^@]+@[^@]+\.gserviceaccount\.com$` $sa) -}}
{{- fail (printf "providers.gcp.workloadIdentity.serviceAccount %q is not a service account email" $sa) -}}
{{- end -}}
{{- if (include "subnet-operator.gcp.wif" . | fromYaml).enabled -}}
{{- fail "providers.gcp: use GKE Workload Identity (workloadIdentity) or Workload Identity Federation (wif), not both" -}}
{{- end -}}
{{- $sa -}}
{{- end -}}
{{- end -}}

{{/*
Egress to the GKE metadata server, which hands out the credentials of GKE Workload Identity:
on by default with GCP, except with Workload Identity Federation, which needs no metadata
server. On other clouds the same link-local address is the node's instance metadata service,
which the operator has no business reaching.
*/}}
{{- define "subnet-operator.gcp.metadataServer" -}}
{{- $gcp := default dict .Values.providers.gcp -}}
{{- $m := default dict $gcp.metadataServer -}}
{{- $wif := (include "subnet-operator.gcp.wif" . | fromYaml).enabled -}}
{{- $default := and $gcp.enabled (not $wif) -}}
enabled: {{ if and $gcp.enabled (hasKey $m "enabled" | ternary $m.enabled $default) }}true{{ else }}false{{ end }}
endpoints:
{{- $endpoints := $m.endpoints | default (list (dict "cidr" "169.254.169.254/32" "port" 80) (dict "cidr" "169.254.169.252/32" "port" 988)) }}
{{- range $endpoints }}
  - cidr: {{ .cidr }}
    port: {{ .port }}
{{- end }}
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
{{- with include "subnet-operator.gcp.workloadIdentity" . -}}
{{- $_ := set $annotations "iam.gke.io/gcp-service-account" . -}}
{{- end -}}
{{- $annotations = merge (deepCopy (default dict .Values.serviceAccount.annotations)) $annotations -}}
{{- if $annotations -}}
{{- toYaml $annotations | trim -}}
{{- end -}}
{{- end -}}
