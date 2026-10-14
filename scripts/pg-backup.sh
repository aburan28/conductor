#!/usr/bin/env bash
# Back up or restore Conductor's Postgres database. docs/OPERATIONS.md explains when to use
# which, and what a dump does and does not contain.
#
#   scripts/pg-backup.sh backup  [FILE]   dump DATABASE_URL to FILE (default: conductor-<UTC time>.dump)
#   scripts/pg-backup.sh restore FILE     restore FILE into DATABASE_URL, which must be an empty database
#   scripts/pg-backup.sh verify  FILE     list FILE's contents and check it has Conductor's schema
#
# The dump is pg_dump's custom format: compressed, restorable table by table, and readable
# by pg_restore of the same or a newer major version. It holds everything conductord keeps,
# including token hashes and the GitHub App's private key, so store it like a secret.
set -euo pipefail

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "pg-backup: $1 not found (install the PostgreSQL client tools)" >&2; exit 1; }
}

: "${DATABASE_URL:?set DATABASE_URL to the database to back up or restore into}"
cmd=${1:-}; shift || true

case "$cmd" in
backup)
  need pg_dump
  file=${1:-conductor-$(date -u +%Y%m%dT%H%M%SZ).dump}
  # --no-owner/--no-privileges: the restore target's role owns everything, whatever role
  # the source used. A dump is a consistent snapshot; conductord can keep running.
  pg_dump --format=custom --no-owner --no-privileges --file="$file.partial" "$DATABASE_URL"
  mv "$file.partial" "$file"
  echo "backed up to $file ($(wc -c <"$file" | tr -d ' ') bytes)"
  ;;
restore)
  need pg_restore; need psql
  file=${1:?restore needs a dump file}
  [ -r "$file" ] || { echo "pg-backup: cannot read $file" >&2; exit 1; }
  # Refuse to restore over a live schema: mixing a dump into existing tables is how a
  # restore silently produces a database that is neither the old state nor the new one.
  existing=$(psql "$DATABASE_URL" -XAtc "SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'schema_migrations'")
  if [ "$existing" != "0" ]; then
    echo "pg-backup: the target already has a Conductor schema; restore into an empty database (createdb), then point conductord at it" >&2
    exit 1
  fi
  pg_restore --no-owner --no-privileges --exit-on-error --single-transaction --dbname="$DATABASE_URL" "$file"
  version=$(psql "$DATABASE_URL" -XAtc "SELECT max(version) FROM schema_migrations")
  echo "restored $file; schema at $version"
  ;;
verify)
  need pg_restore
  file=${1:?verify needs a dump file}
  listing=$(pg_restore --list "$file")
  for table in schema_migrations tasks leases domain_events; do
    grep -q "TABLE DATA [^ ]* $table " <<<"$listing" || { echo "pg-backup: $file has no data for $table" >&2; exit 1; }
  done
  echo "$file looks like a Conductor dump ($(grep -c 'TABLE DATA' <<<"$listing") tables)"
  ;;
*)
  usage
  ;;
esac
