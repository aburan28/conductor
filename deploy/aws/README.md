# Conductor on one EC2 instance

`launch.py` starts a single Ubuntu 24.04 instance that runs Postgres (loopback only) and a
build of Conductor from a pinned commit, both under systemd. Conductor serves HTTPS on
port 8443 with a self-signed certificate, in `enhanced` security mode (tokens only, no local
sign-in). There is no SSH key and no SSM role: progress and the first admin token come back
through the instance's serial console.

## Status: not yet run against AWS

This was written while the only AWS credentials available were rejected by AWS
(`InvalidClientTokenId`), so no instance has been launched with it.

Checked:
- the Conductor-specific steps ran locally against Postgres 16: a self-signed certificate,
  `conductord` over HTTPS in `enhanced` mode, `conductord bootstrap --no-login
  --token-ttl 24h`, a request with the token (200, `admin`), and without it (401), and no
  login file written;
- `userdata.sh` passes `bash -n`, `launch.py` compiles, and the user-data renders for both
  `amd64` and `arm64` within EC2's 16 KB limit.

Not checked: cloud-init, the systemd unit, the Go build on a `t3.small`, and every EC2 and SSM
call. Expect the first real launch to need a fix or two.

## Use

```sh
pip install boto3
# credentials from any boto3 source, e.g. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
python3 deploy/aws/launch.py launch --region us-east-1 --allow-cidr 203.0.113.9/32
python3 deploy/aws/launch.py allow  --allow-cidr 203.0.113.9/32   # open the port later
python3 deploy/aws/launch.py status
python3 deploy/aws/launch.py teardown                             # removes everything it made
```

Without `--allow-cidr` the port stays closed. `0.0.0.0/0` is refused unless you also pass
`--allow-world`. `--commit` chooses what to build (a full 40-character sha; the default is
the `main` this was written against). The first admin token (valid 24 hours) is written to
`conductor-admin-token.txt` with mode 0600. The bootstrap takes about five to eight minutes,
because the instance builds Conductor from source.

## What it creates

Everything is tagged `conductor-aws-demo=<name>`:

- one security group, with TCP 8443 open to `--allow-cidr` only;
- one instance (`t3.small` by default; an instance type ending in `g` selects `arm64`), a 20 GB
  encrypted gp3 root volume, IMDSv2 required, a public IPv4 address.

Roughly $20 a month if left running. `teardown` terminates the instance and deletes the group.

IAM actions needed: `ec2:RunInstances`, `DescribeInstances`, `DescribeVpcs`,
`DescribeSecurityGroups`, `CreateSecurityGroup`, `AuthorizeSecurityGroupIngress`,
`DeleteSecurityGroup`, `CreateTags`, `TerminateInstances`, `GetConsoleOutput`, and
`ssm:GetParameter` on `/aws/service/canonical/*` (the Ubuntu AMI lookup).

## Things to know

- The admin token is printed once to the console output, which anyone with
  `ec2:GetConsoleOutput` on the instance can read. It expires after 24 hours; replace it
  after your first sign-in.
- The certificate is self-signed (it is at `/etc/conductor/tls.crt` on the instance), so
  clients must trust it or skip verification.
- It builds from source because the only release, `v0.1.0`, predates the storage and database
  work. Go 1.26.9 is installed from go.dev with a pinned SHA-256.
- Postgres here is local to the instance. For durability beyond the machine, configure the
  S3 settings described in `docs/STORAGE.md` after it is up.
