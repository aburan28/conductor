SHELL := /bin/bash
GO ?= go
BIN := bin
DATABASE_URL ?= postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable
ENDPOINT ?= http://localhost:8080
INSTALL_PREFIX ?= $(HOME)/.local
INSTALL_BIN := $(INSTALL_PREFIX)/bin
CONDUCTOR_BINS := conductord conductor conductor-mcp
TOKEN_FILE := .conductor/.bootstrap-token

# Upstream holding the GitHub releases `make install` downloads. Derived from
# the origin remote so forks work; override with REPO=owner/name. (No literal
# parentheses in the shell command: make would eat them inside $(shell ...).)
GIT_REPO := $(shell git remote get-url origin 2>/dev/null | sed 's|^https://github.com/||; s|^git@github.com:||; s|\.git$$||; s|/$$||')
ifneq (,$(findstring /,$(GIT_REPO)))
REPO ?= $(GIT_REPO)
else
REPO ?= aburan28/conductor
endif

export DATABASE_URL

# Stamp the version into every binary so `conductor version` and `conductor doctor` can tell
# a stale install from a current one. Named BUILD_* because VERSION already selects which
# release `make install` downloads.
MODULE := $(shell $(GO) list -m 2>/dev/null)
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo devel)
BUILD_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -X $(MODULE)/internal/version.version=$(BUILD_VERSION) -X $(MODULE)/internal/version.commit=$(BUILD_COMMIT)

.PHONY: all build test unit vet fmt db-up db-down db-wait bootstrap setup run serve login up down mcp wrap claude codex opencode clean e2e install install-local uninstall

all: vet build test

build:
	@mkdir -p $(BIN)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/conductord ./cmd/conductord
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/conductor ./cmd/conductor
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/conductor-mcp ./cmd/conductor-mcp
	@echo "built: $(BIN)/conductord $(BIN)/conductor $(BIN)/conductor-mcp"

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

# Pure-logic tests; no database required.
unit:
	$(GO) test ./internal/domain/... ./internal/selector/... ./internal/privacy/... \
	          ./internal/router/... ./internal/resource/... ./internal/harness/... \
	          ./internal/taskcard/... ./internal/config/... ./internal/localstate/... \
	          ./internal/checkpoint/... ./internal/githubapp/...

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

# Bootstrap the database and first tenant/project/principal/token. The DSN
# comes from DATABASE_URL (exported above); pass extra bootstrap flags via
# BOOTSTRAP_FLAGS, e.g. make bootstrap BOOTSTRAP_FLAGS="--org acme --project myrepo".
# Bootstrap also writes this machine's CLI login directly (see --no-login), and the
# freshly minted token is captured into $(TOKEN_FILE) so `make login` can re-use it
# without you copy-pasting from the terminal.
bootstrap: build db-up
	@$(BIN)/conductord bootstrap $(BOOTSTRAP_FLAGS) | tee >(grep -o 'cdt_[A-Za-z0-9_-]*' > $(TOKEN_FILE))
	@echo "token saved to $(TOKEN_FILE)"

# One-shot setup: start Postgres, build binaries, bootstrap the database.
setup: bootstrap

# Start the control plane on 127.0.0.1:8080 (loopback; no TLS needed).
# Runs in foreground; the database is left running. Override the address with
# ADDR=0.0.0.0:8080 (requires --insecure or TLS, which conductord enforces).
serve run: build db-up
	$(BIN)/conductord $(if $(ADDR),--addr $(ADDR))

# One command from a cold checkout to a running, logged-in control plane. A thin wrapper over
# `conductor up`, so `make up` and `conductor up` share one pidfile, one log
# (~/.conductor/runtime) and one login, and `conductor down` stops either. Docker is used only
# when no Postgres answers at the DSN. DATABASE_URL is passed through only when you set it:
# the Makefile's default would otherwise override the DSN `conductor up` chooses itself.
#   make up                         # defaults
#   make up DATABASE_URL=postgres://me@localhost:5432/conductor ADDR=127.0.0.1:9090
up: build
	$(if $(filter file,$(origin DATABASE_URL)),env -u DATABASE_URL) $(BIN)/conductor up \
	  $(if $(filter command line environment,$(origin ENDPOINT)),--endpoint $(ENDPOINT)) \
	  $(if $(ADDR),--addr $(ADDR)) $(if $(PROJECT),--project $(PROJECT))

