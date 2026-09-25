# Image URL to use all building/pushing image targets
IMG ?= controller:latest
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

# The API reference is generated from the Go types of the stable API, so a field cannot be added
# to v1 without appearing in it: CI regenerates it and fails on a diff.
.PHONY: api-docs
api-docs: crd-ref-docs ## Generate the v1 API reference, docs/reference/api.md.
	@mkdir -p docs/reference
	@tmp=$$(mktemp -d); \
	"$(CRD_REF_DOCS)" --source-path=./api/v1 --config=hack/api-docs/config.yaml --renderer=markdown \
		--output-path="$$tmp/api.md" --log-level=warn && \
	{ head -n 1 "$$tmp/api.md"; echo; cat hack/api-docs/header.md; tail -n +2 "$$tmp/api.md"; } > docs/reference/api.md; \
	rc=$$?; rm -rf "$$tmp"; exit $$rc

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest kustomize ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# The scale test syncs one NetworkScope of 400 account/region targets (the capacity the README
# documents) against a fake EC2 that sleeps like the real one, and a real kube-apiserver. It
# takes about fifteen minutes, so it has a build tag of its own and stays out of `make test`.
# SCALE_* variables change the shape; see internal/controller/scale_test.go.
.PHONY: test-scale
test-scale: manifests generate fmt vet setup-envtest ## Measure a full sync at the documented capacity (~15 minutes).
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags=scale ./internal/controller/ -run TestControllers -count=1 -timeout 60m -v \
		-ginkgo.label-filter=scale

# e2e tests run the operator in a Kind cluster against Moto (an AWS API emulator) started by
# the test suite on the Kind docker network. Kind, kubectl and Moto versions are pinned below.
# kubectl kuberc is disabled by default for test isolation; enable with KUBECTL_KUBERC=true.
KIND_CLUSTER ?= subnet-operator-test-e2e
MOTO_IMAGE ?= motoserver/moto:5.2.3
# Kubernetes version of the Kind node, as a kindest/node image pinned by digest. Empty means
# Kind's own default. CI sets it to the oldest and newest supported versions (docs/policy.md).
KIND_NODE_IMAGE ?=

.PHONY: setup-test-e2e
setup-test-e2e: kind kubectl ## Set up a Kind cluster for e2e tests if it does not exist
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) $(if $(KIND_NODE_IMAGE),--image $(KIND_NODE_IMAGE)) --wait 4m || { \
				echo "The cluster did not come up; the runner is probably loaded. Retrying once."; \
				$(KIND) delete cluster --name $(KIND_CLUSTER) >/dev/null 2>&1 || true; \
				$(KIND) create cluster --name $(KIND_CLUSTER) $(if $(KIND_NODE_IMAGE),--image $(KIND_NODE_IMAGE)) --wait 6m; \
			} ;; \
	esac

