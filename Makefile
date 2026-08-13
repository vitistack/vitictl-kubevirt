BINARY := viti-kubevirt
# viti dispatches plugins by binary name, so the "kv" shorthand is a second
# name for the same binary rather than a cobra alias.
ALIAS  := viti-kv
PKG    := github.com/vitistack/vitictl-kubevirt

ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS ?= -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: fix
fix: golangci-lint ## Auto-fix lint issues.
	$(GOLANGCI_LINT) run --fix

.PHONY: gosec
gosec: gosec-bin ## Run security analysis.
	$(GOSEC) ./...

.PHONY: govulncheck
govulncheck: govulncheck-bin ## Check dependencies for known vulnerabilities.
	$(GOVULNCHECK) ./...

.PHONY: update-deps
update-deps: ## Update dependencies to latest versions.
	go get -u ./...
	go mod tidy

##@ Build

.PHONY: build
build: fmt vet ## Build the plugin binary into bin/.
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

.PHONY: install
install: build ## Install the plugin (and its kv alias) onto GOBIN.
	install -m 0755 bin/$(BINARY) $(GOBIN)/$(BINARY)
	ln -sf $(GOBIN)/$(BINARY) $(GOBIN)/$(ALIAS)
	@echo "installed $(GOBIN)/$(BINARY) (+ $(ALIAS) symlink) — run 'viti kubevirt --help' or 'viti kv --help'"

.PHONY: uninstall
uninstall: ## Remove the installed binary and its alias.
	rm -f $(GOBIN)/$(BINARY) $(GOBIN)/$(ALIAS)

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin cover.out

##@ Tools

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT     ?= $(LOCALBIN)/golangci-lint
GOSEC             ?= $(LOCALBIN)/gosec
GOVULNCHECK       ?= $(LOCALBIN)/govulncheck
GOLANGCI_LINT_VER ?= v2.6.2
GOSEC_VER         ?= v2.22.9
GOVULNCHECK_VER   ?= latest

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN)
	@test -x $(GOLANGCI_LINT) || \
		GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VER)

.PHONY: gosec-bin
gosec-bin: $(LOCALBIN)
	@test -x $(GOSEC) || GOBIN=$(LOCALBIN) go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VER)

.PHONY: govulncheck-bin
govulncheck-bin: $(LOCALBIN)
	@test -x $(GOVULNCHECK) || GOBIN=$(LOCALBIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER)
