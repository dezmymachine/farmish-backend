# Farmish backend. Run `make help` for targets.

GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0
BIN_DIR   := $(CURDIR)/bin
GOLANGCI  := $(BIN_DIR)/golangci-lint
GOVULN    := $(BIN_DIR)/govulncheck
IMAGE     ?= farmish-backend:dev
# `make run` loads .env if present, else the committed example defaults.
ENV_FILE  ?= $(if $(wildcard .env),.env,.env.example)

.DEFAULT_GOAL := help
.PHONY: help run build test lint fmt fmt-check tidy tidy-check vuln docker-build smoke ci tools

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n",$$1,$$2}'

run: ## Run the API locally (loads $(ENV_FILE))
	@set -a && . ./$(ENV_FILE) && set +a && go run ./cmd/api

build: ## Build a static binary to bin/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/api ./cmd/api

test: ## Run tests with the race detector
	go test -race -count=1 ./...

lint: $(GOLANGCI) ## Run go vet and golangci-lint
	go vet ./...
	$(GOLANGCI) run ./...

fmt: $(GOLANGCI) ## Format code (gofumpt + goimports via golangci-lint)
	$(GOLANGCI) fmt ./...

fmt-check: $(GOLANGCI) ## Fail if any file needs formatting
	$(GOLANGCI) fmt --diff ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

tidy-check: ## Fail if go.mod/go.sum are not tidy
	go mod tidy -diff

vuln: $(GOVULN) ## Scan dependencies and stdlib for known vulnerabilities
	$(GOVULN) ./...

docker-build: ## Build the Docker image ($(IMAGE))
	docker build -t $(IMAGE) .

smoke: docker-build ## Run the image and check /healthz, then stop it
	./scripts/smoke.sh $(IMAGE)

# Local stand-in for CI while GitHub Actions is off (ADR-0004). Run before every push.
ci: tidy-check fmt-check lint test vuln smoke ## Run every pre-push check
	@echo "ci: all checks passed"

tools: $(GOLANGCI) $(GOVULN) ## Install pinned dev tools into bin/

$(GOLANGCI):
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOVULN):
	GOBIN=$(BIN_DIR) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
