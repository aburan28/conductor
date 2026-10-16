# Leader node, Postgres and network access

`conductor leader` runs the configured control plane in the foreground. The leader
owns the database connection; workers use its authenticated HTTPS API. One configured
leader is sufficient. This command does not elect another leader, replicate a local
Postgres database, or configure database failover.

## Postgres on the leader

From the repository being coordinated:

```bash
conductor leader --bootstrap --project myrepo
```

Local mode is the default. It reuses a running loopback Postgres or starts the existing
`conductor-db` Docker container with its persistent volume. The default database has a
generated password saved in the operator's owner-only state file. Its port stays bound
to loopback. Docker is required only when a managed local database must be started.
`--bootstrap` initializes the project and saves an operator login before the daemon
serves requests; omit it on subsequent launches. Stopping the foreground leader leaves
the database container and its data running.

Use `--dsn` or `DATABASE_URL` for a running local PostgreSQL URL. For native Postgres,
Unix sockets or a database lifecycle managed elsewhere, select external mode:

```bash
conductor leader --database external --config leader.yaml
```

```yaml
version: 1
server:
  addr: 127.0.0.1:8080
database:
  mode: external
  url_file: /etc/conductor/database.url
```

External and RDS modes require an explicit database URL from a CLI flag, `DATABASE_URL`,
or the configuration's `url_env`/`url_file` reference. They never start Docker, including
when the database URL reaches an external database through a loopback tunnel. A saved
local database is not silently substituted. Keep database passwords out of committed
configuration files and process arguments; prefer the environment or an owner-only
secret file. The CLI passes the resolved connection string to the daemon through its
environment.

## AWS RDS PostgreSQL

An existing RDS PostgreSQL instance uses the same database schema and pgx driver. Place
the trusted CA bundle on the leader, and supply a URL such as the following through a
secret file or environment variable, replacing the placeholders:

```text
postgresql://conductor:<url-encoded-password>@<rds-endpoint>:5432/conductor?sslmode=verify-full&sslrootcert=/etc/conductor/rds-ca.pem
```

```yaml
version: 1
server:
  addr: 0.0.0.0:8443
  public_url: https://leader.example.com:8443
  tls_cert: /etc/conductor/server.pem
  tls_key: /etc/conductor/server.key
  security_mode: enhanced
database:
  mode: rds
  url_file: /etc/conductor/database.url
```

```bash
conductor leader --config leader.yaml --bootstrap --project myrepo
```

RDS mode requires verified TLS and an explicitly configured CA trust pool for every
database host and fallback. `sslmode=require`, plaintext, and certificate-chain-only
verification are rejected. Missing or invalid CA files are rejected before connecting.
The database hostname must match its certificate. Standard AWS endpoints, China-region
endpoints and custom DNS names are supported subject to that verification.

The leader needs network access to the RDS endpoint; configure the VPC route, security
group and database user accordingly. The database user needs permission to create and
update Conductor's schema: the daemon applies migrations at startup. Workers do not
need access to RDS. Budget connections across replicas: the existing pool reserves a
maximum-connection floor of eight per daemon. This release supports PostgreSQL password
authentication; it does not refresh expiring RDS IAM authentication tokens or provision
AWS resources.

## UPnP on a home or office router

Use a TLS certificate matching the advertised hostname, point that hostname at the
router's public address, and select the leader's LAN IPv4 address:

```bash
conductor leader --bootstrap --project myrepo \
  --addr 0.0.0.0:8443 \
  --public-url https://leader.example.com:8443 \
  --tls-cert /etc/conductor/server.pem \
  --tls-key /etc/conductor/server.key \
  --nat-mode upnp --nat-internal-ip 192.168.1.20 \
  --nat-external-port 8443 --nat-lease 30m
```

Equivalent file settings are:

```yaml
nat:
  mode: upnp
  internal_ip: 192.168.1.20
  external_port: 8443
  lease: 30m
  timeout: 5s
```

UPnP maps only the authenticated TLS API's TCP port. It renews its mapping and removes
the owned mapping during graceful shutdown; Postgres is never mapped. UPnP requires a
router that supports the protocol and a usable public IPv4 address. Carrier-grade NAT,
another upstream router, firewall rules, DNS and certificate configuration can still
prevent remote access. A successful router mapping does not establish Internet
reachability; verify the advertised URL from another network.

## Tailscale for NATs that cannot forward ports

Install and authenticate Tailscale on the leader, enable tailnet HTTPS, and allow the
workers through the tailnet access policy. The daemon can then supervise a Tailscale
Serve route to its loopback HTTP backend:

```bash
conductor leader --bootstrap --project myrepo \
  --addr 127.0.0.1:8080 \
  --public-url https://leader.your-tailnet.ts.net \
  --nat-mode tailscale --nat-https-port 443
```

Tailscale handles traversal and relay fallback. This mode requires a loopback HTTP
backend and enhanced Conductor authentication; HTTPS is served by Tailscale. Supply
the actual tailnet URL when using `--bootstrap`, because bootstrap runs before network
auto-discovery. Existing configuration on the requested Tailscale HTTPS port is refused
without being replaced, and shutdown removes only the route this daemon created.

## Connect workers

Issue a project invitation on the leader and use its link on each worker:

```bash
conductor invite teammate
conductor join "<invitation-link>"
conductor up --endpoint https://leader.example.com:8443
```

The worker needs the CLI, the reachable endpoint and its own login. Remote `up` neither
starts a local daemon nor uses a saved local database. It can verify an existing login
without a local `conductord` binary. For a private certificate authority, configure
`CONDUCTOR_CA_CERT` with its trusted CA certificate on clients.

Run the foreground command under a service manager for production. SIGINT/SIGTERM to
the supervising CLI forwards SIGTERM to the daemon, giving it time to release mappings
and drain work. Keep the leader's database secret, operator credentials and secret-sealing
key in durable, access-controlled state, and follow `docs/OPERATIONS.md` for backups.

## Validation boundaries

Database-mode, TLS, flag precedence, worker startup and graceful termination tests run
without a database. The complete integration suite requires a writable Postgres instance
through `CI_DATABASE_URL`, native Postgres binaries or Docker (`make ci`). Live RDS,
router UPnP and Tailscale reachability require their respective deployment environments;
unit tests do not establish that those environments are available.
