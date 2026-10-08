# Storage: one bucket for sessions, checkpoints, and the database

Conductor can keep three kinds of state in one S3-compatible bucket:

| What | Why it leaves the machine | Sealed |
|---|---|---|
| Session resume records (`conductor backup`) | a replaced machine reopens its sessions | when a seal passphrase is set |
| Checkpoints (`conductor checkpoint push`) | continue a conversation on another machine | always (plaintext is refused) |
| The control-plane database (`conductor db …`, planned: lands with the database archive pull request) | Postgres stays local and fast; the bucket makes it durable | when `database.seal` is true |

Postgres remains the database: it runs on the machine (the macOS app bundles a private
one). What S3 adds is durability. Once the database archive lands (planned, see "The
database" below), Postgres will archive every write-ahead-log segment to the bucket as it
fills (or at least every `archive_timeout`, 60 s by default) and take a base backup on a
schedule, so a lost machine restores to within about a minute of where it stopped, on any
other machine that can reach the bucket. This build does not archive the database yet.

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

**Planned.** The `conductor db` commands and the `archive_command` and `restore_command`
settings below land with the database archive pull request. In this build the `database.*`
settings are stored and shown by `conductor storage show`, but no WAL segment is archived and
no base backup is taken. The description that follows is the design, not the current
behaviour.

Postgres archives to the bucket through two commands it calls itself (planned):

```
archive_mode = on
archive_command = 'conductor db archive-wal %p %f'
restore_command = 'conductor db fetch-wal %f %p'     # only while restoring
archive_timeout = 60
```

Planned commands:

```
conductor db archiving --data-dir DIR [--write]   # print (or append to postgresql.auto.conf) the settings above
conductor db base-backup [--dsn DSN]              # pg_basebackup, streamed to the bucket
conductor db backups [--json]                     # base backups and the WAL range each can replay
conductor db status [--json]                      # last archived segment, last base backup, lag
conductor db restore --data-dir DIR [--backup latest|ID] [--target-time RFC3339]
conductor db prune [--keep N]                     # drop old base backups and the WAL only they need
```

Keys are namespaced by the cluster's system identifier, so two databases never share
segments (planned layout):

```
<prefix>/db/<system-identifier>/wal/<segment>[.sealed]
<prefix>/db/<system-identifier>/base/<UTC timestamp>/base.tar[.sealed]
<prefix>/db/<system-identifier>/base/<UTC timestamp>/manifest.json
```

`archive-wal` is idempotent. It succeeds if the bucket already holds a segment with
identical content, and it fails if the bucket holds different content under that name.
Postgres then keeps the segment and retries, rather than lose either copy.
