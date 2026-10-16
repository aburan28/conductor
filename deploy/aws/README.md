# Private AWS control plane

For the separate EC2 demo launcher that builds from source and exposes a
certificate on a selected CIDR, see [DEMO.md](DEMO.md). This runbook describes
the CloudFormation stack used for the private SSM and Tailscale deployment.

This directory deploys one Ubuntu 24.04 EC2 host for Conductor. PostgreSQL 18
and `conductord` run on encrypted EBS. The API binds only to `127.0.0.1:8080`;
Tailscale Serve provides private HTTPS. The EC2 security group has no inbound
rules. SSM manages the host, and a private, versioned S3 bucket stores verified
logical backups. The root EBS volume is retained if the instance is deleted.

The server database holds coordination state. A running Claude or Codex process,
its worktree, and its native transcript stay on the machine running that agent.
`conductor sessions save` and sealed checkpoints preserve their resume material
separately. Moving the control plane does not move or restart those processes.

## Provision

1. Choose an AWS profile, region, VPC, subnet with outbound internet access,
   current Ubuntu 24.04 amd64 AMI, and globally unique bucket name. Confirm the
   account with `aws sts get-caller-identity --profile "$AWS_PROFILE"`.
2. Validate `stack.yaml` with `aws cloudformation validate-template`, then
   create `conductor-private` with `VpcId`, `SubnetId`, `ImageId`, and
   `BackupBucketName` parameters and `CAPABILITY_IAM`. Record the stack outputs,
   especially `InstanceId` and `BackupBucket`.
3. Cross-build `./cmd/conductord` and `./cmd/conductor` with
   `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`. Stage them as
   `conductord-linux-amd64` and `conductor-linux-amd64` beside the scripts and
   systemd units here. Generate `SHA256SUMS` for every staged file and upload
   **only that nonsecret package** to `s3://$BUCKET/bootstrap/`. Check the
   S3 bucket's public access block and encryption before any data transfer.
4. On the host, use SSM `AWS-RunShellScript` to install `unzip` and AWS CLI v2
   using [AWS's Linux instructions](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html).
   Download `prepare-host.sh` from the bucket, verify its pinned SHA-256, then
   run `bash prepare-host.sh "$BUCKET"`. The script verifies the package
   manifest, installs PostgreSQL 18 and Tailscale, creates the `conductor`
   database and systemd units, and leaves the API stopped. Review the SSM
   command output and `systemctl status postgresql`.

The existing Conductor sealing key is required to read sealed database values.
Provisioning does not generate a replacement or export the key. Transfer it
only through a specifically authorized secure channel, compare its SHA-256
with the source, and install it as `/etc/conductor/secret.key` owned by
`conductor:conductor` with mode `0600`. The `conductor.service` condition refuses
to start without it.

Run `sudo /opt/conductor/bootstrap/smoke-restore.sh "$BUCKET"` before importing
real data. It creates two temporary PostgreSQL databases, uploads a synthetic
dump to the private bucket, restores it with `restore.sh`, compares table
counts, then removes the fixture and its exact S3 object version.

## Stage and verify data

Keep the source server authoritative during preparation. Identify its actual
PostgreSQL listener; a Docker development database may use the same port on a
different address family. Use `pg_dump -Fc` from PostgreSQL 18 to make a
consistent staging snapshot. Run `pg_restore --list` and record its SHA-256.
The dump includes sensitive coordination and session metadata, so transferring
it to AWS also requires specific authorization. Never put it in a public
bucket or in the Git repository.

After the authorized transfer to a private S3 object, restore a staging copy:

```sh
sudo -u postgres createdb --owner=conductor conductor_stage
sudo /opt/conductor/bootstrap/restore.sh "$BUCKET" "$STAGING_OBJECT_KEY" "$SHA256" conductor_stage
sudo -u conductor /opt/conductor/bootstrap/table-counts.sh conductor_stage
```

`restore.sh` verifies the SHA-256 and `pg_restore --list`, requires the target
database to be empty and the API to be stopped, and restores in one
transaction. Compare schema and table counts against a source snapshot made
under a write fence. Keep the original dump and checksum until the comparison
and an independent readback are complete.

Authorize the EC2 host in the intended tailnet with
`sudo tailscale up --hostname conductor-aws`. After `tailscale status` shows a
DNS name, run `sudo /opt/conductor/bootstrap/configure-tailnet.sh`. It writes
`CONDUCTOR_PUBLIC_URL` and configures **Tailscale Serve**, not Funnel, for
private HTTPS to the loopback API. Tailscale may require a browser sign-in or
tailnet HTTPS consent. Verify the URL and tailnet access controls before
changing any clients.

## Final cutover

1. Record the active sessions and client endpoints. Give running agents a
   short API retry window; do not terminate their processes or worktrees.
2. Fence source writes by stopping only the source `conductord` service. Keep
   PostgreSQL and the local agent processes running. Record exact per-table
   counts with `table-counts.sh`, then take and validate a **new** final dump.
   The staging dump is not a cutover dump.
3. Transfer the final dump only after the sensitive export is authorized.
   Restore it into the empty `conductor` database on EC2 with `restore.sh` and
   compare the row counts. Confirm the existing sealing key's hash on EC2.
4. Enable and start both units with `systemctl enable --now conductor.service
   conductor-backup.timer`. Check `/v1/health`
   through the Tailscale HTTPS URL, authenticate with an existing token, read
   tasks and session state, and run one backup followed by S3 checksum
   readback. Repoint every runner and CLI to the new URL and verify heartbeats
   and checkpoint resume from both harnesses. Only then retire the local API.

If any comparison or health check fails, stop the EC2 API and restart the
source API without changing its database. Keep the source database, sealing
key, native transcripts, and checkpoint files until the new endpoint and
backups have been exercised. A logical dump plus a brief write fence gives a
bounded control-plane interruption; this design does not claim seamless
zero-downtime writes or automatic relocation of native agent processes.

## Operations

- `conductor-backup.timer` runs a verified `pg_dump` to the versioned bucket
  once daily. A successful object readback checks the SHA-256 and archive
  listing. There is no continuous WAL archive, so changes since the latest
  backup can be lost after an unrecoverable volume failure.
- CloudWatch requests EC2 system recovery after two failed system checks.
  That is host recovery, not database replication. Monitor instance status,
  EBS free space, timer results, and the age of the newest verified S3 dump.
- IAM permissions for the instance are limited to SSM and its backup bucket.
  Keep Tailscale Serve private and Conductor in enhanced security mode;
  bearer-token authentication remains required even inside the tailnet.
