# macOS S3 recovery jobs

These jobs preserve PostgreSQL and local resume files in private S3. They do
not replace the live PostgreSQL database: task claims and leases still need a
running database. Every uploaded object has a unique name, uses a conditional
create-only PUT, requests SSE-S3 encryption, and is checked against S3's
SHA-256 response before the job reports success.

## PostgreSQL

Enable WAL archiving with `conductor db archiving --write --data-dir PATH`,
restart PostgreSQL, then take and restore a base backup as described by
`conductor db --help`. Verify `conductor db status --json` reports
`archiving: true`, `failing: false`, and a recent WAL. A base backup plus its
WAL is the recovery unit; a `pg_dump` alone does not provide continuous
recovery.

The `base-backup` command does not currently schedule itself from
`base_backup_every_hours`. Render
[`com.conductor.base-backup.plist.example`](com.conductor.base-backup.plist.example)
with the absolute CLI and runtime paths, validate it with `plutil -lint`, and
load it into the user's launchd domain. It runs daily at 03:15 local time.
`--no-prune` retains every uploaded base backup; set an explicit retention
policy only after checking recovery needs and cost.

[`backup-db.sh`](backup-db.sh) is an independent four-hour logical dump path.
Supply `CONDUCTOR_BACKUP_BUCKET`, `CONDUCTOR_BACKUP_DIR`, and standard libpq
variables (`PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`, and a mode-0600
`PGPASSFILE`). AWS CLI credentials must be available without embedding them
in the script or launchd plist. Run `pg_restore --list` on a downloaded dump
and verify its S3 checksum before relying on it. The script keeps only
`CONDUCTOR_LOCAL_BACKUPS_KEEP` local dumps (default two), while S3 objects
remain untouched.

## Raw checkpoint and session snapshot

[`backup-raw-snapshot.sh`](backup-raw-snapshot.sh) takes a one-time snapshot.
Set `CONDUCTOR_BACKUP_BUCKET` and `CONDUCTOR_BACKUP_DIR` to an existing local
staging directory with enough space for the raw source; optionally set
`CONDUCTOR_DATA_DIR`. Each UTC date becomes one tar object, preserving each
checkpoint's raw bytes and filename. Session records are stored in a separate
tar object. The final `MANIFEST.tsv` is uploaded only after every archive was
verified by S3 HEAD. Its absence means the snapshot did not complete.
If a single day's archive exceeds S3's 5 GiB single-PUT limit, the script
stops before upload; split that date into smaller groups and keep the same
create-only and manifest checks.

The script uses macOS `stat -f` and assumes Conductor-generated checkpoint
names without whitespace or newlines. It does not upload arbitrary chat
transcripts. Raw session records may contain private content; grant bucket
access accordingly. For an independent check, download the manifest and an
archive, compare their SHA-256 values and tar entry counts, and test extraction
into a separate directory. Restore only the records needed for recovery.

The Nov 13 live Mac used locally configured equivalents of these scripts and
jobs. The generic versions here were syntax-checked and shellchecked; the
live snapshot and logical dump were uploaded and read back separately. See
[`docs/UX_AUDIT_20261113.md`](../../docs/UX_AUDIT_20261113.md) for measured
status and remaining external gates.
