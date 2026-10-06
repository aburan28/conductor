#!/usr/bin/env bash
# ci-local.sh — run the same checks as CI (.github/workflows/ci.yml) on this machine, before
# pushing, so a pull request is not the first place a gofmt slip or a failing test shows up.
#
#   scripts/ci-local.sh            quick: gofmt, vet, staticcheck, build, tests without a database
#   scripts/ci-local.sh full       everything CI runs on Linux: quick + govulncheck, the
#                                  Postgres-backed suite, the race detector, and the end-to-end script
#   scripts/ci-local.sh STEP...    only the named steps, in order (see `steps` below)
#
# The database. The integration tests are the point of the full run, so it never lets them
# skip silently (CI refuses that too). It uses, in order:
#   1. $CI_DATABASE_URL, if set — an existing database you want the tests to use;
#   2. a throwaway Postgres from local binaries (initdb/pg_ctl on PATH, or the usual Debian,
#      Homebrew and Postgres.app locations), in a temp directory on a free port;
#   3. a throwaway postgres:17-alpine container, if Docker is running.
# Whatever it starts, it stops on exit. If none of these is possible the full run fails and
# says why; quick never needs a database.
#
# Make targets wrap this: `make check` (quick), `make ci` (full), and `make hooks` installs a
# pre-push hook that runs the quick checks on every `git push` (skip once with --no-verify).
set -euo pipefail

cd "$(dirname "$0")/.."

# Pinned tools. These are the versions CI runs (ci.yml), and the newest that build with the
# Go 1.25 toolchain go.mod selects. Change them here and there together.
STATICCHECK_VERSION=v0.7.0
GOVULNCHECK_VERSION=v1.7.0

bold=$'\033[1m' red=$'\033[31m' green=$'\033[32m' dim=$'\033[2m' reset=$'\033[0m'
[ -t 1 ] || { bold= red= green= dim= reset=; }

step() { printf '\n%s── %s%s\n' "$bold" "$*" "$reset"; }
die() { printf '%sci-local: %s%s\n' "$red" "$*" "$reset" >&2; exit 1; }

# --- database ------------------------------------------------------------------------------

