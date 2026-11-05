CONTROLLER_TOOLS_VERSION ?= v0.23.0
ENVTEST_VERSION ?= v0.25.2
ENVTEST_K8S_VERSION ?= 1.37.0
GOVULNCHECK_VERSION ?= v1.9.0

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
LOCALBIN ?= $(CURDIR)/bin

.PHONY: help
help: ## list targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-12s %s\n", $$1, $$2}'

.PHONY: generate
generate: ## deepcopy code
	$(CONTROLLER_GEN) object paths=./...

.PHONY: manifests
manifests: ## CRD and RBAC YAML under config/ (the broker gets its own ClusterRole file)
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=idp-operator paths=./cmd paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	$(CONTROLLER_GEN) rbac:roleName=idp-broker paths=./cmd/broker paths=./internal/broker/... output:rbac:stdout > config/rbac/broker-role.yaml

.PHONY: fmt
fmt: ## fail if gofmt would change anything
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)

.PHONY: vet
vet:
	go vet ./...

.PHONY: envtest
envtest: ## download control plane binaries for envtest
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path >/dev/null

.PHONY: test
test: generate manifests envtest ## run unit and envtest tests
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -count=1

.PHONY: vuln
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: check
check: fmt vet test vuln ## everything CI should run

.PHONY: build
build:
	go build -o $(LOCALBIN)/manager ./cmd
	go build -o $(LOCALBIN)/broker ./cmd/broker
