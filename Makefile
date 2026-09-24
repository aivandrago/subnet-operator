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

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
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
KIND_CLUSTER ?= aws-subnet-operator-test-e2e
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
# chart reference, e.g. hypersurgery/aws-subnet-operator from ChartMuseum.
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
	@docker rmi "example.com/aws-subnet-operator:$(KIND_CLUSTER)" >/dev/null 2>&1 || true

CHART_DIR ?= charts/aws-subnet-operator

.PHONY: helm-crds
helm-crds: manifests ## Copy the generated CRDs into the Helm chart.
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
	- $(CONTAINER_TOOL) buildx create --name aws-subnet-operator-builder
	$(CONTAINER_TOOL) buildx use aws-subnet-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm aws-subnet-operator-builder
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
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

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
