#!/usr/bin/env bash
#
# One-command development setup.
#
# Brings a fresh checkout to the point where `conductor` and `conductord` are on the PATH,
# Postgres is running and migrated, and a login token exists. Every step is idempotent:
# re-running this is the supported way to repair a half-finished setup.
#
# Usage: make setup   (or: scripts/setup.sh)
#
# Environment:
#   PREFIX          install directory for the binaries      (default: ~/.local/bin)
#   SHELL_RC        shell rc file to configure               (default: ~/.zshrc, or ~/.bashrc
#                                                             when $SHELL is bash)
#   DATABASE_URL    Postgres DSN                             (default: local docker compose db)
#   CONDUCTOR_ORG / CONDUCTOR_PROJECT / CONDUCTOR_PRINCIPAL  bootstrap identity
#   CONDUCTOR_ENDPOINT                                       (default: http://127.0.0.1:8080)
#   SKIP_SHELL_RC=1 do everything except touching the rc file
#   SKIP_BOOTSTRAP=1 migrate the database but mint no token

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin"
PREFIX="${PREFIX:-$HOME/.local/bin}"
DATABASE_URL="${DATABASE_URL:-postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable}"
CONDUCTOR_ENDPOINT="${CONDUCTOR_ENDPOINT:-http://127.0.0.1:8080}"
CONDUCTOR_ORG="${CONDUCTOR_ORG:-default}"
CONDUCTOR_PROJECT="${CONDUCTOR_PROJECT:-$(basename "$ROOT")}"
CONDUCTOR_PRINCIPAL="${CONDUCTOR_PRINCIPAL:-${USER:-operator}}"
export DATABASE_URL

BEGIN_MARK="# >>> conductor setup >>>"
END_MARK="# <<< conductor setup <<<"

step() { printf '\n\033[1m── %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- prerequisites

step "Checking prerequisites"
command -v go >/dev/null 2>&1 || die "go is not installed (Go 1.25+ required): https://go.dev/dl/"
command -v git >/dev/null 2>&1 || die "git is not installed"
command -v docker >/dev/null 2>&1 || die "docker is not installed: https://docs.docker.com/get-docker/"
docker compose version >/dev/null 2>&1 || die "docker compose v2 is not available"
docker info >/dev/null 2>&1 || die "the docker daemon is not running — start Docker and re-run"
note "go $(go version | awk '{print $3}'), docker $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo '?')"

# ---------------------------------------------------------------- database

step "Starting Postgres"
"${MAKE:-make}" -C "$ROOT" --no-print-directory db-up

# ---------------------------------------------------------------- build

step "Building binaries"
"${MAKE:-make}" -C "$ROOT" --no-print-directory build

# ---------------------------------------------------------------- install

step "Installing to $PREFIX"
mkdir -p "$PREFIX"
for binary in conductord conductor conductor-mcp; do
  install -m 0755 "$BIN/$binary" "$PREFIX/$binary"
  note "$PREFIX/$binary"
done

# ---------------------------------------------------------------- schema + token

TOKEN_LINE=""
if [[ "${SKIP_BOOTSTRAP:-0}" == "1" ]]; then
  step "Applying migrations (SKIP_BOOTSTRAP=1: no tenant, no token)"
  "$PREFIX/conductord" migrate | sed 's/^/   /'
else
  step "Migrating the database and bootstrapping $CONDUCTOR_ORG/$CONDUCTOR_PROJECT"
  BOOTSTRAP_OUT="$("$PREFIX/conductord" bootstrap --org "$CONDUCTOR_ORG" \
    --project "$CONDUCTOR_PROJECT" --principal "$CONDUCTOR_PRINCIPAL" --repo "$ROOT")"
  printf '%s\n' "$BOOTSTRAP_OUT" | sed 's/^/   /'
  TOKEN_LINE="$(printf '%s\n' "$BOOTSTRAP_OUT" | grep -o 'cdt_[A-Za-z0-9._-]*' | head -1 || true)"
fi

# ---------------------------------------------------------------- shell rc

if [[ "${SKIP_SHELL_RC:-0}" == "1" ]]; then
  step "Skipping shell configuration (SKIP_SHELL_RC=1)"
else
  if [[ -z "${SHELL_RC:-}" ]]; then
    case "${SHELL:-}" in
      */bash) SHELL_RC="$HOME/.bashrc" ;;
      *)      SHELL_RC="$HOME/.zshrc" ;;
    esac
  fi
  step "Configuring $SHELL_RC"
  touch "$SHELL_RC"

  # Replace any previous block rather than appending a second one, so re-running setup
  # keeps exactly one managed section.
  if grep -qF "$BEGIN_MARK" "$SHELL_RC"; then
    tmp="$(mktemp)"
    awk -v b="$BEGIN_MARK" -v e="$END_MARK" '
      $0 == b {skip=1} !skip {print} $0 == e {skip=0}
    ' "$SHELL_RC" > "$tmp"
    # Drop a trailing blank line left behind by the removed block.
    printf '%s\n' "$(cat "$tmp")" > "$SHELL_RC"
    rm -f "$tmp"
    note "replaced the existing conductor block"
  fi

  {
    printf '\n%s\n' "$BEGIN_MARK"
    printf '# Managed by scripts/setup.sh in %s — edit above or below this block.\n' "$ROOT"
    # Prepend only once, so re-sourcing the rc file does not grow $PATH.
    printf 'case ":$PATH:" in *":%s:"*) ;; *) export PATH="%s:$PATH" ;; esac\n' "$PREFIX" "$PREFIX"
    printf 'export DATABASE_URL="%s"\n' "$DATABASE_URL"
    printf 'export CONDUCTOR_ENDPOINT="%s"\n' "$CONDUCTOR_ENDPOINT"
    printf '%s\n' "$END_MARK"
  } >> "$SHELL_RC"
  note "PATH, DATABASE_URL and CONDUCTOR_ENDPOINT exported"
fi

# ---------------------------------------------------------------- done

step "Ready"
note "postgres:  $DATABASE_URL"
note "binaries:  $PREFIX/{conductord,conductor,conductor-mcp}"
if [[ -n "${SHELL_RC:-}" && "${SKIP_SHELL_RC:-0}" != "1" ]]; then
  note "reload:    source $SHELL_RC"
fi
echo
note "Next:"
note "  conductord &"
if [[ -n "$TOKEN_LINE" ]]; then
  note "  conductor login --endpoint $CONDUCTOR_ENDPOINT --token $TOKEN_LINE --project $CONDUCTOR_PROJECT"
else
  note "  conductor login --endpoint $CONDUCTOR_ENDPOINT --token <token> --project $CONDUCTOR_PROJECT"
fi
note "  conductor dashboard"
