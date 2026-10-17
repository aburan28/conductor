#!/usr/bin/env bash
set -euo pipefail
umask 077

: "${CONDUCTOR_BACKUP_BUCKET:?set CONDUCTOR_BACKUP_BUCKET}"
workdir=$(mktemp -d /var/backups/conductor/backup.XXXXXXXX)
trap 'rm -rf "$workdir"' EXIT
stamp=$(date -u +%Y%m%dT%H%M%SZ)
dump="$workdir/conductor-${stamp}.dump"
download="$workdir/verified.dump"
key="database/conductor-${stamp}.dump"

/usr/lib/postgresql/18/bin/pg_dump -Fc --dbname=conductor --file="$dump"
/usr/lib/postgresql/18/bin/pg_restore --list "$dump" >/dev/null
sha=$(sha256sum "$dump" | cut -d ' ' -f 1)
aws s3 cp "$dump" "s3://${CONDUCTOR_BACKUP_BUCKET}/${key}" \
  --sse AES256 --metadata "sha256=${sha}" --only-show-errors
aws s3 cp "s3://${CONDUCTOR_BACKUP_BUCKET}/${key}" "$download" --only-show-errors
echo "${sha}  ${download}" | sha256sum --check --status
/usr/lib/postgresql/18/bin/pg_restore --list "$download" >/dev/null
printf '%s %s %s\n' "$stamp" "$key" "$sha"
