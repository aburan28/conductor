# Operating conductord

How to run the control plane so that it stays up, tells you when it is not, and can be put
back after a bad day. DESIGN.md §26–§28 has the reasoning; this page has the knobs.

## Probes and signals

| Endpoint | Auth | Use it for |
|---|---|---|
| `GET /v1/health` | none | Liveness. 200 when the database answers a ping, 503 otherwise. It names the failure (`"database": "unreachable"`) and never quotes the driver's error, which can carry hosts and credentials; the full error is in the log under the response's request id. |
| `GET /v1/ready` | any token | Readiness and a summary for an operator: database, schema version against this binary, the shared scheduler heartbeat (`stale` past two minutes), and the GitHub poller (`ok` / `error` / `never_ran`). 503 when degraded. |
| `GET /metrics` | see below | Prometheus text format. |

`/metrics` describes the deployment rather than any tenant's data, but it is still not
public. With no `--metrics-token` it answers only direct loopback connections, and nothing
at all when `--behind-proxy` is set (every request would appear to come from the proxy).
With `--metrics-token TOKEN` (or `CONDUCTOR_METRICS_TOKEN`) it requires
`Authorization: Bearer TOKEN` from anywhere; that token is not a user token and grants
nothing else.

The metrics, all prefixed `conductor_`:

| Metric | Type | Meaning |
|---|---|---|
| `http_requests_total{route,method,status}` | counter | Requests by route *pattern* (`GET /v1/tasks/{task}`), never by raw path. Unmatched paths share `route="unmatched"`. |
| `http_request_duration_seconds{route}` | histogram | Includes event streams, which last as long as the connection. |
| `http_requests_in_flight`, `http_panics_total` | gauge, counter | |
| `scheduler_tick_duration_seconds` | histogram | One pass over every project. |
| `scheduler_errors_total{stage}` | counter | `tick`, `project`, `prune`, `budget`, `queue`. |
| `scheduler_last_tick_timestamp_seconds` | gauge | Per process. The shared heartbeat in `/v1/ready` covers every replica. |
| `leases_active`, `leases_reclaimed_total`, `leases_extended_total` | gauge, counters | Extended means outage recovery (below). |
| `events_written_total{type}` | counter | Domain events by type. |
| `retention_deleted_total{table}` | counter | |
| `github_polls_total{outcome}` | counter | `ok`, `error`, `skipped` (another replica polled). |
| `github_webhooks_deferred_total` | counter | Deliveries left to the poller because the check pool was full. |
| `event_streams_open`, `event_stream_feeds`, `event_streams_rejected_total` | gauges, counter | Open dashboards, projects being polled for them, connections refused by the caps. |
| `db_pool_*` | gauges, counters | `acquired_conns`, `idle_conns`, `total_conns`, `max_conns`, `acquires_total`, `empty_acquires_total`, `acquire_wait_seconds_total`. |

Worth alerting on: `/v1/ready` failing; `rate(scheduler_errors_total[5m]) > 0` for more
than a few minutes; `leases_extended_total` increasing (there was an outage);
`db_pool_empty_acquires_total` growing quickly (the pool is too small for the load).

**Logs.** Every request gets an id: the caller's `X-Request-Id` if it sent a well-formed one,
otherwise a generated one, echoed in the response's `X-Request-Id`. Each request is logged at
Info as one line — method, route pattern, path, status, duration, bytes, principal id and
request id. Query strings, headers and bodies are never logged (a query string can carry a
token; bodies carry task titles). `/v1/health`, `/v1/ready` and `/metrics` log at Debug
unless they fail. A recovered panic is logged at Error with its stack.

## Shutdown, restarts and outages

On SIGTERM or SIGINT, conductord:

1. stops the scheduler, the GitHub poller and peer links, ends open event streams, and
   cancels background webhook checks; the notification relay sends nothing new and finishes
   recording the requests already in flight (each bounded by a 10s timeout);
2. stops accepting connections and lets in-flight requests finish;
3. waits for all of that, then closes its database pool and exits.