# The dashboard's browser code has its own tests, run by Node's built-in runner: no packages.
.PHONY: test-dashboard
test-dashboard: ## Test the dashboard app's live data source (needs Node 18 or later).
	node --test hack/dashboard/*.test.js

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet helm ## Run the e2e tests in Kind against Moto, then tear everything down.
	@status=0; \
	PATH="$(LOCALBIN):$$PATH" KIND="$(KIND)" KIND_CLUSTER="$(KIND_CLUSTER)" MOTO_IMAGE="$(MOTO_IMAGE)" HELM="$(HELM)" \
		go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout 30m || status=$$?; \
	$(MAKE) cleanup-test-e2e; \
	exit $$status

# The upgrade test installs the latest published chart, creates every kind of object, upgrades
# to this checkout (CRDs first, then helm upgrade) and checks nothing was lost or re-reported.
# UPGRADE_FROM picks the previous chart version instead of the latest; UPGRADE_CHART another
# chart reference, e.g. hypersurgery/subnet-operator from ChartMuseum. The test expects 0.9, the
# last release that stored network.hypersurgery.dev/v1beta1: it checks that the objects 0.9 wrote
# are rewritten at v1 and stay readable at both versions.
UPGRADE_FROM ?=
UPGRADE_CHART ?=

.PHONY: test-upgrade
test-upgrade: setup-test-e2e manifests generate fmt vet helm-crds helm ## Run the upgrade test from the latest release in Kind, then tear everything down.
	@status=0; \
	PATH="$(LOCALBIN):$$PATH" KIND="$(KIND)" KIND_CLUSTER="$(KIND_CLUSTER)" MOTO_IMAGE="$(MOTO_IMAGE)" HELM="$(HELM)" \
		E2E_UPGRADE=true UPGRADE_FROM="$(UPGRADE_FROM)" UPGRADE_CHART="$(UPGRADE_CHART)" \
		go test -tags=e2e ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=upgrade -timeout 30m || status=$$?; \
	$(MAKE) cleanup-test-e2e; \
	exit $$status

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Tear down the Kind cluster and Moto container used for e2e tests
	@docker rm -f "$(KIND_CLUSTER)-moto" >/dev/null 2>&1 || true
	@$(KIND) delete cluster --name $(KIND_CLUSTER) || true
	@docker rm -f "$(KIND_CLUSTER)-control-plane" >/dev/null 2>&1 || true
	@# The suite tags the manager image per cluster; untagged, these pile up on a shared runner.
	@docker rmi "example.com/subnet-operator:$(KIND_CLUSTER)" >/dev/null 2>&1 || true

CHART_DIR ?= charts/subnet-operator

.PHONY: helm-crds
helm-crds: manifests ## Copy the generated CRDs into the Helm chart.
	# The chart ships exactly the generated CRDs: one that is no longer generated goes too.
	rm -f "$(CHART_DIR)"/crds/*.yaml
	cp config/crd/bases/*.yaml "$(CHART_DIR)/crds/"

.PHONY: helm-lint
helm-lint: helm helm-crds ## Lint and render the Helm chart.
	"$(HELM)" lint "$(CHART_DIR)"
	"$(HELM)" template release "$(CHART_DIR)" >/dev/null
	"$(HELM)" template release "$(CHART_DIR)" -f examples/values-organization.yaml >/dev/null
	# The webhooks ship either way: with cert-manager issuing the certificate, with one the
	# chart signs itself, and not at all.
	"$(HELM)" template release "$(CHART_DIR)" --set webhook.certificate.certManager=true >/dev/null
	"$(HELM)" template release "$(CHART_DIR)" --set webhook.enabled=false >/dev/null
	# The dashboard, alone and with everything around it.
	"$(HELM)" template release "$(CHART_DIR)" --set dashboard.enabled=true >/dev/null
	"$(HELM)" template release "$(CHART_DIR)" --set dashboard.enabled=true --set networkPolicy.enabled=true \
		--set dashboard.ingress.enabled=true --set 'dashboard.ingress.hosts[0].host=subnets.example.com' \
		--set dashboard.tls.secretName=dashboard-tls >/dev/null

# Artifact Hub reads the chart's artifacthub.io/* annotations; its own linter checks them
# the way Artifact Hub will when it indexes a release.
.PHONY: artifacthub-lint
artifacthub-lint: ah ## Check the chart's Artifact Hub metadata with Artifact Hub's linter.
	"$(AH)" lint --kind helm --path charts

CHARTMUSEUM_URL ?= https://charts.hypersurgery.dev

.PHONY: helm-publish
helm-publish: helm helm-lint ## Package the chart and push it to ChartMuseum (needs CHARTMUSEUM_USER/PASSWORD).
	@[ -n "$(CHARTMUSEUM_USER)" ] && [ -n "$(CHARTMUSEUM_PASSWORD)" ] || { \
		echo "Set CHARTMUSEUM_USER and CHARTMUSEUM_PASSWORD"; exit 1; }
	@set -e; \
	out=$$(mktemp -d); \
	"$(HELM)" package "$(CHART_DIR)" -d "$$out" >/dev/null; \
	pkg=$$(ls "$$out"/*.tgz); \
	echo "Uploading $$(basename $$pkg) to $(CHARTMUSEUM_URL)"; \
	code=$$(curl -sS -u "$(CHARTMUSEUM_USER):$(CHARTMUSEUM_PASSWORD)" -o "$$out/resp" -w "%{http_code}" \
		--data-binary "@$$pkg" "$(CHARTMUSEUM_URL)/api/charts"); \
	cat "$$out/resp"; echo; \
	rm -rf "$$out"; \
	[ "$$code" = "201" ] || { echo "upload failed with HTTP $$code"; exit 1; }

.PHONY: lint
test-alerts: helm promtool ## Test the chart's alerts: which fire and which stay quiet, with promtool.
	HELM="$(HELM)" PROMTOOL="$(PROMTOOL)" hack/alerts/test.sh

lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager and dashboard binaries.
	go build -o bin/manager ./cmd
	go build -o bin/dashboard ./cmd/dashboard

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name subnet-operator-builder
	$(CONTAINER_TOOL) buildx use subnet-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm subnet-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize kubectl ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize kubectl ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize kubectl ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize kubectl ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ OLM bundle

# The OLM bundle for OperatorHub (docs/olm.md). The bundle is generated, not committed: its
# source is config/manifests (the ClusterServiceVersion base, whose CRD descriptors come from the
# +operator-sdk markers in api/) plus config/default and config/webhook. The release workflow
# generates it with the operator image's digest and publishes it as a signed bundle image.

# VERSION is the operator version the bundle describes, without a leading v. It defaults to the
# chart's appVersion, which is the version of the latest release.
VERSION ?= $(shell sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}$$/\1/p' $(CHART_DIR)/Chart.yaml)
BUNDLE_PACKAGE ?= subnet-operator
IMAGE_TAG_BASE ?= ghcr.io/aivandrago/subnet-operator
# The operator image the bundle deploys. A release passes it by digest (image@sha256:...), and
# the digest then pins both the Deployment and relatedImages.
BUNDLE_OPERATOR_IMG ?= $(IMAGE_TAG_BASE):$(VERSION)
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)
# With a digest, operator-sdk also lists the image in relatedImages, which is what mirroring
# tools for disconnected clusters read. It needs no registry access for an image given by digest.
BUNDLE_DIGEST_FLAG = $(if $(findstring @sha256:,$(BUNDLE_OPERATOR_IMG)),--use-image-digests)
# alpha until 1.0; the release workflow publishes 1.0 and later to stable (docs/olm.md).
CHANNELS ?= alpha
DEFAULT_CHANNEL ?= $(firstword $(subst $(comma), ,$(CHANNELS)))
comma := ,
# The upgrade graph: every bundle replaces the previous published one (BUNDLE_REPLACES, a
# version; empty for the first bundle) and skips everything older than itself, so OLM upgrades
# any earlier version straight to this one.
BUNDLE_REPLACES ?=
BUNDLE_SKIP_RANGE ?= <$(VERSION)
# The commit time, so the same commit always generates the same bundle.
BUNDLE_CREATED_AT ?= $(shell TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ 2>/dev/null)

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate the OLM bundle in bundle/ (VERSION, BUNDLE_OPERATOR_IMG, CHANNELS, BUNDLE_REPLACES).
	@[ -n "$(VERSION)" ] || { echo "Set VERSION"; exit 1; }
	"$(OPERATOR_SDK)" generate kustomize manifests -q --package $(BUNDLE_PACKAGE) --apis-dir api
	@# Everything that depends on the release is set in a copy of config/, so generating a
	@# bundle never changes a committed file.
	@set -e; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	cp -R config "$$tmp/"; \
	( cd "$$tmp/config/manager" && "$(KUSTOMIZE)" edit set image controller=$(BUNDLE_OPERATOR_IMG) ); \
	case "$(DEFAULT_CHANNEL)" in stable) maturity=stable ;; *) maturity=alpha ;; esac; \
	patch='[{"op":"add","path":"/metadata/annotations/containerImage","value":"$(BUNDLE_OPERATOR_IMG)"},'; \
	patch="$$patch"'{"op":"add","path":"/metadata/annotations/olm.skipRange","value":"$(BUNDLE_SKIP_RANGE)"},'; \
	patch="$$patch"'{"op":"replace","path":"/spec/maturity","value":"'"$$maturity"'"}'; \
	if [ -n "$(BUNDLE_REPLACES)" ]; then \
		patch="$$patch"',{"op":"add","path":"/spec/replaces","value":"$(BUNDLE_PACKAGE).v$(BUNDLE_REPLACES)"}'; \
	fi; \
	( cd "$$tmp/config/manifests" && "$(KUSTOMIZE)" edit add patch --kind ClusterServiceVersion --patch "$$patch]" ); \
	rm -rf bundle; \
	"$(KUSTOMIZE)" build "$$tmp/config/manifests" | "$(OPERATOR_SDK)" generate bundle -q --overwrite \
		$(BUNDLE_DIGEST_FLAG) --version $(VERSION) --package $(BUNDLE_PACKAGE) \
		--channels $(CHANNELS) --default-channel $(DEFAULT_CHANNEL) \
		> "$$tmp/generate.log" 2>&1 || { cat "$$tmp/generate.log"; exit 1; }; \
	grep -v '^[0-9/]* [0-9:]* ' "$$tmp/generate.log" || true
	@# operator-sdk stamps the time it ran; the commit time keeps a release's bundle reproducible.
	sed -i.bak 's/^    createdAt: .*/    createdAt: "$(BUNDLE_CREATED_AT)"/' bundle/manifests/$(BUNDLE_PACKAGE).clusterserviceversion.yaml
	rm -f bundle/manifests/$(BUNDLE_PACKAGE).clusterserviceversion.yaml.bak
	@# The webhook Service only told operator-sdk which Deployment serves the webhooks. OLM
	@# creates a Service of its own for them, so this one would only be a second, unused one.
	rm -f bundle/manifests/webhook-service_v1_service.yaml

