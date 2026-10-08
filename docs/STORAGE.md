# Storage: one bucket for sessions, checkpoints, and the database

Conductor can keep three kinds of state in one S3-compatible bucket:

| What | Why it leaves the machine | Sealed |
|---|---|---|
| Session resume records (`conductor backup`) | a replaced machine reopens its sessions | when a seal passphrase is set |
| Checkpoints (`conductor checkpoint push`) | continue a conversation on another machine | always (plaintext is refused) |
| The control-plane database (`conductor db …`) | Postgres stays local and fast; the bucket makes it durable | when `database.seal` is true |

Postgres remains the database: it runs on the machine (the macOS app bundles a private
one). What S3 adds is durability. Postgres archives every write-ahead-log segment to the
bucket as it fills (or at least every `archive_timeout`, 60 s by default) and takes a base
backup on a schedule, so a lost machine restores to within about a minute of where it
stopped, on any other machine that can reach the bucket.

## The settings file

`storage.json` lives beside the CLI's other state: `$CONDUCTOR_STATE_DIR/storage.json`, else
`~/.conductor/storage.json`. It is written with mode 0600 and never holds a secret unless
`auth.secret` is `"file"`.

```json
{
  "version": 1,
  "s3": {
    "bucket": "my-team-conductor",
    "region": "us-east-1",
    "endpoint": "",
    "path_style": false,
    "insecure": false,
    "prefix": "conductor"
  },
  "auth": {
    "method": "static",
    "access_key_id": "AKIA...",
    "secret": "keychain",
    "secret_access_key": "",
    "profile": ""
  },
  "uses": { "sessions": true, "checkpoints": true, "database": true },
  "database": {
    "archive_wal": true,
    "archive_timeout_seconds": 60,
    "base_backup_every_hours": 24,
    "keep_base_backups": 7,
    "seal": true
  }
}
```

| Field | Meaning |
|---|---|
| `s3.bucket` | required |
| `s3.region` | default `us-east-1`; `AWS_REGION` is used when empty |
| `s3.endpoint` | empty for AWS; a URL for MinIO, R2, Ceph, … |
| `s3.path_style` | address the bucket in the path (`https://host/bucket/key`); most non-AWS stores need it |
| `s3.insecure` | allow an `http://` endpoint (a local MinIO); refused otherwise |
| `s3.prefix` | every key is under this prefix; default `conductor` |
| `auth.method` | `static`, `profile`, or `environment` (below) |
| `auth.access_key_id` | `static` only |
| `auth.secret` | `static` only: `keychain` (macOS Keychain) or `file` |
| `auth.secret_access_key` | `static` with `secret: "file"` only |
| `auth.profile` | `profile` only; empty means `AWS_PROFILE`, then `default` |
| `uses.*` | which kinds of state go to the bucket; all default to true once a bucket is set |
| `database.*` | see "The database" below |

## Signing in to the bucket

Three methods, chosen with `auth.method`:

- **`static`**: an access key ID and secret access key.
  - `secret: "keychain"` keeps the secret in the macOS Keychain as a generic password with
    service `dev.conductor.s3` and account equal to the access key ID. The CLI reads it with
    `/usr/bin/security find-generic-password -s dev.conductor.s3 -a <id> -w`. The macOS app
    writes it and lists `/usr/bin/security` among the item's trusted applications, so the CLI
    can read it without a prompt.
  - `secret: "file"` stores it in `storage.json` itself (0600). Meant for Linux servers
    without a keychain.
- **`profile`**: a named profile from `~/.aws/config` and `~/.aws/credentials`
  (`AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` are honoured). It supports:
  - static keys in the profile;
  - `credential_process`;
  - IAM Identity Center (SSO), through the token cache that `aws sso login` writes;
  - `role_arn` with `source_profile`, through STS `AssumeRole`.
  Nothing secret is stored by Conductor.
- **`environment`**: whatever the machine provides, tried in this order:
  - `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN`;
  - web identity (`AWS_WEB_IDENTITY_TOKEN_FILE` with `AWS_ROLE_ARN`);
  - ECS container credentials;
  - the EC2 instance role, through IMDSv2.
  For servers and CI.

Credentials that expire (SSO, assumed roles, instance roles) are refreshed before they do.

## Precedence

1. `CONDUCTOR_BACKUP=off` turns every upload off.
2. `CONDUCTOR_BACKUP_S3_*` environment variables, when `CONDUCTOR_BACKUP_S3_BUCKET` is set,
   override the file entirely (the configuration that existed before this file).
