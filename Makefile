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
	"$(CONTROLLER_GEN)" object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: test-database-integration
test-database-integration: setup-envtest ## 使用临时 API server、PostgreSQL 与 OpenBao 容器验证 Database 后端；API server、PostgreSQL 与 OpenBao 在 go test 外启动一次。
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" \
		INTEGRATION_SERVICES="apiserver postgres openbao" hack/integration-test.sh ./internal/database/... ./internal/infra/... ./internal/bootstrap/...

.PHONY: lint-database-integration
lint-database-integration: golangci-lint ## 检查集成测试构建标签下的 Database 代码。
	"$(GOLANGCI_LINT)" run --build-tags=integration \
		./internal/database/... ./internal/infra/... ./internal/bootstrap/... ./internal/testenv/...

.PHONY: test-loadbalancer-integration
test-loadbalancer-integration: setup-envtest ## 使用临时 API server 与 HAProxy + Data Plane API、PowerDNS 容器验证 LoadBalancer；后两者在 go test 外启动一次。
	KUBEBUILDER_ASSETS="$(ENVTEST_ASSETS)" \
		INTEGRATION_SERVICES="haproxy pdns" hack/integration-test.sh ./internal/loadbalancer/...
	$(MAKE) test-lb-agent-netlink

# 与 manager 镜像同一 base；测试二进制是静态链接的，root 只在容器自己的网络 namespace 内。
LB_AGENT_NETLINK_IMAGE ?= gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3

.PHONY: test-lb-agent-netlink
test-lb-agent-netlink: ## 在一次性容器的网络 namespace 中以 CAP_NET_ADMIN 验证 LB agent 管理 lo 上的 VIP。
	CGO_ENABLED=0 go test -c -tags=integration -o "$(LOCALBIN)/lb-agent.test" ./internal/loadbalancer/agent
	docker run --rm --user 0 --cap-add NET_ADMIN -e AYATORI_NETLINK_TEST=1 \
		-v "$(LOCALBIN)/lb-agent.test:/lb-agent.test:ro" --entrypoint /lb-agent.test \
		$(LB_AGENT_NETLINK_IMAGE) -test.run '^TestLoopback$$' -test.v

.PHONY: lint-loadbalancer-integration
lint-loadbalancer-integration: golangci-lint ## 检查集成测试构建标签下的 LoadBalancer 代码。
	"$(GOLANGCI_LINT)" run --build-tags=integration ./internal/loadbalancer/... ./internal/testenv/...

.PHONY: test-compute-integration
test-compute-integration: setup-envtest ## 使用临时 API server 与 libvirt 进程内 test 驱动验证 Compute Agent（需要 libvirt-dev）。
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" \
		go test -tags=integration -race -count=1 ./internal/compute/...

.PHONY: lint-compute-integration
lint-compute-integration: golangci-lint ## 检查集成测试构建标签下的 Compute 代码。
	"$(GOLANGCI_LINT)" run --build-tags=integration ./internal/compute/...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.27.1
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
	- $(CONTAINER_TOOL) buildx create --name ayatori-builder
	$(CONTAINER_TOOL) buildx use ayatori-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm ayatori-builder
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
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
OAPI_CODEGEN ?= $(LOCALBIN)/oapi-codegen

## Tool Versions
# renovate: datasource=go depName=sigs.k8s.io/kustomize/kustomize/v5
KUSTOMIZE_VERSION ?= v5.8.3
# renovate: datasource=go depName=sigs.k8s.io/controller-tools
CONTROLLER_TOOLS_VERSION ?= v0.23.0

# GITHUB_RELEASES 是 GitHub release 的下载前缀；CI 指向 Artifact Keeper 的 github-releases 缓存，不直连外网。
GITHUB_RELEASES ?= https://github.com

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

