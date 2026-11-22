#!/bin/bash
# Periodic backup of the Conductor control-plane database.
# Keep a bounded local cache and upload every verified dump to private S3.
set -euo pipefail
umask 077

DEST="${CONDUCTOR_BACKUP_DIR:-$HOME/.conductor/runtime/backups}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
KEEP="${CONDUCTOR_LOCAL_BACKUPS_KEEP:-2}"
BUCKET="${CONDUCTOR_BACKUP_BUCKET:?set CONDUCTOR_BACKUP_BUCKET}"
PREFIX="${CONDUCTOR_BACKUP_PREFIX:-database}"
AWS="${AWS_CLI:-aws}"
OPENSSL="${OPENSSL_BIN:-openssl}"
PG_DUMP="${PG_DUMP_BIN:-pg_dump}"
PG_RESTORE="${PG_RESTORE_BIN:-pg_restore}"
[[ "$KEEP" =~ ^[1-9][0-9]*$ ]] || { echo 'ERROR: local backup retention must be positive' >&2; exit 1; }

mkdir -p "$DEST"
chmod 700 "$DEST"
TMP="$DEST/.conductor-$STAMP.dump.tmp"
FILE="$DEST/conductor-$STAMP.dump"
KEY="$PREFIX/conductor-$STAMP.dump"
trap 'rm -f "$TMP"' EXIT

# A failed dump or invalid archive cannot become a completed backup.
if ! "$PG_DUMP" -Fc > "$TMP"; then
  echo "$(date -u +%FT%TZ) ERROR: pg_dump failed" >&2
  exit 1
fi
if ! "$PG_RESTORE" --list "$TMP" > /dev/null 2>&1; then
  echo "$(date -u +%FT%TZ) ERROR: pg_restore could not list dump" >&2
  exit 1
fi
# A same-second rerun must fail rather than replace an earlier dump.
ln "$TMP" "$FILE"
rm "$TMP"
chmod 600 "$FILE"

SIZE="$(stat -f %z "$FILE")"
SHA256="$(shasum -a 256 "$FILE" | awk '{print $1}')"
CHECKSUM_B64="$("$OPENSSL" dgst -sha256 -binary "$FILE" | "$OPENSSL" base64 -A)"

# S3 validates the supplied checksum before accepting the object.
"$AWS" s3api put-object \
  --bucket "$BUCKET" --key "$KEY" --body "$FILE" \
  --if-none-match '*' \
  --server-side-encryption AES256 \
  --checksum-sha256 "$CHECKSUM_B64" \
  --metadata "sha256=$SHA256" > /dev/null

read -r REMOTE_SIZE REMOTE_CHECKSUM REMOTE_SSE <<< "$("$AWS" s3api head-object \
  --bucket "$BUCKET" --key "$KEY" --checksum-mode ENABLED \
  --query '[ContentLength,ChecksumSHA256,ServerSideEncryption]' --output text)"
if [[ "$REMOTE_SIZE" != "$SIZE" || "$REMOTE_CHECKSUM" != "$CHECKSUM_B64" || "$REMOTE_SSE" != "AES256" ]]; then
  echo "$(date -u +%FT%TZ) ERROR: S3 size, checksum, or encryption mismatch for $KEY" >&2
  exit 1
fi

# Raw checkpoint export runs separately; it must not hold up the database
# dump schedule when individual object uploads stall.

# Rotate only the local cache; S3 retains the verified copies.
find "$DEST" -maxdepth 1 -type f -name 'conductor-*.dump' -print \
  | sort -r | tail -n +$((KEEP + 1)) | while IFS= read -r OLD; do
  rm -f "$OLD"
done

COUNT="$(find "$DEST" -maxdepth 1 -type f -name 'conductor-*.dump' | wc -l | tr -d ' ')"
echo "$(date -u +%FT%TZ) ok key=$KEY bytes=$SIZE sha256=$SHA256 local_retained=$COUNT"
