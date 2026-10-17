#!/usr/bin/env bash
# Exercise S3 upload, checksum validation, and transactional restore with synthetic data.
set -euo pipefail
umask 077

if [[ ${EUID} -ne 0 || $# -ne 1 ]]; then
  echo 'usage: sudo smoke-restore.sh <private-backup-bucket>' >&2
  exit 2
fi
bucket=$1
source_db=conductor_smoke_src_$$
target_db=conductor_smoke_dst_$$
key="smoke/${source_db}.dump"
workdir=$(mktemp -d /var/backups/conductor/smoke.XXXXXXXX)
version=
cleanup() {
  status=$?
  trap - EXIT
  if [[ -n $version ]]; then
    if ! aws s3api delete-object --bucket "$bucket" --key "$key" \
      --version-id "$version" --output text >/dev/null; then
      echo "could not remove synthetic S3 object version: $key $version" >&2
      status=1
    fi
  fi
  if ! runuser -u postgres -- dropdb --if-exists "$target_db" >/dev/null 2>&1; then
    echo "could not remove synthetic database: $target_db" >&2
    status=1
  fi
  if ! runuser -u postgres -- dropdb --if-exists "$source_db" >/dev/null 2>&1; then
    echo "could not remove synthetic database: $source_db" >&2
    status=1
  fi
  rm -rf "$workdir" || status=1
  if (( status == 0 )); then
    echo 'Synthetic S3 restore drill passed; fixture and object version removed.'
  fi
  exit "$status"
}
trap cleanup EXIT

runuser -u postgres -- createdb --owner=conductor "$source_db"
runuser -u conductor -- psql -X -v ON_ERROR_STOP=1 --dbname="$source_db" \
  -c "CREATE TABLE smoke (id integer PRIMARY KEY, payload text); INSERT INTO smoke VALUES (1, 'one'), (2, 'two');"
chown conductor:conductor "$workdir"
dump="$workdir/source.dump"
runuser -u conductor -- /usr/lib/postgresql/18/bin/pg_dump -Fc \
  --dbname="$source_db" --file="$dump"
sha=$(sha256sum "$dump" | cut -d ' ' -f 1)
version=$(aws s3api put-object --bucket "$bucket" --key "$key" \
  --body "$dump" --server-side-encryption AES256 --query VersionId --output text)
runuser -u postgres -- createdb --owner=conductor "$target_db"
/opt/conductor/bootstrap/restore.sh "$bucket" "$key" "$sha" "$target_db"
runuser -u conductor -- /opt/conductor/bootstrap/table-counts.sh "$source_db" \
  >"$workdir/source.counts"
runuser -u conductor -- /opt/conductor/bootstrap/table-counts.sh "$target_db" \
  >"$workdir/target.counts"
diff -u "$workdir/source.counts" "$workdir/target.counts"
