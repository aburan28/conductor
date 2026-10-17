# Conductor on one EC2 instance

This is the separate demo launcher. For the private SSM, Tailscale, and backup
deployment, follow [README.md](README.md).

`launch.py` starts a single Ubuntu 24.04 instance that runs Postgres (loopback only) and a
build of Conductor from a pinned commit, both under systemd. Conductor serves HTTPS on
port 8443 with a self-signed certificate, in `enhanced` security mode (tokens only, no local
sign-in). There is no SSH key and no SSM role: progress and the first admin token come back
through the instance's serial console.

## Status

Run against AWS once (us-east-1, `t3.small`, Ubuntu 24.04, amd64): the instance built Conductor
from source, `conductord` came up under systemd, and the serial console reported
`CONDUCTOR_READY health=200` about 12 minutes after launch. That run found two bugs, both
fixed here: `go build` needs `HOME`/`GOPATH`/`GOCACHE` under cloud-init, and Go's download
output filled the console's 64 KB buffer and hid the progress markers.

Verified from the console only: with the port closed, the health check from outside was not
possible. Not exercised: `arm64`, `allow`, and a sign-in with the minted token from a client.

Also checked offline: Conductor steps against Postgres 16; `userdata.sh` / `launch.py` compile
and render within the 16 KB user-data limit; fake EC2/SSM control-flow for `plan` / `launch`.

## Use

```sh
pip install boto3
# credentials from any boto3 source, e.g. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY
python3 deploy/aws/launch.py plan   --region us-east-1 --allow-my-ip   # preview: creates nothing
python3 deploy/aws/launch.py launch --region us-east-1 --allow-my-ip
python3 deploy/aws/launch.py launch --region us-east-1 --allow-cidr 203.0.113.9/32   # or name it yourself
python3 deploy/aws/launch.py allow  --allow-my-ip                 # open the port later
python3 deploy/aws/launch.py status
python3 deploy/aws/launch.py teardown                             # removes everything it made
```

`plan` takes the same flags as `launch` and creates nothing. It resolves the AMI, the VPC and
subnet, asks EC2 to validate the instance request with `DryRun` (it does not dry-run the
security group, which has no such call here), and prints the instance type, the address the
port would be opened to, the commit and the user-data size. Run it first, read it, then run
`launch` with the same flags.

Without `--allow-cidr` or `--allow-my-ip` the port stays closed. `0.0.0.0/0` is refused unless
you also pass `--allow-world`.

`--allow-my-ip` looks up the public IPv4 address of **the machine running the script** (it asks
api.ipify.org, then ipv4.icanhazip.com, then checkip.amazonaws.com), prints it, and opens the
port to that one `/32`. So run it from the machine you will browse from. If you run it somewhere
else (a CI runner, a cloud sandbox, a jump host) that machine's address is the one that gets
opened, not yours, and such hosts often send traffic from a changing pool of addresses. Behind a
shared NAT, VPN or office network the address is the shared one, so everyone behind it can reach
the port (the admin token is still required). An IPv6-only answer is not usable, because the
security group is IPv4; the script then asks you to pass `--allow-cidr`. `--commit` chooses what
to build (a full 40-character sha; the default is the `main` this was written against). The
first admin token (valid 24 hours) is written to `conductor-admin-token.txt` with mode 0600.
The bootstrap takes about five to eight minutes, because the instance builds Conductor from
source.

## What it creates

Everything is tagged `conductor-aws-demo=<name>`:

- one security group, with TCP 8443 open to `--allow-cidr` / `--allow-my-ip` only;
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
