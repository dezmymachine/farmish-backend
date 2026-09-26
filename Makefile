# Farmish backend. Run `make help` for targets.

GOLANGCI_LINT_VERSION := v2.14.0
GOVULNCHECK_VERSION   := v1.8.0
SQLC_VERSION          := 1.31.1
OAPI_CODEGEN_VERSION  := v2.8.0
REDOCLY_VERSION       := 2.54.3
BIN_DIR   := $(CURDIR)/bin
GOLANGCI  := $(BIN_DIR)/golangci-lint
GOVULN    := $(BIN_DIR)/govulncheck
OAPI      := $(BIN_DIR)/oapi-codegen
API_GEN   := internal/http/api/api.gen.go
IMAGE     ?= farmish-backend:dev
# `make run` loads .env if present, else the committed example defaults.
ENV_FILE  ?= $(if $(wildcard .env),.env,.env.example)
LOAD_ENV  := set -a && . ./$(ENV_FILE) && set +a
# Admin connection to the compose Postgres; dbtest creates a throwaway DB per test.
TEST_DATABASE_URL ?= postgres://farmish:farmish@127.0.0.1:$(or $(FARMISH_PG_PORT),54320)/farmish?sslmode=disable
# Firebase Auth emulator from docker-compose; authtest creates users in it.
TEST_AUTH_EMULATOR_HOST ?= 127.0.0.1:$(or $(FARMISH_AUTH_EMULATOR_PORT),9099)
REDOCLY   := docker run --rm -u $$(id -u):$$(id -g) -v $(CURDIR):/spec -w /spec redocly/cli:$(REDOCLY_VERSION)
SQLC      := docker run --rm -u $$(id -u):$$(id -g) -v $(CURDIR):/src -w /src sqlc/sqlc:$(SQLC_VERSION)

.DEFAULT_GOAL := help
.PHONY: help run build test lint fmt fmt-check tidy tidy-check vuln docker-build smoke ci tools \
        db-up db-down db-reset migrate-up migrate-down migrate-version migrate-new sqlc sqlc-check \
        generate generate-check api-lint auth-up grant-admin revoke-admin

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n",$$1,$$2}'

run: ## Run the API locally (loads $(ENV_FILE))
	@$(LOAD_ENV) && go run ./cmd/api

build: ## Build a static binary to bin/api
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/api ./cmd/api

test: db-up auth-up ## Run all tests (incl. DB + Auth emulator tests) with the race detector
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' DBTEST_REQUIRED=1 \
	FIREBASE_AUTH_EMULATOR_HOST='$(TEST_AUTH_EMULATOR_HOST)' AUTHTEST_REQUIRED=1 \
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

smoke: docker-build db-up auth-up ## Run the image against compose Postgres; check probes + SIGTERM
	./scripts/smoke.sh $(IMAGE)

db-up: ## Start compose Postgres and wait until healthy
	docker compose up -d --wait postgres

auth-up: ## Start the Firebase Auth emulator (127.0.0.1:9099) and wait until healthy
	docker compose up -d --wait --build firebase-auth

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

grant-admin: ## Make a user admin: make grant-admin EMAIL=you@x.com (or FUID=<firebase uid>)
	@$(LOAD_ENV) && go run ./cmd/admin grant-admin $(if $(EMAIL),--email '$(EMAIL)') $(if $(FUID),--uid '$(FUID)')

revoke-admin: ## Remove admin: make revoke-admin EMAIL=you@x.com (or FUID=<firebase uid>)
	@$(LOAD_ENV) && go run ./cmd/admin revoke-admin $(if $(EMAIL),--email '$(EMAIL)') $(if $(FUID),--uid '$(FUID)')

sqlc: ## Generate internal/db from db/queries + migrations
	$(SQLC) generate

sqlc-check: ## Fail if generated sqlc code is stale or queries don't compile
	$(SQLC) diff
	$(SQLC) vet

generate: $(OAPI) ## Generate Go server types from api/openapi.yaml
	$(OAPI) -config api/oapi-codegen.yaml api/openapi.yaml

generate-check: $(OAPI) ## Fail if the generated API code is stale
	@dir=$$(mktemp -d) && trap 'rm -rf $$dir' EXIT && \
	sed "s#^output:.*#output: $$dir/api.gen.go#" api/oapi-codegen.yaml > $$dir/cfg.yaml && \
	$(OAPI) -config $$dir/cfg.yaml api/openapi.yaml && \
	diff -u $(API_GEN) $$dir/api.gen.go || { echo "$(API_GEN) is stale: run make generate"; exit 1; }

api-lint: ## Lint api/openapi.yaml with Redocly (redocly.yaml rules)
	$(REDOCLY) lint --format=stylish api/openapi.yaml

# Local stand-in for CI while GitHub Actions is off (ADR-0004). Run before every push.
ci: tidy-check fmt-check api-lint generate-check sqlc-check lint test vuln smoke ## Run every pre-push check
	@echo "ci: all checks passed"

tools: $(GOLANGCI) $(GOVULN) $(OAPI) ## Install pinned dev tools into bin/

$(GOLANGCI):
	GOBIN=$(BIN_DIR) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

$(GOVULN):
	GOBIN=$(BIN_DIR) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

$(OAPI):
	GOBIN=$(BIN_DIR) go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)