# The operatorframework suite is what OperatorHub's own pipeline runs: the OperatorHub metadata
# (operatorhubv2, capabilities, categories), good practices and deprecated APIs.
.PHONY: bundle-validate
bundle-validate: bundle ## Generate the bundle and validate it with the OperatorHub and good-practices suites.
	"$(OPERATOR_SDK)" bundle validate ./bundle --select-optional suite=operatorframework

.PHONY: bundle-build
bundle-build: ## Build the bundle image (run make bundle first).
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(CONTAINER_TOOL) push $(BUNDLE_IMG)

# A file-based catalog holding the bundles in BUNDLE_IMGS (pushed bundle images), for installing
# through a CatalogSource the way OperatorHub's catalog does.
CATALOG_IMG ?= $(IMAGE_TAG_BASE)-catalog:v$(VERSION)
BUNDLE_IMGS ?= $(BUNDLE_IMG)

.PHONY: catalog
catalog: opm ## Render a file-based catalog of BUNDLE_IMGS into catalog/ (the bundle images must be pushed).
	rm -rf catalog && mkdir -p catalog/$(BUNDLE_PACKAGE)
	"$(OPM)" init $(BUNDLE_PACKAGE) --default-channel=$(DEFAULT_CHANNEL) --icon=site/dashboard/icon.svg \
		--output yaml > catalog/$(BUNDLE_PACKAGE)/index.yaml
	"$(OPM)" render $(BUNDLE_IMGS) --output yaml >> catalog/$(BUNDLE_PACKAGE)/index.yaml
	@set -e; for img in $(BUNDLE_IMGS); do \
		name=$$("$(OPM)" render $$img --output json | sed -n 's/^    "name": "\($(BUNDLE_PACKAGE)\.v[^"]*\)".*/\1/p' | head -1); \
		echo "---"; echo "schema: olm.channel"; echo "package: $(BUNDLE_PACKAGE)"; \
		echo "name: $(DEFAULT_CHANNEL)"; echo "entries:"; echo "- name: $$name"; \
	done >> catalog/$(BUNDLE_PACKAGE)/index.yaml
	"$(OPM)" validate catalog

