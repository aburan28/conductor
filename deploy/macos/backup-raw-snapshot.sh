#!/bin/bash
# Archive raw checkpoint and session files under a unique, create-only S3 prefix.
set -euo pipefail
umask 077

AWS="${AWS_CLI:-aws}"
OPENSSL="${OPENSSL_BIN:-openssl}"
BUCKET="${CONDUCTOR_BACKUP_BUCKET:?set CONDUCTOR_BACKUP_BUCKET}"
SOURCE="${CONDUCTOR_DATA_DIR:-$HOME/.conductor}"
ROOT="${CONDUCTOR_BACKUP_DIR:?set CONDUCTOR_BACKUP_DIR to a local staging directory}"
[[ -d "$SOURCE/checkpoints" && -d "$SOURCE/sessions" ]] || {
  echo "ERROR: checkpoint and session directories must exist under $SOURCE" >&2
  exit 1
}
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)-$("$OPENSSL" rand -hex 4)"
PREFIX="raw-snapshots/$RUN_ID"
DEST="$ROOT/raw-snapshot-$RUN_ID"
mkdir -m 700 "$DEST"
MANIFEST="$DEST/MANIFEST.tsv"
printf 'archive\tentries\tbytes\tsha256\ts3_key\n' > "$MANIFEST"

upload_archive() {
  local archive="$1" entries="$2" record="${3:-true}" name key size hex b64 actual_size actual_checksum actual_sse
  name="${archive##*/}"
  key="$PREFIX/$name"
  size="$(stat -f %z "$archive")"
  if (( size > 5368709120 )); then
    echo "ERROR: $name exceeds S3's 5 GiB single-PUT limit; split this date's files" >&2
    return 1
  fi
  hex="$(shasum -a 256 "$archive" | cut -d ' ' -f 1)"
  b64="$("$OPENSSL" dgst -sha256 -binary "$archive" | "$OPENSSL" base64 -A)"
  "$AWS" s3api put-object --bucket "$BUCKET" --key "$key" --body "$archive" \
    --if-none-match '*' --server-side-encryption AES256 \
    --checksum-sha256 "$b64" --metadata "sha256=$hex" > /dev/null
  read -r actual_size actual_checksum actual_sse <<< "$("$AWS" s3api head-object \
    --bucket "$BUCKET" --key "$key" --checksum-mode ENABLED \
    --query '[ContentLength,ChecksumSHA256,ServerSideEncryption]' --output text)"
  if [[ "$actual_size" != "$size" || "$actual_checksum" != "$b64" || "$actual_sse" != AES256 ]]; then
    echo "ERROR: S3 verification failed for $key" >&2
    return 1
  fi
  if [[ "$record" == true ]]; then
    printf '%s\t%s\t%s\t%s\t%s\n' "$name" "$entries" "$size" "$hex" "$key" >> "$MANIFEST"
  fi
  echo "verified s3://$BUCKET/$key entries=$entries bytes=$size sha256=$hex"
}

# Checkpoint names are generated IDs without whitespace or newlines. Grouping by
# their UTC date keeps every object below S3's single-PUT size limit.
while IFS= read -r day; do
  list="$DEST/checkpoints-$day.list"
  archive="$DEST/checkpoints-$day.tar"
  find "$SOURCE/checkpoints" -maxdepth 1 -type f \
    \( -name "ck-${day}T*.ckpt" -o -name "ck-${day}T*.json" \) -print \
    | sed "s@^$SOURCE/checkpoints/@@" | sort > "$list"
  entries="$(wc -l < "$list" | tr -d ' ')"
  [[ "$entries" -gt 0 ]] || continue
  tar -C "$SOURCE/checkpoints" -cf "$archive" -T "$list"
  archived="$(tar -tf "$archive" | wc -l | tr -d ' ')"
  [[ "$archived" == "$entries" ]] || { echo "ERROR: incomplete archive $archive" >&2; exit 1; }
  upload_archive "$archive" "$entries"
done < <(find "$SOURCE/checkpoints" -maxdepth 1 -type f \
  \( -name 'ck-*.ckpt' -o -name 'ck-*.json' \) -print \
  | awk -F/ '{print substr($NF,4,8)}' | sort -u)

# Sessions are small and mutable, so the run ID identifies this exact snapshot.
find "$SOURCE/sessions" -type f -print \
  | sed "s@^$SOURCE/@@" | sort > "$DEST/sessions.list"
entries="$(wc -l < "$DEST/sessions.list" | tr -d ' ')"
if [[ "$entries" -gt 0 ]]; then
  tar -C "$SOURCE" -cf "$DEST/sessions.tar" -T "$DEST/sessions.list"
  archived="$(tar -tf "$DEST/sessions.tar" | wc -l | tr -d ' ')"
  [[ "$archived" == "$entries" ]] || { echo 'ERROR: incomplete sessions archive' >&2; exit 1; }
  upload_archive "$DEST/sessions.tar" "$entries"
fi

# Publish the manifest last. Its presence means every listed object was read back
# through S3 HEAD with a matching server-side SHA-256 checksum and encryption.
upload_archive "$MANIFEST" "$(wc -l < "$MANIFEST" | tr -d ' ')" false
echo "complete s3://$BUCKET/$PREFIX/ local=$DEST"
