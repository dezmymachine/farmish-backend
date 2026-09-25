# Farmish backend. Run `make help` for targets.

GOLANGCI_LINT_VERSION := v2.14.0
BIN_DIR   := $(CURDIR)/bin
GOLANGCI  := $(BIN_DIR)/golangci-lint
IMAGE     ?= farmish-backend:dev
# `make run` loads .env if present, else the committed example defaults.
ENV_FILE  ?= $(if $(wildcard .env),.env,.env.example)

.DEFAULT_GOAL := help
.PHONY: help run build test lint fmt tidy docker-build tools

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

tidy: ## Tidy go.mod/go.sum
	go mod tidy

docker-build: ## Build the Docker image ($(IMAGE))
	docker build -t $(IMAGE) .

tools: $(GOLANGCI) ## Install pinned dev tools into bin/

$(GOLANGCI):
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