.PHONY: catalog-build
catalog-build: opm ## Build the catalog image from catalog/.
	"$(OPM)" generate dockerfile catalog
	$(CONTAINER_TOOL) build -f catalog.Dockerfile -t $(CATALOG_IMG) .

.PHONY: catalog-push
catalog-push: ## Push the catalog image.
	$(CONTAINER_TOOL) push $(CATALOG_IMG)

# test-olm installs OLM and then the bundle into a Kind cluster, through a registry container on
# the Kind network, checks that the webhooks work with the certificates OLM issues, and runs
# scorecard (hack/olm/test.sh). OLM's release manifests carry no checksums, so theirs are pinned
# here; the images they name are pinned by digest upstream.
OLM_VERSION ?= v0.46.0
OLM_CRDS_SHA256 ?= 2ecd51a33dfa00a5abcae7bf47bcc4eea6c6d6b4212bb36d6b887aa6ef6d63c5
OLM_SHA256 ?= 3a6cd5caea67bedc11464ad443fa0826d3777d7f1b4f2dfa00766a739f2ab44c
OLM_REGISTRY_IMAGE ?= registry:3.1.2@sha256:c87f33837722a100572e95d7dc4bf539fc42cf68202b13c3bc03c0ff54c3a649
# The image of the registry pod operator-sdk run bundle starts; the same version as OPM_VERSION.
OLM_OPM_IMAGE ?= quay.io/operator-framework/opm:v1.74.0@sha256:b32d3891616662620da08d7f0ec42c2e69fa2de43427dc975d35b12f7a969a0f

