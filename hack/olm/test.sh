#!/usr/bin/env bash
# Installs the OLM bundle into a Kind cluster the way an OperatorHub user gets it, through OLM,
# and checks what the bundle is responsible for:
#   - the ClusterServiceVersion installs (OLM accepts the CSV, its permissions and webhooks);
#   - the admission webhooks answer with the certificate OLM issued and injected;
#   - the CRDs convert through the conversion webhook OLM configured, and the operator leaves
#     that configuration to OLM (--crd-conversion unset);
#   - scorecard's basic and OLM suites pass.
#
# Run it through `make test-olm`, which creates the Kind cluster first and deletes it after.
# The operator and bundle images go to a registry container on the Kind network: OLM pulls the
# bundle image itself (it is not enough to load it into the node), and the image references in
# the bundle must resolve inside the cluster.
set -euo pipefail

: "${KIND:?}" "${KUBECTL:?}" "${OPERATOR_SDK:?}" "${KIND_CLUSTER:?}" "${VERSION:?}"
: "${OLM_VERSION:?}" "${OLM_CRDS_SHA256:?}" "${OLM_SHA256:?}" "${REGISTRY_IMAGE:?}" "${OPM_IMAGE:?}"

ns=subnet-operator-system
registry="${KIND_CLUSTER}-registry"
work="$(mktemp -d)"
kubectl() { "$KUBECTL" "$@"; }
step() { printf '\n=== %s\n' "$*"; }

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		step "Diagnostics"
		kubectl get csv,subscription,installplan,catalogsource,pods -n "$ns" -o wide || true
		kubectl describe csv -n "$ns" || true
		kubectl logs -n "$ns" deployment/subnet-operator-controller-manager --tail=100 || true
		kubectl get events -n "$ns" --sort-by=.lastTimestamp | tail -40 || true
		kubectl logs -n olm deployment/olm-operator --tail=60 || true
		kubectl logs -n olm deployment/catalog-operator --tail=60 || true
	fi
	docker rm -f "$registry" >/dev/null 2>&1 || true
	[ -n "${operator_local:-}" ] && docker rmi "$operator_local" >/dev/null 2>&1 || true
	[ -n "${bundle_local:-}" ] && docker rmi "$bundle_local" >/dev/null 2>&1 || true
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

step "Starting a registry on the Kind network"
docker rm -f "$registry" >/dev/null 2>&1 || true
docker run -d --name "$registry" --network kind -p 127.0.0.1::5000 "$REGISTRY_IMAGE" >/dev/null
# Pushed from here through the published port, pulled in the cluster by the container's IP.
host="$(docker port "$registry" 5000/tcp | head -1)"
ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$registry")"
in_cluster="$ip:5000"
# containerd pulls from it over plain HTTP (Kind node images read /etc/containerd/certs.d).
for node in $("$KIND" get nodes --name "$KIND_CLUSTER"); do
	docker exec "$node" mkdir -p "/etc/containerd/certs.d/$in_cluster"
	printf 'server = "http://%s"\n\n[host."http://%s"]\n  capabilities = ["pull", "resolve"]\n' \
		"$in_cluster" "$in_cluster" |
		docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/$in_cluster/hosts.toml"
done
for _ in $(seq 30); do
	curl -fsS -o /dev/null "http://$host/v2/" && break
	sleep 1
done

step "Building and pushing the operator image"
operator_local="$host/subnet-operator:olm-$KIND_CLUSTER"
make docker-build IMG="$operator_local"
docker push "$operator_local"
digest="$(docker inspect --format '{{index .RepoDigests 0}}' "$operator_local")"
digest="${digest##*@}"
# By digest, as a release does: the bundle then pins it and lists it in relatedImages.
operator_image="$in_cluster/subnet-operator@$digest"

step "Generating, validating, building and pushing the bundle"
make bundle-validate VERSION="$VERSION" BUNDLE_OPERATOR_IMG="$operator_image"
grep -q "image: $operator_image" bundle/manifests/subnet-operator.clusterserviceversion.yaml
bundle_local="$host/subnet-operator-bundle:v$VERSION-$KIND_CLUSTER"
make bundle-build BUNDLE_IMG="$bundle_local"
docker push "$bundle_local"
bundle_image="$in_cluster/subnet-operator-bundle:v$VERSION-$KIND_CLUSTER"

step "Installing OLM $OLM_VERSION"
base="https://github.com/operator-framework/operator-lifecycle-manager/releases/download/$OLM_VERSION"
for f in crds.yaml:"$OLM_CRDS_SHA256" olm.yaml:"$OLM_SHA256"; do
	name="${f%%:*}" want="${f#*:}"
	curl -fsSLo "$work/$name" "$base/$name"
	got="$( { sha256sum "$work/$name" 2>/dev/null || shasum -a 256 "$work/$name"; } | cut -d' ' -f1)"
	[ "$want" = "$got" ] || { echo "checksum mismatch for OLM $name: $got"; exit 1; }
