SHELL := /bin/bash
GO ?= go
BIN := bin
DATABASE_URL ?= postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable

export DATABASE_URL

.PHONY: all setup install migrate build test unit vet fmt db-up db-down db-wait db-reset run mcp clean e2e help

all: vet build test

# One command from a fresh checkout: prerequisites, Postgres, migrations, binaries on the
# PATH, and a configured ~/.zshrc. Idempotent — re-run it to repair a partial setup.
setup:
	@MAKE="$(MAKE)" ./scripts/setup.sh

# Everything except touching your shell rc.
install:
	@MAKE="$(MAKE)" SKIP_SHELL_RC=1 ./scripts/setup.sh

# Apply the schema without creating a tenant or minting a token.
migrate: build
	$(BIN)/conductord migrate

help:
	@echo "make setup      one-command dev setup (db, build, install, zshrc)"
	@echo "make install    same, but leaves your shell rc alone"
	@echo "make migrate    apply database migrations"
	@echo "make db-up      start Postgres         make db-down   stop and wipe it"
	@echo "make db-reset   wipe and re-migrate    make build     build the binaries"
	@echo "make unit       tests without a db     make test      the full suite"
	@echo "make e2e        scripted end-to-end demo"

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
	          ./internal/taskcard/... ./internal/config/...

# Full suite; integration tests skip themselves unless DATABASE_URL points at a live db.
test:
	$(GO) test ./...

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

# Throw the data away and come back to an empty, migrated schema.
db-reset: db-down db-up migrate

run: build
	$(BIN)/conductord --addr :8080

e2e: build
	./scripts/e2e.sh

clean:
	rm -rf $(BIN)