3. `storage.json`.

## Commands

```
conductor storage show [--json]
conductor storage set --bucket B [--region R] [--endpoint URL] [--path-style] [--insecure]
                      [--prefix P]
                      [--auth static --access-key-id ID [--secret-from stdin|keychain]
                                                        [--secret-store keychain|file]]
                      [--auth profile [--profile NAME]]
                      [--auth environment]
                      [--sessions=BOOL] [--checkpoints=BOOL] [--database=BOOL]
                      [--archive-wal=BOOL] [--archive-timeout SECONDS]
                      [--base-backup-every HOURS] [--keep N] [--seal=BOOL] [--json]
conductor storage test [--json]      # resolve credentials, then put, get, list, delete a probe
conductor storage unset [--keep-secret]
conductor storage profiles [--json]  # the AWS profiles on this machine and their kind
```

`set` merges into the existing file: only the flags given change. `--secret-from stdin`
reads the secret from standard input and keeps it where `--secret-store` says: `keychain`
(the default on macOS) or `file` (the default elsewhere). `--secret-from keychain` means
the secret is already in the Keychain under `dev.conductor.s3` / the access key ID.
`unset` also removes that Keychain item unless `--keep-secret` is given.

`profiles --json` prints `[{"name": "dev", "kind": "sso", "region": "us-east-1"}, …]`.
`kind` is `static`, `sso`, `assume-role`, `process`, or `unknown`.

`show --json` prints:

```json
{ "configured": true, "path": "/Users/me/.conductor/storage.json", "source": "file",
  "s3": { ... }, "auth": { "method": "static", "access_key_id": "AKIA...", "secret": "keychain",
  "secret_access_key": "", "profile": "" },
  "uses": { "sessions": true, "checkpoints": true, "database": true },
  "database": { "archive_wal": true, "archive_timeout_seconds": 60, "base_backup_every_hours": 24,
                "keep_base_backups": 7, "seal": true },
  "effective_region": "us-east-1", "auth_description": "access key AKIA..., secret in the Keychain" }
```

The secret itself is never printed, and `uses` and `database` show effective values,
with defaults applied. `source` is `file`, `env`, or `none`. `off` is true when
`CONDUCTOR_BACKUP=off`.

`test --json` prints:

```json
{ "ok": true, "credentials": "static (keychain)", "location": "s3://bucket/prefix",
  "steps": [ { "name": "credentials", "ok": true, "ms": 3 },
             { "name": "put", "ok": true, "ms": 41 },
             { "name": "get", "ok": true, "ms": 22 },
             { "name": "list", "ok": true, "ms": 30 },
             { "name": "delete", "ok": true, "ms": 25 } ],
  "error": "" }
```

## Limits

An object is read in full only up to 128 MiB, which holds a 64 MiB WAL segment with the seal
overhead. A larger object is refused with an error, never returned in part.

## Sealing

Sealed objects are encrypted on the machine before upload with a passphrase:
`CONDUCTOR_CHECKPOINT_KEY`, or on macOS the Keychain generic password with service
`dev.conductor.seal` and account `default`. A bucket set up with sealing refuses plaintext.

## The database

Postgres archives to the bucket through two commands it calls itself:

```
archive_mode = on
archive_command = '/path/to/conductor db archive-wal %p %f'
restore_command = '/path/to/conductor db fetch-wal %f %p'     # only while restoring
archive_timeout = 60s
```

`conductor db archiving --data-dir DIR --write` writes the archive settings into the data
directory's `postgresql.auto.conf`, with the binary's absolute path. It prefixes
`CONDUCTOR_STATE_DIR=…` when that variable is set, because Postgres's environment may not
carry it. Restart Postgres when `archive_mode` changes. `conductor db restore` writes the
`restore_command`.

```
conductor db archiving --data-dir DIR [--write] [--json]
conductor db archive-wal <path> <name> [--data-dir DIR]     # archive_command
conductor db fetch-wal <name> <path> [--data-dir DIR]       # restore_command; exit 1 = not archived
conductor db base-backup [--dsn DSN] [--pg-bin DIR] [--no-prune] [--json]
conductor db backups [--system-id ID | --data-dir DIR] [--json]
conductor db status [--system-id ID | --data-dir DIR] [--local] [--json]
conductor db restore --data-dir DIR [--backup latest|ID] [--target-time RFC3339] [--system-id ID] [--json]
conductor db prune [--keep N] [--system-id ID | --data-dir DIR] [--json]
```