.PHONY: test-olm
test-olm: setup-test-e2e kustomize operator-sdk kubectl ## Install the bundle through OLM in Kind, check the webhooks, run scorecard, then tear everything down.
	@status=0; \
	KIND="$(KIND)" KUBECTL="$(KUBECTL)" OPERATOR_SDK="$(OPERATOR_SDK)" KIND_CLUSTER="$(KIND_CLUSTER)" \
		OLM_VERSION="$(OLM_VERSION)" OLM_CRDS_SHA256="$(OLM_CRDS_SHA256)" OLM_SHA256="$(OLM_SHA256)" \
		REGISTRY_IMAGE="$(OLM_REGISTRY_IMAGE)" OPM_IMAGE="$(OLM_OPM_IMAGE)" VERSION="$(VERSION)" \
		hack/olm/test.sh || status=$$?; \
	$(MAKE) cleanup-test-e2e; \
	exit $$status

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= $(LOCALBIN)/kubectl
HELM ?= $(LOCALBIN)/helm
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0
CRD_REF_DOCS_VERSION ?= v0.3.0
CRD_REF_DOCS_SUM ?= h1:9bGSUkBR56Z7TuDGQAu3KGbBkagwwZ6RkZmS+qvDuDM=

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
# The BSD and GNU checksum tools disagree about reading a checksum list, so compare digests.
define sha256of
$$( { sha256sum "$(1)" 2>/dev/null || shasum -a 256 "$(1)"; } | cut -d' ' -f1 )
endef
KIND_VERSION ?= v0.33.0
HELM_VERSION ?= v4.3.0
KUBECTL_VERSION ?= v1.37.0

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	$(call go-install-tool,$(KIND),sigs.k8s.io/kind,$(KIND_VERSION))

.PHONY: helm
PROMTOOL ?= $(LOCALBIN)/promtool
PROMTOOL_VERSION ?= 3.14.0