# Stop the control plane `make up` (or `conductor up`) started. Postgres keeps running;
# `make down DB=1` stops it too.
down: build
	$(BIN)/conductor down $(if $(DB),--db)

# Save the bootstrap token (or one you pass with TOKEN=…) as the conductor CLI's
# default credentials. Requires `conductord` to already be reachable at ENDPOINT.
#   make login                 # uses the token from the last `make bootstrap`
#   make login TOKEN=cdt_…     # uses an explicit token (e.g. from `conductor member add`)
#   make login PROJECT=myrepo  # pin a default project (auto-detected if there is only one)
login: build
	@if [[ -n "$(TOKEN)" ]]; then \
	  tok="$(TOKEN)"; \
	elif [[ -s $(TOKEN_FILE) ]]; then \
	  tok=$$(cat $(TOKEN_FILE)); \
	else \
	  echo "no token: pass TOKEN=cdt_… or run make bootstrap first" >&2; exit 1; \
	fi; \
	$(BIN)/conductor login --endpoint $(ENDPOINT) --token $$tok \
	  $(if $(PROJECT),--project $(PROJECT))

# Register a Conductor session, start the heartbeat sidecar, and launch the named
# harness with whatever args you pass. Example:
#   make wrap claude ARGS="--model opus"
#   make wrap codex
#   make wrap opencode
wrap: build
	$(BIN)/conductor wrap $(HARNESS) $(ARGS)

claude:
	@$(MAKE) wrap HARNESS=claude ARGS="$(ARGS)"
codex:
	@$(MAKE) wrap HARNESS=codex ARGS="$(ARGS)"
opencode:
	@$(MAKE) wrap HARNESS=opencode ARGS="$(ARGS)"

e2e: build
	./scripts/e2e.sh

clean:
	rm -rf $(BIN)

# Download the released binaries for this platform from the GitHub releases of
# $(REPO) into INSTALL_BIN (default ~/.local/bin), and idempotently add that
# directory to PATH in your shell's startup file (zsh, bash or fish; for other
# shells the line to add is printed). No Go toolchain needed. Pin a release with
# VERSION=vX.Y.Z; by default the latest release is used. Override the prefix
# with: make install INSTALL_PREFIX=/usr/local (needs sudo for /usr/local).
# To install a build of the current tree instead, use install-local.
install:
	@mkdir -p $(INSTALL_BIN)
	@./scripts/install-release.sh $(REPO) $(INSTALL_BIN) $(if $(VERSION),$(VERSION),)
	@./scripts/install-path.sh $(INSTALL_BIN)
	@echo "installed: $(CONDUCTOR_BINS:%=$(INSTALL_BIN)/%)"

# Build the binaries from the current tree and copy them into INSTALL_BIN.
install-local: build
	@mkdir -p $(INSTALL_BIN)
	@for bin in $(CONDUCTOR_BINS); do \
	  install -m 0755 $(BIN)/$$bin $(INSTALL_BIN)/$$bin; \
	done
	@./scripts/install-path.sh $(INSTALL_BIN)
	@echo "installed: $(CONDUCTOR_BINS:%=$(INSTALL_BIN)/%)"

# Remove the binaries and the PATH entry added by `make install`. State is left
# alone; README.md "Removing Conductor" lists the rest (~/.conductor, each
# repository's .conductor/, the conductor-db container and its volume).
uninstall:
	@for bin in $(CONDUCTOR_BINS); do \
	  rm -f $(INSTALL_BIN)/$$bin; \
	done
	@./scripts/install-path.sh --remove $(INSTALL_BIN)
	@echo "removed: $(CONDUCTOR_BINS:%=$(INSTALL_BIN)/%)"
