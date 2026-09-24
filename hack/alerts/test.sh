#!/usr/bin/env bash
# Render the chart's PrometheusRule and test the alerts with promtool.
#
# The alerts are only as good as their behaviour when something is actually wrong, and the
# worst failure is an alert that cannot fire at all — which is what SubnetOperatorDown exists
# to prevent. So the rules are not just linted: alerts_test.yaml feeds them series and checks
# which alerts fire and which stay quiet.
#
# Usage: hack/alerts/test.sh          (needs helm and promtool on PATH, or HELM/PROMTOOL set)
set -euo pipefail
cd "$(dirname "$0")/../.."

helm="${HELM:-bin/helm}"
promtool="${PROMTOOL:-promtool}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Release "r" in namespace "ops": alerts_test.yaml is written against these names.
"$helm" template r charts/aws-subnet-operator -n ops \
  --set prometheusRule.enabled=true \
  --set metrics.serviceMonitor.enabled=true \
  --show-only templates/prometheusrule.yaml > "$work/rule.yaml"

# promtool wants the rule groups on their own, without the PrometheusRule wrapper: everything
# under spec:, one level of indentation less. awk rather than a YAML library, so the test
# needs nothing on the runner beyond helm and promtool.
awk 'found { print substr($0, 3) } /^spec:$/ { found = 1 }' "$work/rule.yaml" > "$work/rules.yaml"

cp hack/alerts/alerts_test.yaml "$work/"
"$promtool" check rules "$work/rules.yaml"
"$promtool" test rules "$work/alerts_test.yaml"