.PHONY: promtool
promtool: $(PROMTOOL) ## Download promtool locally if necessary (checksum verified).
$(PROMTOOL): $(LOCALBIN)
	@[ -f "$(PROMTOOL)-$(PROMTOOL_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		name=prometheus-$(PROMTOOL_VERSION).$$os-$$arch; \
		base=https://github.com/prometheus/prometheus/releases/download/v$(PROMTOOL_VERSION); \
		echo "Downloading $$base/$$name.tar.gz"; \
		tmp=$$(mktemp -d); \
		curl -fsSLo "$$tmp/$$name.tar.gz" "$$base/$$name.tar.gz"; \
		want=$$(curl -fsSL "$$base/sha256sums.txt" | grep " $$name.tar.gz$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name.tar.gz); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name.tar.gz"; exit 1; }; \
		tar -xzf "$$tmp/$$name.tar.gz" -C "$$tmp"; \
		mv "$$tmp/$$name/promtool" "$(PROMTOOL)-$(PROMTOOL_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(PROMTOOL)-$(PROMTOOL_VERSION)" "$(PROMTOOL)"

AH ?= $(LOCALBIN)/ah
AH_VERSION ?= 1.23.0

.PHONY: ah
ah: $(AH) ## Download the Artifact Hub CLI locally if necessary (checksum verified).
$(AH): $(LOCALBIN)
	@[ -f "$(AH)-$(AH_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS | sed 's/^darwin$$/macos/'); arch=$$(go env GOARCH); \
		name=ah_$(AH_VERSION)_$${os}_$$arch.tar.gz; \
		base=https://github.com/artifacthub/hub/releases/download/v$(AH_VERSION); \
		echo "Downloading $$base/$$name"; \
		tmp=$$(mktemp -d); \
		curl -fsSLo "$$tmp/$$name" "$$base/$$name"; \
		want=$$(curl -fsSL "$$base/ah_$(AH_VERSION)_checksums.txt" | grep " $$name$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name"; exit 1; }; \
		tar -xzf "$$tmp/$$name" -C "$$tmp" ah; \
		mv "$$tmp/ah" "$(AH)-$(AH_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(AH)-$(AH_VERSION)" "$(AH)"

# The OLM bundle tools. Each release publishes a checksums.txt that lists every binary.
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
OPERATOR_SDK_VERSION ?= v1.42.3

.PHONY: operator-sdk
operator-sdk: $(OPERATOR_SDK) ## Download operator-sdk locally if necessary (checksum verified).
$(OPERATOR_SDK): $(LOCALBIN)
	@[ -f "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)" ] || { \
		set -e; \
		name=operator-sdk_$$(go env GOOS)_$$(go env GOARCH); \
		base=https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION); \
		echo "Downloading $$base/$$name"; \
		tmp=$$(mktemp -d); \
		curl -fsSLo "$$tmp/$$name" "$$base/$$name"; \
		want=$$(curl -fsSL "$$base/checksums.txt" | grep " $$name$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name"; exit 1; }; \
		chmod +x "$$tmp/$$name"; \
		mv "$$tmp/$$name" "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(OPERATOR_SDK)-$(OPERATOR_SDK_VERSION)" "$(OPERATOR_SDK)"

OPM ?= $(LOCALBIN)/opm
OPM_VERSION ?= v1.74.0

.PHONY: opm
opm: $(OPM) ## Download opm locally if necessary (checksum verified).
$(OPM): $(LOCALBIN)
	@[ -f "$(OPM)-$(OPM_VERSION)" ] || { \
		set -e; \
		name=$$(go env GOOS)-$$(go env GOARCH)-opm; \
		base=https://github.com/operator-framework/operator-registry/releases/download/$(OPM_VERSION); \
		echo "Downloading $$base/$$name"; \
		tmp=$$(mktemp -d); \
		curl -fsSLo "$$tmp/$$name" "$$base/$$name"; \
		want=$$(curl -fsSL "$$base/checksums.txt" | grep " $$name$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name"; exit 1; }; \
		chmod +x "$$tmp/$$name"; \
		mv "$$tmp/$$name" "$(OPM)-$(OPM_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(OPM)-$(OPM_VERSION)" "$(OPM)"

ORAS ?= $(LOCALBIN)/oras
ORAS_VERSION ?= 1.3.4

.PHONY: oras
oras: $(ORAS) ## Download oras locally if necessary (checksum verified).
$(ORAS): $(LOCALBIN)
	@[ -f "$(ORAS)-$(ORAS_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		name=oras_$(ORAS_VERSION)_$${os}_$$arch.tar.gz; \
		base=https://github.com/oras-project/oras/releases/download/v$(ORAS_VERSION); \
		echo "Downloading $$base/$$name"; \
		tmp=$$(mktemp -d); \
		curl -fsSLo "$$tmp/$$name" "$$base/$$name"; \
		want=$$(curl -fsSL "$$base/oras_$(ORAS_VERSION)_checksums.txt" | grep " $$name$$" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$name); \
		[ -n "$$want" ] && [ "$$want" = "$$got" ] || { echo "checksum mismatch for $$name"; exit 1; }; \
		tar -xzf "$$tmp/$$name" -C "$$tmp" oras; \
		mv "$$tmp/oras" "$(ORAS)-$(ORAS_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(ORAS)-$(ORAS_VERSION)" "$(ORAS)"

helm: $(HELM) ## Download helm locally if necessary (checksum verified).
$(HELM): $(LOCALBIN)
	@[ -f "$(HELM)-$(HELM_VERSION)" ] || { \
		set -e; \
		os=$$(go env GOOS); arch=$$(go env GOARCH); \
		tgz=helm-$(HELM_VERSION)-$$os-$$arch.tar.gz; \
		echo "Downloading https://get.helm.sh/$$tgz"; \
		tmp=$$(mktemp -d); \
		curl -fsSLo "$$tmp/$$tgz" "https://get.helm.sh/$$tgz"; \
		want=$$(curl -fsSL "https://get.helm.sh/$$tgz.sha256sum" | cut -d' ' -f1); \
		got=$(call sha256of,$$tmp/$$tgz); \
		[ "$$want" = "$$got" ] || { echo "checksum mismatch for $$tgz"; exit 1; }; \
		tar -xzf "$$tmp/$$tgz" -C "$$tmp"; \
		mv "$$tmp/$$os-$$arch/helm" "$(HELM)-$(HELM_VERSION)"; \
		rm -rf "$$tmp"; \
	}
	@ln -sf "$(HELM)-$(HELM_VERSION)" "$(HELM)"

.PHONY: kubectl
kubectl: $(KUBECTL) ## Download kubectl locally if necessary (checksum verified).
$(KUBECTL): $(LOCALBIN)
	@[ -f "$(KUBECTL)-$(KUBECTL_VERSION)" ] || { \
		set -e; \
		url=https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/$$(go env GOOS)/$$(go env GOARCH)/kubectl; \
		echo "Downloading $$url"; \
		curl -fsSLo "$(KUBECTL)-$(KUBECTL_VERSION)" "$$url"; \
		want=$$(curl -fsSL "$$url.sha256"); \
		got=$(call sha256of,$(KUBECTL)-$(KUBECTL_VERSION)); \
		[ "$$want" = "$$got" ] || { echo "checksum mismatch for kubectl"; exit 1; }; \
		chmod +x "$(KUBECTL)-$(KUBECTL_VERSION)"; \
	}
	@ln -sf "$(KUBECTL)-$(KUBECTL_VERSION)" "$(KUBECTL)"

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

# crd-ref-docs is installed with go install like the tools above, which checks the module
# against the Go checksum database; the module hash is also pinned here, so a re-tagged release
# cannot slip in.
.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary (checksum verified).
$(CRD_REF_DOCS): $(LOCALBIN)
	@sum=$$(GOFLAGS=-mod=mod go mod download -json github.com/elastic/crd-ref-docs@$(CRD_REF_DOCS_VERSION) | sed -n 's/^[[:space:]]*"Sum": "\(.*\)",$$/\1/p'); \
	[ "$$sum" = "$(CRD_REF_DOCS_SUM)" ] || { echo "crd-ref-docs $(CRD_REF_DOCS_VERSION): module hash $$sum, want $(CRD_REF_DOCS_SUM)" >&2; exit 1; }
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