done
# The OLM CRDs are too large for the last-applied annotation of a client-side apply.
kubectl create -f "$work/crds.yaml"
kubectl wait --for=condition=Established -f "$work/crds.yaml" --timeout=120s
kubectl create -f "$work/olm.yaml"
# OLM's manifest also subscribes the cluster to the whole OperatorHub.io catalog, which this
# test does not use and which would only pull a large image.
kubectl delete catalogsource -n olm operatorhubio-catalog --ignore-not-found
kubectl rollout status -n olm deployment/olm-operator --timeout=300s
kubectl rollout status -n olm deployment/catalog-operator --timeout=300s
for _ in $(seq 60); do
	[ "$(kubectl get csv -n olm packageserver -o jsonpath='{.status.phase}' 2>/dev/null)" = Succeeded ] && break
	sleep 5
done
[ "$(kubectl get csv -n olm packageserver -o jsonpath='{.status.phase}')" = Succeeded ]

step "Installing the bundle through OLM (AllNamespaces)"
kubectl create namespace "$ns"
"$OPERATOR_SDK" run bundle "$bundle_image" --namespace "$ns" --install-mode AllNamespaces \
	--use-http --index-image "$OPM_IMAGE" --security-context-config restricted --timeout 8m
csv="subnet-operator.v$VERSION"
[ "$(kubectl get csv -n "$ns" "$csv" -o jsonpath='{.status.phase}')" = Succeeded ]
kubectl rollout status -n "$ns" deployment/subnet-operator-controller-manager --timeout=180s

step "The CRDs convert through the webhook OLM configured"
for crd in networkscopes networks subnets sheetexports subnetclaims resourceimports; do
	crd="$crd.network.hypersurgery.dev"
	strategy="$(kubectl get crd "$crd" -o jsonpath='{.spec.conversion.strategy}')"
	svc_ns="$(kubectl get crd "$crd" -o jsonpath='{.spec.conversion.webhook.clientConfig.service.namespace}')"
	ca="$(kubectl get crd "$crd" -o jsonpath='{.spec.conversion.webhook.clientConfig.caBundle}')"
	echo "$crd: $strategy via $svc_ns, CA ${#ca} bytes"
	[ "$strategy" = Webhook ] && [ "$svc_ns" = "$ns" ] && [ -n "$ca" ] ||
		{ echo "$crd does not convert through OLM's webhook"; exit 1; }
done

step "The admission webhooks are OLM's, with its CA"
for kind in validatingwebhookconfiguration mutatingwebhookconfiguration; do
	names="$(kubectl get "$kind" -l olm.owner="$csv" -o name)"
	echo "$kind: $names"
	[ "$(echo "$names" | grep -c .)" -eq 3 ]
	for name in $names; do
		[ -n "$(kubectl get "$name" -o jsonpath='{.webhooks[0].clientConfig.caBundle}')" ]
	done
done

step "A valid scope is admitted and reads back at v1beta1 (admission, then conversion)"
scope() {
	cat <<EOF
apiVersion: network.hypersurgery.dev/$1
kind: NetworkScope
metadata:
  name: $2
spec:
  provider: AWS
  accounts:
    - id: "$4"
  regions: [$3]
EOF
}
# The Service endpoints can lag the rollout by a moment.
for i in $(seq 30); do
	scope v1 olm-check eu-central-1 111111111111 | kubectl apply -f - && break
	[ "$i" -lt 30 ] || exit 1
	sleep 2
done
got="$(kubectl get networkscopes.v1beta1.network.hypersurgery.dev olm-check -o jsonpath='{.apiVersion} {.spec.regions}')"
echo "read at v1beta1: $got"
case "$got" in "network.hypersurgery.dev/v1beta1 "*eu-central-1*) ;; *) exit 1 ;; esac

step "A duplicate region written at v1beta1 is converted and then refused by the webhook"
# The v1beta1 schema lists regions as atomic, so only the webhook (registered for v1, reached
# after conversion) can refuse the duplicate. Another account, so that is the only error.
if out="$(scope v1beta1 olm-duplicate "eu-central-1, eu-central-1" 222222222222 | kubectl apply -f - 2>&1)"; then
	echo "admitted: $out"
	exit 1
fi
echo "$out"
echo "$out" | grep -qF 'spec.regions[1]: Duplicate value: "eu-central-1"'
! kubectl get networkscope olm-duplicate >/dev/null 2>&1

step "The operator left the conversion to OLM"
kubectl logs -n "$ns" deployment/subnet-operator-controller-manager | grep -i conversion || true
if kubectl get events -A --field-selector reason=ConversionConfigured -o name | grep -q .; then
	echo "the operator configured the CRDs' conversion itself"
	exit 1
fi

step "Scorecard"
"$OPERATOR_SDK" scorecard ./bundle --namespace "$ns" --wait-time 5m --output text

step "Removing the operator through OLM"
kubectl delete networkscope olm-check --wait=true --timeout=60s
"$OPERATOR_SDK" cleanup subnet-operator --namespace "$ns" --timeout 3m
echo "OLM install test passed"
