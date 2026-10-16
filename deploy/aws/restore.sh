#!/usr/bin/env bash
# Restores a verified snapshot into an empty conductor database; never overwrites data.
set -euo pipefail
umask 077

if [[ ${EUID} -ne 0 || $# -lt 3 || $# -gt 4 ]]; then
  echo "usage: sudo restore.sh <bucket> <dump-object-key> <expected-sha256> [database]" >&2
  exit 2
fi
bucket=$1
key=$2
expected=$3
database=${4:-conductor}
if [[ ! $expected =~ ^[[:xdigit:]]{64}$ ]]; then
  echo "expected SHA-256 must be 64 hex characters" >&2
  exit 2
fi
if [[ ! $database =~ ^[a-z_][a-z0-9_]{0,62}$ ]]; then
  echo "database name must be a simple PostgreSQL identifier" >&2
  exit 2
fi
if systemctl is-active --quiet conductor.service; then
  echo "refusing restore while conductor.service is active" >&2
  exit 1
fi
table_count=$(runuser -u conductor -- psql -Atqc \
  "SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')" "$database")
if (( table_count != 0 )); then
  echo "refusing restore into a nonempty database (${table_count} tables)" >&2
  exit 1
fi
workdir=$(mktemp -d /var/backups/conductor/restore.XXXXXXXX)
trap 'rm -rf "$workdir"' EXIT
dump="$workdir/source.dump"
aws s3 cp "s3://${bucket}/${key}" "$dump" --only-show-errors
echo "${expected}  ${dump}" | sha256sum --check --status
/usr/lib/postgresql/18/bin/pg_restore --list "$dump" >/dev/null
chown conductor:conductor "$workdir" "$dump"
runuser -u conductor -- /usr/lib/postgresql/18/bin/pg_restore \
  --exit-on-error --single-transaction --no-owner --no-acl \
  --dbname="$database" "$dump"
echo "Restore completed; compare source and destination row counts before activation."
