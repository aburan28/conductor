#!/usr/bin/env bash
# Print exact per-table row counts for comparing a stopped source with a restore.
set -euo pipefail

if [[ $# -ne 1 || ! $1 =~ ^[a-z_][a-z0-9_]{0,62}$ ]]; then
  echo 'usage: table-counts.sh <database>' >&2
  exit 2
fi
database=$1
LC_ALL=C psql --no-psqlrc -X -A -t -F $'\t' -v ON_ERROR_STOP=1 \
  --dbname="$database" <<'SQL' | LC_ALL=C sort
SELECT format('SELECT %L, count(*) FROM %I.%I;',
              schemaname || '.' || tablename, schemaname, tablename)
FROM pg_catalog.pg_tables
WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
ORDER BY schemaname, tablename
\gexec
SQL