The whole sequence is bounded by `--shutdown-timeout` (25s). Give the process manager a
grace period a little longer than that (Kubernetes' default is 30s).

**Leases survive an outage.** A worker renews its lease by heartbeating to the control
plane, so while every replica is down — an upgrade, a crash, a Postgres maintenance window —
no lease can be renewed. Each scheduler pass stamps a heartbeat shared by all replicas; when
the gap since the last stamp exceeds `--outage-after` (by default the tick timeout plus three
ticks, at least 30s), open leases are first extended by the gap, so each has the time it had
left when the outage began. The first start against a database no scheduler has ever ticked
against (a new install, or the first start of this version) gives every open lease one full
TTL instead. This runs before the listener opens, with or without `--no-scheduler`. A worker
that died while the control plane was healthy leaves no gap and is reclaimed on time.

If you run API-only replicas (`--no-scheduler`) beside separate scheduler replicas, the
heartbeat measures the scheduler's availability, not the API's: a scheduler that stays up
while every API replica is down will reclaim leases that could not be renewed. Run the
scheduler in the same processes that serve the API (the default) unless you have a reason
not to.

## Database

**Bounded calls.** Every pooled connection has `statement_timeout` (30s,
`--db-statement-timeout`) and `lock_timeout` (10s, `--db-lock-timeout`); negative disables
either. A value in the DSN itself (`?statement_timeout=...`) wins. Migrations lift both on
their own connection. Each scheduler pass is additionally bounded by `--tick-timeout` (30s),
so one hung query fails one pass instead of stalling the loop.

**Pool size.** Each process opens up to `max(8, pool_max_conns from the DSN)` connections.
That is per replica: N replicas need N times that from Postgres' `max_connections`.

**Migrations are forward-only.** conductord applies pending migrations at startup under an
advisory lock, so replicas starting together are safe. It refuses to start against a
database that has a migration it does not know — a newer conductord has run against it —
and says which. New migrations take the next unused four-digit prefix; the two `0002_*`
files predate that rule and cannot be renamed (the file name is the version a database has
recorded). The version is the whole name, applied in byte-wise order, so they are distinct
and deterministically ordered.

**Upgrades and rollback.** Back up, then start the new version; it migrates. There are no
down migrations, so rolling back means restoring the backup taken before the upgrade and
starting the old binary against it. Anything recorded between the upgrade and the rollback
is lost — keep the window short, or roll forward with a fix instead.

## Retention

The scheduler prunes, every ten minutes, in batches of 1000 rows (at most 20 batches per
table per pass, so a large backlog drains over several passes):

| Data | Kept for | Flag |
|---|---|---|
| Domain events | 90 days | `--retention-days` / `CONDUCTOR_RETENTION_DAYS` |
| Outbox rows a consumer delivered | 7 days, or the event window if shorter | (follows `--retention-days`) |
| Outbox rows never delivered | 30 days | `--outbox-undelivered-days` |
| Audit log | 365 days | `--audit-retention-days` / `CONDUCTOR_AUDIT_RETENTION_DAYS` |
| Idempotency keys | 24 hours | `--idempotency-ttl` |
| Usage buckets | 180 days (0, or at least 31: budgets look back 30) | `--usage-retention-days` |
| GitHub check-run records | 30 days | |
| Expired GitHub setup links | 1 hour after expiry | |

0 keeps that kind forever. Two kinds of event are never pruned: the newest event of each
aggregate (sequence numbers continue from it, and consumers detect a gap by them), and an
event whose outbox row is still waiting for delivery inside the undelivered window.

The outbox is written in the same transaction as every event. The notification relay
(`--notify-poll`, every 3s by default) claims the undelivered rows of projects that have a
notification channel, sends them, and marks them delivered — including rows it gives up on
after retries, so a dead endpoint does not hold events past their window. Rows of projects
with no channel are never claimed; the undelivered window is what bounds them.

## HTTP limits

| Limit | Default | Flag |
|---|---|---|
| Time to send request headers | 10s | |
| Time to send a request body | 30s | `--body-timeout` |
| Idle keep-alive connection | 120s | |
| Event-stream connections, total / per principal | 1000 / 16 | `--max-streams`, `--max-streams-per-principal` |
| Concurrent webhook-triggered pull request checks | 8 | `--max-webhook-checks` |

There is no server-wide read or write timeout: both stay armed for a whole response, and the
event stream is a long-lived one. The body deadline is set only on requests that have a body
and is cleared once the body has been read. Event streams on one project share one database
poll per second, however many are open. A stream past the caps gets 503 with `Retry-After`; a
webhook past the pool is acknowledged and its pull request is picked up by the next poll.

## Outbound notifications

Notification channels (README, "Notifications") make conductord send HTTP requests to URLs
project maintainers choose, from inside your network. By default it reaches only public
addresses over https: loopback, RFC 1918, link-local (including the cloud metadata address),
CGNAT and other non-public ranges are refused on the address actually dialed, after DNS, and
redirects are not followed. Requests go out directly, not through `HTTPS_PROXY`; allow
outbound 443 from conductord to your chat provider.

| Setting | Default | Flag |
|---|---|---|
| Relay poll interval (negative disables the relay) | 3s | `--notify-poll` |
| Allow loopback and private destinations (a chat server on the LAN) | off | `--notify-allow-private-networks` |
| Allow plain `http://` URLs (local testing) | off | `--notify-allow-http` |
| Per-request timeout / channels sent to at once | 10s / 4 | |
| Retries | backoff from 15s doubling to 1h; give up after 8 attempts or 24h | |

Give every replica the same notification flags: the one that handles a request validates the
URL, and whichever claims an event sends it. `conductor_notifications_total{kind,outcome}`
counts sends (`delivered`, `retry`, `failed`, `expired`) and
`conductor_notify_relay_errors_total` counts failed passes.

## Running more than one replica

Several conductord processes may share one database behind a load balancer. What they
agree on lives in Postgres:

- claims, leases, reservations, tasks and events (transactional, `SKIP LOCKED` scheduling);
- the scheduler's liveness heartbeat (outage recovery above), the last budget alert level
  per project, and which stalled attempts were announced, so neither a restart nor a second
  replica re-announces a budget crossing or a stall;
- notification channels and which events each has been sent: the relay claims outbox rows
  with `SKIP LOCKED`, so replicas split the backlog and each event goes to a channel once
  (receivers should still deduplicate by event id: delivery is at least once);
- the GitHub App's credentials, pending setup links, and the check run posted on each
  commit. Polling is gated by an advisory lock, so one replica polls at a time and another
  takes over when it dies. An app set up through one replica is served by all within 15s.

What stays per process:

- **MCP HTTP sessions** (`/mcp`). Each session's gateway — including the lease fence it is
  holding for the agent — lives in the process that answered `initialize`. A request
  carrying `Mcp-Session-Id` that reaches another replica is answered 404 ("unknown or
  expired MCP session; re-initialize"), and the agent starts a new session without the
  fence it had. Route `/mcp` with session affinity: hash on the `Mcp-Session-Id`
  request header where the load balancer supports it, or use cookie or client-IP stickiness.
  A replica restart ends its sessions either way. The stdio gateway (`conductor-mcp`) has
  no such state and needs nothing.
- **The authentication failure limiter**, which counts per process by design: N replicas
  allow N times the failures before throttling a client.
- **Event-stream feeds and caps**, which are per process; the database load is one poll per
  watched project per replica.

The GitHub App's credentials are stored in the database (the `github_app` table) so every
replica can act as the app, with its private key, webhook secret and client secret sealed
under the server's secret key (next section). Replicas must therefore share that key.
`CONDUCTOR_GITHUB_APP_ID`, `CONDUCTOR_GITHUB_APP_PRIVATE_KEY` (or `_FILE`) and
`CONDUCTOR_GITHUB_WEBHOOK_SECRET` still override the stored values field by field, for a
deployment that injects the key from a secret store. A credentials file written by an older
conductord (`~/.conductor/github-app.json`) is imported (sealed) once, when the database has
no app, and is not read after that; delete it once the import is logged.

## The secret key

Secrets conductord keeps in Postgres — the GitHub App's private key, webhook secret and
client secret, and each notification channel's URL and signing secret — are sealed with AES-256-GCM under a 32-byte key that never goes into the
database. A database dump, backup or restored copy is then useless for acting as the app
without the key as well.

Where the key comes from, first match wins:

1. `CONDUCTOR_SECRET_KEY`: the key itself, base64 (`openssl rand -base64 32`). Use this to
   inject it from a secret manager.
2. `--secret-key-file PATH` or `CONDUCTOR_SECRET_KEY_FILE`.
3. `secret.key` in `CONDUCTOR_STATE_DIR`, else `~/.conductor/secret.key`.

A key file that does not exist is created (0600, in a 0700 directory) the first time
something is sealed, so a server that never sets up the GitHub App or a notification channel
never creates one.

**Every replica must use the same key.** If a replica's key differs, it cannot unseal the
stored app: it logs `the GitHub App's stored credentials cannot be unsealed`, naming the key
the row was sealed with and its own, and serves no app; `/v1/github/status` shows it as not
configured. Worse, if that replica creates the key file itself and someone runs setup
through it, the other replicas lose the app. For more than one replica, generate the key once
and give it to all of them (`CONDUCTOR_SECRET_KEY` from your secret store, or the same file
mounted read-only), before the first setup.

**Back the key up, separately from the database.** Keeping it in the same place as the dumps
defeats the purpose.

**If the key is lost**, the sealed credentials cannot be recovered, and nothing else is
affected: tasks, leases, tokens and everything else are not sealed. Start conductord with a
new key, then run `conductor github setup --replace` to create and install the app again (the
old app can be deleted in GitHub's settings), and re-add notification channels
(`conductor notify remove` / `add`): until then each send fails with "cannot unseal", shown
as the channel's last error. The same applies when restoring a backup without its key.

**Rotating the key** is the same procedure: switch every replica to the new key, then run
setup again. There is no in-place re-encryption.

Rows stored by a conductord older than this (plaintext secrets) keep working: the first
server with a key that reads one seals it in place.

## Backup and restore

Everything conductord knows is in Postgres, except the secret key that seals the GitHub
App's secrets (above), which must be backed up separately. What is *not* in Postgres
either: agents' worktrees (on
each runner's disk), session resume records and checkpoints (`conductor backup`, on each
user's machine), and CLI logins. Back those up where they live.

**Logical backups** with `scripts/pg-backup.sh` (pg_dump's custom format; conductord can
keep running — a dump is a consistent snapshot):

```bash
export DATABASE_URL=postgres://conductor:...@db:5432/conductor?sslmode=require
scripts/pg-backup.sh backup /backups/conductor-$(date -u +%F).dump
scripts/pg-backup.sh verify /backups/conductor-2026-10-05.dump
```

Run it from cron or a systemd timer at least daily, keep several generations off the
database host, and use a `pg_dump` of the same or a newer major version than the server.

**Restore** into an empty database, then point conductord at it:

```bash
createdb conductor_restored
DATABASE_URL=postgres://.../conductor_restored scripts/pg-backup.sh restore /backups/conductor-2026-10-05.dump
conductord --dsn postgres://.../conductor_restored
```

The script refuses a target that already has a Conductor schema, and restores in one
transaction, so a failed restore leaves the target empty. On first start against the
restored database, outage recovery extends the leases that were open at backup time, giving
their workers a chance to reconnect; work that finished after the backup was taken must be
re-reported or redone.

**Point-in-time recovery.** For a team deployment, a daily dump bounds loss to a day.
For less, run Postgres with continuous WAL archiving (RDS and most managed Postgres do this
with automated backups; self-hosted, `archive_mode` plus pgBackRest or WAL-G) and restore to
a timestamp. conductord needs nothing special for either.

**Drill it.** A backup that has never been restored is a hope. Periodically restore the
latest dump into a scratch database, start a conductord against it with `--no-scheduler`
on a spare port, and check `/v1/ready`.