- **`base-backup`**
  - Runs `pg_basebackup -D - -Ft -X fetch`. The archive therefore contains the WAL it needs, and a base backup restores even without the WAL archive.
  - Streams the archive to the bucket (multipart, sealed on the way), then writes `manifest.json`.
  - Then prunes down to `keep_base_backups`.
  - The connection comes from `--dsn`, else `DATABASE_URL`, else the one `conductor up` saved.
  - The tools come from `--pg-bin`, else `CONDUCTOR_PG_BIN`, else `PATH`, else the usual install locations.
- **`restore`**
  - Rebuilds an empty data directory from a base backup and writes `recovery.signal`.
  - Postgres replays the archived WAL when it next starts. It stops at the end of the archive or at `--target-time`, then promotes itself to a normal, writable server.
  - Until promotion it accepts read-only connections: wait for `pg_is_in_recovery()` to be false before using it.
- **`prune`**
  - Keeps the newest N base backups.
  - Deletes the WAL that only older backups needed, comparing by log position, as `pg_archivecleanup` does.
  - Never deletes timeline `.history` files.
- **When archiving is off** (no bucket, `uses.database` false, or `database.archive_wal` false):
  - `archive-wal` succeeds without uploading, so Postgres does not pile up WAL on disk.
  - `archiving --write` sets `archive_mode = 'off'`.

### Keys in the bucket

Keys are namespaced by the cluster's system identifier, so two databases never share
segments:

```
<prefix>/db/<system-identifier>/key.json                       the sealed data key
<prefix>/db/<system-identifier>/wal/<file>[.sealed]
<prefix>/db/<system-identifier>/base/<UTC timestamp>/base.tar[.sealed]
<prefix>/db/<system-identifier>/base/<UTC timestamp>/manifest.json
```

`archive-wal` is idempotent. It succeeds if the bucket already holds identical content under
that name, and fails if the bucket holds different content. Postgres then keeps the segment
and retries, rather than lose either copy. Uploads use `If-None-Match: *`.

### Sealing the database archive

Each cluster has one random 256-bit data key:

- it is stored in the bucket as `key.json`, sealed with the seal passphrase (PBKDF2-SHA256 with 600,000 iterations, then AES-256-GCM);
- it is cached on the machine in `<state>/db-keys/<system id>.key` (0600), so archiving does not repeat the key derivation for every segment.

Objects are sealed in 1 MiB AES-256-GCM chunks:

- each object has its own subkey and a chunk counter;
- the final chunk is marked.

A sealed object therefore cannot be truncated, reordered or altered without failing to
open. A new machine needs only the bucket and the passphrase.

### JSON for the app

`db status --json`:

```json
{ "configured": true, "enabled": true, "archiving": true, "location": "s3://bucket/conductor",
  "sealed": true, "system_id": "7693956267215457548",
  "archive": { "system_id": "…", "archived": 1432, "last_wal": "00000001000000000000059A",
               "last_at": "2026-10-07T12:00:01Z", "last_error": "", "last_error_wal": "",
               "last_error_at": "", "last_base_backup": "20261007T030000Z",
               "last_base_backup_at": "2026-10-07T03:01:12Z", "last_base_backup_error": "" },
  "lag_seconds": 42, "failing": false,
  "base_backups": { "count": 7, "latest_id": "20261007T030000Z", "latest_at": "2026-10-07T03:01:12Z" },
  "wal": { "segments": 1432, "bytes": 24025956352, "first": "…", "last": "…" },
  "error": "" }
```

- `archive` is what this machine recorded, so it is present without network access (`--local`).
- `base_backups` and `wal` come from the bucket.
- Empty fields are omitted.

`db backups --json`:

```json
{ "system_id": "…", "location": "s3://…",
  "backups": [ { "version": 1, "id": "20261007T030000Z", "system_id": "…", "pg_version": "17.2",
                 "started_at": "…", "finished_at": "…", "start_lsn": "0/2000028", "end_lsn": "0/2000100",
                 "timeline": 1, "start_wal": "000000010000000000000002", "wal_segment_size": 16777216,
                 "size": 40606720, "stored_size": 40642123, "sealed": true, "key_id": "…" } ],
  "wal": { "segments": 12, "bytes": 201326592, "first": "…", "last": "…" } }
```

Without a cluster in the bucket, `backups` is an empty list and `error` says why.