DB_CLEANUP=()
cleanup() {
  local c
  for c in "${DB_CLEANUP[@]+"${DB_CLEANUP[@]}"}"; do eval "$c" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

free_port() {
  local p
  for p in $(seq 55500 55599); do
    if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then echo "$p"; return; fi
  done
  die "no free port in 55500-55599 for a throwaway Postgres"
}

wait_tcp() { # host port seconds
  local i
  for i in $(seq 1 "$3"); do
    (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && return 0
    sleep 1
  done
  return 1
}

find_pg_bin() {
  if command -v initdb >/dev/null 2>&1 && command -v pg_ctl >/dev/null 2>&1; then
    dirname "$(command -v initdb)"; return
  fi
  local d
  for d in /usr/lib/postgresql/*/bin /opt/homebrew/opt/postgresql@*/bin /usr/local/opt/postgresql@*/bin \
           /opt/homebrew/bin /Applications/Postgres.app/Contents/Versions/latest/bin; do
    [ -x "$d/initdb" ] && [ -x "$d/pg_ctl" ] && { echo "$d"; return; }
  done
  return 1
}

# initdb refuses to run as root; in a root container, run the server as the postgres user.
as_pg() {
  if [ "$(id -u)" -eq 0 ]; then
    id postgres >/dev/null 2>&1 || die "running as root and there is no postgres user to run a throwaway Postgres as"
    su postgres -s /bin/sh -c "$*"
  else
    sh -c "$*"
  fi
}

start_local_postgres() {
  local bin port dir
  bin=$(find_pg_bin) || return 1
  port=$(free_port)
  dir=$(mktemp -d "${TMPDIR:-/tmp}/conductor-ci-pg.XXXXXX")
  [ "$(id -u)" -eq 0 ] && chown postgres "$dir"
  echo "${dim}throwaway Postgres from $bin on 127.0.0.1:$port${reset}"
  as_pg "'$bin/initdb' -D '$dir/data' -U conductor --auth=trust -E UTF8 >'$dir/initdb.log' 2>&1" ||
    { cat "$dir/initdb.log" >&2; return 1; }
  as_pg "'$bin/pg_ctl' -D '$dir/data' -l '$dir/server.log' -o \"-p $port -k '$dir' -c listen_addresses=127.0.0.1 -c fsync=off\" -w start >/dev/null" ||
    { cat "$dir/server.log" >&2; return 1; }
  DB_CLEANUP+=("as_pg \"'$bin/pg_ctl' -D '$dir/data' -m immediate stop\"; rm -rf '$dir'")
  as_pg "'$bin/createdb' -h 127.0.0.1 -p $port -U conductor conductor" ||
    { cat "$dir/server.log" >&2; return 1; }
  export DATABASE_URL="postgres://conductor@127.0.0.1:$port/conductor?sslmode=disable"
}

start_docker_postgres() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || return 1
  local id port
  id=$(docker run -d --rm -e POSTGRES_USER=conductor -e POSTGRES_PASSWORD=conductor \
        -e POSTGRES_DB=conductor -p 127.0.0.1::5432 postgres:17-alpine) || return 1
  DB_CLEANUP+=("docker rm -f $id")
  port=$(docker port "$id" 5432/tcp | head -1 | sed 's/.*://')
  echo "${dim}throwaway Postgres in container ${id:0:12} on 127.0.0.1:$port${reset}"
  local i
  for i in $(seq 1 60); do
    docker exec "$id" pg_isready -U conductor -d conductor >/dev/null 2>&1 && break
    sleep 1
  done
  export DATABASE_URL="postgres://conductor:conductor@127.0.0.1:$port/conductor?sslmode=disable"
}

DB_READY=
ensure_database() {
  [ -n "$DB_READY" ] && return
  if [ -n "${CI_DATABASE_URL:-}" ]; then
    export DATABASE_URL="$CI_DATABASE_URL"
    echo "${dim}using CI_DATABASE_URL${reset}"
  elif ! start_local_postgres && ! start_docker_postgres; then
    die "the full run needs Postgres 16+ and found none to start.
  Install it (apt install postgresql, brew install postgresql@17) or start Docker,
  or point CI_DATABASE_URL at a database the tests may write to."
  fi
  # The integration tests skip themselves when they cannot reach a database, which would
  # turn a broken setup into a green run. CI fails on that; so does this.
  local out
  out=$(go test ./internal/db/ -run TestConcurrentClaimsYieldExactlyOneLease -count=1 -v 2>&1) ||
    { echo "$out" >&2; die "the database probe test failed"; }
  if grep -q -- "--- SKIP" <<<"$out"; then
    echo "$out" >&2
    die "the integration tests skipped: DATABASE_URL does not reach Postgres"
  fi
  DB_READY=1
}

# --- steps ---------------------------------------------------------------------------------

do_gofmt() {
  step "gofmt"
  local unformatted
  unformatted=$(gofmt -l .)
  if [ -n "$unformatted" ]; then
    echo "These files are not gofmt'd (fix with: gofmt -w <file>, or make fmt):"
    echo "$unformatted"
    return 1
  fi
}
do_vet()          { step "go vet";      go vet ./...; }
do_staticcheck()  { step "staticcheck"; go run "honnef.co/go/tools/cmd/staticcheck@$STATICCHECK_VERSION" ./...; }
do_govulncheck()  { step "govulncheck"; go run "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" ./...; }
do_build()        { step "build";       make --no-print-directory build; }
do_unit() {
  step "tests (no database)"
  env -u DATABASE_URL go test ./... -count=1
}
do_test() {
  ensure_database
  step "tests (Postgres)"
  go test ./... -count=1
}
do_race() {
  ensure_database
  step "tests with the race detector"
  go test -race ./... -count=1
}
do_e2e() {
  ensure_database
  step "end-to-end"
  ./scripts/e2e.sh
}

steps="gofmt vet staticcheck govulncheck build unit test race e2e"
quick="gofmt vet staticcheck build unit"
full="gofmt vet staticcheck govulncheck build test race e2e"

case "${1:-quick}" in
  quick) set -- $quick ;;
  full)  set -- $full ;;
  -h|--help|help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; echo; echo "steps: $steps"; exit 0 ;;
esac

start=$(date +%s)
for s in "$@"; do
  case " $steps " in *" $s "*) ;; *) die "unknown step \"$s\" (steps: $steps)" ;; esac
  if ! "do_$s"; then
    printf '\n%sci-local: %s failed%s — fix it before pushing (git push --no-verify skips the hook).\n' "$red" "$s" "$reset" >&2
    exit 1
  fi
done
printf '\n%sci-local: %s passed in %ss%s\n' "$green" "$*" "$(( $(date +%s) - start ))" "$reset"
