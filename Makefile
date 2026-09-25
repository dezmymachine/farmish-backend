# Farmish backend. Run `make help` for targets.

GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0
SQLC_VERSION          := 1.31.1
BIN_DIR   := $(CURDIR)/bin
GOLANGCI  := $(BIN_DIR)/golangci-lint
GOVULN    := $(BIN_DIR)/govulncheck
IMAGE     ?= farmish-backend:dev
# `make run` loads .env if present, else the committed example defaults.
ENV_FILE  ?= $(if $(wildcard .env),.env,.env.example)
LOAD_ENV  := set -a && . ./$(ENV_FILE) && set +a
# Admin connection to the compose Postgres; dbtest creates a throwaway DB per test.
TEST_DATABASE_URL ?= postgres://farmish:farmish@127.0.0.1:$(or $(FARMISH_PG_PORT),54320)/farmish?sslmode=disable
SQLC      := docker run --rm -u $$(id -u):$$(id -g) -v $(CURDIR):/src -w /src sqlc/sqlc:$(SQLC_VERSION)

.DEFAULT_GOAL := help
.PHONY: help run build test lint fmt fmt-check tidy tidy-check vuln docker-build smoke ci tools \
        db-up db-down db-reset migrate-up migrate-down migrate-version migrate-new sqlc sqlc-check

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n",$$1,$$2}'

run: ## Run the API locally (loads $(ENV_FILE))
	@$(LOAD_ENV) && go run ./cmd/api

build: ## Build a static binary to bin/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/api ./cmd/api

test: db-up ## Run all tests (incl. DB tests) with the race detector
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' DBTEST_REQUIRED=1 go test -race -count=1 ./...

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

smoke: docker-build db-up ## Run the image against compose Postgres; check probes + SIGTERM
	./scripts/smoke.sh $(IMAGE)

db-up: ## Start compose Postgres and wait until healthy
	docker compose up -d --wait postgres

db-down: ## Stop compose services (data kept)
	docker compose down

db-reset: ## Stop compose services and delete their data
	docker compose down -v

migrate-up: ## Apply pending migrations to DATABASE_URL (from $(ENV_FILE))
	@$(LOAD_ENV) && go run ./cmd/migrate up

migrate-down: ## Roll back migrations: N=1 (default), N=3, or N=all
	@$(LOAD_ENV) && go run ./cmd/migrate down $(or $(N),1)

migrate-version: ## Print the current migration version
	@$(LOAD_ENV) && go run ./cmd/migrate version

migrate-new: ## Create the next migration pair: make migrate-new name=create_users
	@./scripts/migrate-new.sh "$(name)"

sqlc: ## Generate internal/db from db/queries + migrations
	$(SQLC) generate

sqlc-check: ## Fail if generated sqlc code is stale or queries don't compile
	$(SQLC) diff
	$(SQLC) vet

# Local stand-in for CI while GitHub Actions is off (ADR-0004). Run before every push.
ci: tidy-check fmt-check sqlc-check lint test vuln smoke ## Run every pre-push check
	@echo "ci: all checks passed"

tools: $(GOLANGCI) $(GOVULN) ## Install pinned dev tools into bin/

$(GOLANGCI):
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOVULN):
	GOBIN=$(BIN_DIR) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
