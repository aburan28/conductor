# Hosted Conductor storage

Status: proposed paid tier; no customer billing or hosted provisioning is shipped.

## What the paid tier stores

A paid customer would connect its CLI and runners to an operator-hosted Conductor API. The
API would keep the coordination ledger, including task text and events, in a private RDS
PostgreSQL database. The operator would use RDS automated backups and keep any separate
logical exports in private S3. Source checkouts, runner processes, local provider
credentials, and raw agent transcripts from normal coordination stay on the customer's machines.
Conductor's project secret vault is a separate server-side feature: when used, it stores
sealed credential values in PostgreSQL. A hosted plan must disclose that behavior and
keep its sealing key outside RDS, or disable the vault for customers promised local-only
credentials.
Customers use the authenticated API, not direct SQL or S3 access.

Conductor currently supports an existing RDS PostgreSQL endpoint with `--database rds`,
password authentication, `sslmode=verify-full`, and an explicit CA bundle. It does not
provision RDS, refresh RDS IAM database tokens, meter subscriptions, or offer a hosted
customer signup flow. The AWS stack in `deploy/aws/` currently uses PostgreSQL on the EC2
host. The S3 implementation supports session resume records, encrypted checkpoints, and
local PostgreSQL WAL/base backups. The local WAL/base-backup commands do not back up RDS;
an RDS deployment must use RDS automated backups/PITR and a separate tested export path.

An explicit `conductor checkpoint push` can upload an encrypted bundle containing a
conversation and uncommitted worktree. A bucket in a CLI's storage configuration enables
session, checkpoint, and database uses by default. A hosted tier must not distribute
operator bucket credentials to clients or silently configure that bucket for them.
Checkpoint upload to any customer-controlled storage remains a separate, explicit choice.

## Pilot isolation

Use one `conductord` deployment and one RDS database boundary per customer during the
pilot. The simplest strong boundary is a dedicated RDS instance and private S3 bucket per
customer, with a distinct database role and Conductor sealing key. Keep the RDS endpoint
private to the service's VPC; runners connect outbound to the API over authenticated
HTTPS. A shared RDS instance with one database per customer is a future cost option, but
its backups, failover, and operator privileges still cross customer boundaries. Do not
advertise dedicated restore or isolation with a shared instance until those paths are
independently tested.

For each pilot customer, provision and record:

1. A private, encrypted RDS PostgreSQL instance, database role, verified CA configuration,
   connection cap, automated-backup retention, and a successful point-in-time restore drill.
2. One Conductor service with a separate secretbox key kept outside RDS, authenticated API,
   per-customer logs/metrics, and an operator-only database credential. Restart or reload
   the service after password rotation; today's pool reads its DSN at startup.
3. An operator-only S3 backup/export location with tenant-scoped IAM, encryption, retention,
   versioning, and a restore test. Customer clients get no operator S3 credentials or
   operator bucket configuration. Check client configuration and environment overrides
   during onboarding so an operator bucket is never selected by a client.
4. An export and deletion procedure that preserves the customer's data through a documented
   grace period. Billing suspension must stop new paid operations without deleting data or
   making the export impossible.

Laptop OIDC signing keys do not belong in RDS, S3, task metadata, or support logs. The
project vault can store sealed model-provider keys in RDS when a customer elects to use
it; that is part of the data inventory and backup/restore test. Provider calls remain
billed through the customer's own account unless a
separate model-usage product is specified and metered.
Paying for hosted storage does not authorize sending customer task material to a free
model provider; model routing retains its separate provider and data-use policy.

## Subscription boundary

The subscription pays for the hosted control plane, database, backups, storage, and
support. It needs a billing account and entitlement ledger outside the customer's
Conductor database. Record plan, period, resource limits, invoice state, and the minimum
identifiers required for reconciliation; do not copy task text or source artifacts into
billing events. Server-side entitlements enforce limits, with an explicit grace state for
payment failures and a tested export path.

Set a price only after measuring a chosen AWS Region and instance class, backup retention,
API and S3 usage, support burden, and high-availability target. RDS charges for instance
time, storage, backups beyond allowances, and some transfer; Multi-AZ changes that cost.
Use the AWS Pricing Calculator and a real pilot bill. A dedicated instance puts a real
per-customer floor under the subscription price.

## Launch gates

- A disposable RDS test proves verified TLS, migrations, API startup, writes, failover,
  and restoration. The read-only live smoke in `internal/db/rds_test.go` can check an
  existing endpoint without modifying it; it cannot establish the write path.
- A backup from every pilot tenant restores to a new deployment with its correct sealing
  key, API identity, and task ledger. Record recovery time and point of recovery.
- Cross-customer API, admin, secrets, S3, logs, metrics, and billing access tests pass.
  Shared multi-tenant deployment additionally needs completed row-level security or
  equivalent service-layer isolation and per-tenant backup/export/erase behavior.
- An explicit data inventory distinguishes ledger fields, S3 backups, optional encrypted
  checkpoints, and data that remains local. Opt-in checkpoint behavior is tested against
  the paid service's IAM and bucket policy.
- Customer terms state retention, backup window, export/deletion process, support access,
  and whether a specific plan includes Multi-AZ or other availability commitments.

References: [RDS TLS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.SSL.html),
[point-in-time restore](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_PIT.html),
[RDS PostgreSQL pricing](https://aws.amazon.com/rds/postgresql/pricing/), and
[AWS Pricing Calculator](https://calculator.aws/#/).