#ENVTEST_K8S_VERSION 是 envtest 二进制的完整版本（例如 1.37.0），取 go.mod 的 k8s.io/api 的次版本：
# controller-tools 只保证发布每个次版本的 .0，k8s.io/api 的补丁版本不一定有对应的 envtest。
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?0\.([0-9]+)\..*/1.\1.0/')
ENVTEST_ASSETS = $(LOCALBIN)/k8s/$(ENVTEST_K8S_VERSION)-$(shell go env GOOS)-$(shell go env GOARCH)

# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.15.0
# renovate: datasource=go depName=github.com/oapi-codegen/oapi-codegen/v2
OAPI_CODEGEN_VERSION ?= v2.9.0
# 与 LB 主机部署的 dataplaneapi 版本一致；升级时同时更新校验和并重新生成。
DATAPLANEAPI_VERSION ?= v3.4.3
DATAPLANEAPI_SPEC_SHA256 ?= 035a0dc6f44251fdf5c5b5a32bd62827c027c238a774888b899f1b6bce20a566
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: $(ENVTEST_ASSETS)/kube-apiserver ## Download the binaries required for ENVTEST in the local bin directory.
$(ENVTEST_ASSETS)/kube-apiserver: | $(LOCALBIN)
	@set -e; asset=envtest-v$(ENVTEST_K8S_VERSION)-$$(go env GOOS)-$$(go env GOARCH).tar.gz; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	echo "Downloading $$asset"; \
	curl -fsSL --retry 3 -o "$$tmp/$$asset" "$(GITHUB_RELEASES)/kubernetes-sigs/controller-tools/releases/download/envtest-v$(ENVTEST_K8S_VERSION)/$$asset"; \
	mkdir -p "$(ENVTEST_ASSETS)"; \
	tar -xzf "$$tmp/$$asset" -C "$(ENVTEST_ASSETS)" --strip-components=2

.PHONY: oapi-codegen
oapi-codegen: $(OAPI_CODEGEN) ## Download oapi-codegen locally if necessary.
$(OAPI_CODEGEN): $(LOCALBIN)
	$(call go-install-tool,$(OAPI_CODEGEN),github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen,$(OAPI_CODEGEN_VERSION))

.PHONY: generate-dataplaneapi
generate-dataplaneapi: oapi-codegen ## 从固定版本的 Data Plane API spec 生成 HAProxy adapter 客户端；去掉 x-go-type 以免依赖 client-native。
	@spec="$(LOCALBIN)/dataplane_spec-$(DATAPLANEAPI_VERSION).yaml"; \
	curl -fsSL -o "$$spec" https://raw.githubusercontent.com/haproxytech/dataplaneapi/$(DATAPLANEAPI_VERSION)/specification/build/dataplane_spec.yaml && \
	echo "$(DATAPLANEAPI_SPEC_SHA256)  $$spec" | sha256sum -c - && \
	sed -E '/^\s*x-go-type: /d; /^\s*x-go-type-import:/{N;d}' "$$spec" > "$$spec.local" && \
	"$(OAPI_CODEGEN)" -config internal/loadbalancer/adapter/haproxy/dataplaneapi/oapi-codegen.yaml "$$spec.local"

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
# 使用上游发布的二进制并按发布的 checksums 校验，不从源码编译；结构化日志检查用内置 loggercheck。
$(GOLANGCI_LINT): | $(LOCALBIN)
	@set -e; v=$(GOLANGCI_LINT_VERSION); v=$${v#v}; \
	asset=golangci-lint-$$v-$$(go env GOOS)-$$(go env GOARCH).tar.gz; \
	base=$(GITHUB_RELEASES)/golangci/golangci-lint/releases/download/v$$v; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	echo "Downloading golangci-lint v$$v"; \
	curl -fsSL --retry 3 -o "$$tmp/$$asset" "$$base/$$asset"; \
	curl -fsSL --retry 3 "$$base/golangci-lint-$$v-checksums.txt" | grep " $$asset$$" | (cd "$$tmp" && sha256sum -c -) >/dev/null; \
	tar -xzf "$$tmp/$$asset" -C "$$tmp" --strip-components=1; \
	mv "$$tmp/golangci-lint" "$(GOLANGCI_LINT)"

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
