SHELL := /bin/bash
GO ?= go
BIN := bin
DATABASE_URL ?= postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable

export DATABASE_URL

.PHONY: all build test unit vet fmt db-up db-down db-wait run mcp clean e2e

all: vet build test

build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/conductord ./cmd/conductord
	$(GO) build -o $(BIN)/conductor ./cmd/conductor
	$(GO) build -o $(BIN)/conductor-mcp ./cmd/conductor-mcp
	@echo "built: $(BIN)/conductord $(BIN)/conductor $(BIN)/conductor-mcp"

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Pure-logic tests; no database required.
unit:
	$(GO) test ./internal/domain/... ./internal/selector/... ./internal/privacy/... \
	          ./internal/router/... ./internal/resource/... ./internal/harness/... \
	          ./internal/taskcard/... ./internal/config/... ./internal/localstate/...

# Full suite. When Docker is available, bring up the local Postgres first so
# the integration tests actually run; otherwise unset DATABASE_URL so they
# skip themselves instead of failing against an unreachable database.
test:
	@if docker compose up -d db >/dev/null 2>&1; then \
	  $(MAKE) --no-print-directory db-wait; \
	  $(GO) test ./...; \
	else \
	  echo ">> no docker compose; integration tests will skip (DATABASE_URL unset)"; \
	  env -u DATABASE_URL $(GO) test ./...; \
	fi

db-up:
	docker compose up -d db
	@$(MAKE) db-wait

db-wait:
	@echo -n "waiting for postgres"; \
	for i in $$(seq 1 60); do \
	  if docker compose exec -T db pg_isready -U conductor -d conductor >/dev/null 2>&1; then echo " ok"; exit 0; fi; \
	  echo -n "."; sleep 1; \
	done; echo " timeout"; exit 1

db-down:
	docker compose down -v

run: build
	$(BIN)/conductord --addr :8080

e2e: build
	./scripts/e2e.sh

clean:
	rm -rf $(BIN)
