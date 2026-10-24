# AWS high availability

This stack runs at least two on-demand Fargate tasks in separate Availability Zones behind
an HTTPS Application Load Balancer, internal by default with an explicit public option.
A private RDS PostgreSQL Multi-AZ instance
provides automatic database failover, encrypted storage, 14-day point-in-time recovery,
storage autoscaling and deletion protection. ECS replaces failed tasks and rolls back failed
rolling deployments. Existing `deploy/aws/stack.yaml` remains the single-host deployment.

Creating this stack starts a **new, empty database**. It does not migrate the current service,
move its sealing key, change client credentials, or switch DNS. A deployed stack is not an
accepted migration until data, encryption, identity and the failure drills below are verified.

## Required inputs

- One VPC and two private subnets in different AZs. Both must have enough free IP addresses
  for the ALB and overlapping old/new task deployments. The RDS subnet group uses the same
  two subnets. CloudFormation rejects duplicate IDs; preflight must also verify distinct AZs.
- Independent outbound connectivity in each AZ: normally one NAT gateway per AZ. If using
  VPC endpoints, provide ECR API, ECR Docker, Secrets Manager and CloudWatch Logs interface
  endpoints plus the S3 gateway endpoint, and separately provide outbound HTTPS for GitHub,
  model providers and other integrations. A shared NAT gateway remains a failure dependency.
- For default internal mode, private/VPN connectivity from clients to the ALB and a bounded
  `ClientCidr`. For explicit public mode, two public ALB subnets with active internet-gateway
  routes; see below. ECS tasks and RDS remain private in either mode. No SSH port is opened.
- An issued ACM certificate in the same region covering `PublicUrl`, and control of its DNS.
  The stack outputs the ALB DNS name and hosted zone ID; DNS cutover is a separate action.
- An immutable ECR `ImageUri` ending in `@sha256:<64 lowercase hex digits>`, built for
  Linux/amd64 using the Dockerfile in this directory. The image must contain the RDS CA
  bundle and the `/v1/live` and `/v1/readyz` probes.
- An existing Secrets Manager `SealingKeySecretArn` containing **only** the base64-encoded
  32-byte sealing key. Every replica gets the same value. For a migration it must be the
  current deployment's key. Do not replace it with a fresh key or paste it into parameters,
  shell history, source control, or chat. If the secret uses a customer-managed KMS key,
  provide `SealingKeyKmsArn` and permit the execution role in that key's policy.
- An existing SNS `AlarmTopicArn` in this region with confirmed operator subscriptions and
  permissions for CloudWatch and RDS event delivery. Test delivery before cutover.

The database password is generated in Secrets Manager and injected into both RDS and ECS;
it is never a stack parameter or output. The image uses pgx's `PGHOST`, `PGPORT`, `PGUSER`,
`PGPASSWORD`, `PGDATABASE`, `PGSSLMODE=verify-full` and `PGSSLROOTCERT` environment variables.
`DATABASE_URL=connect_timeout=5` supplies the bounded connection timeout without embedding
credentials. The database requires TLS independently through `rds.force_ssl`.

## Optional network stack

When the account has only default public subnets, `network.yaml` creates a separate VPC
with DNS support, two public NAT subnets and two private service subnets across distinct
AZs. Each private subnet routes through its own same-AZ NAT gateway. It does not modify
existing networks. The default CIDR is `10.80.0.0/16`; select a non-overlapping private
`/16` after checking every VPC, VPN and connected network. Subnet CIDRs are derived as four
non-overlapping `/24` ranges.

This creates **two continuously billed NAT gateways and two public IPv4 addresses**, plus
NAT data-processing/transfer charges. The service stack separately incurs ALB, Fargate,
Multi-AZ RDS, storage, monitoring and backup charges. Review current regional costs before
execution. Creating a change set itself does not create these resources.

Select two available standard AZs in your target region and prepare the network change set:

```sh
aws ec2 describe-availability-zones --region us-west-2 \
  --filters Name=state,Values=available Name=zone-type,Values=availability-zone \
  --query 'AvailabilityZones[].ZoneName'
cfn-lint deploy/aws/ha/network.yaml
aws cloudformation create-change-set --region us-west-2 \
  --stack-name conductor-ha-network --change-set-name initial-network \
  --change-set-type CREATE --template-body file://deploy/aws/ha/network.yaml \
  --parameters ParameterKey=AvailabilityZoneA,ParameterValue=us-west-2a \
               ParameterKey=AvailabilityZoneB,ParameterValue=us-west-2b \
               ParameterKey=VpcCidr,ParameterValue=10.80.0.0/16
aws cloudformation describe-change-set --region us-west-2 \
  --stack-name conductor-ha-network --change-set-name initial-network
```

After review and explicit execution, wait for network completion. Copy its `VpcId`,
`PrivateSubnetA` and `PrivateSubnetB` outputs into the service `parameters.json`, then run
the service preflight. Use the private outputs for `PrivateSubnetA/B`. Use the public outputs only for the
optional `PublicSubnetA/B` parameters, never for ECS or RDS.

**Internal mode needs private client access.** NAT provides outbound connectivity only;
it does not expose the internal ALB to this Mac or other clients. Connect the VPC using an
existing private route, VPN, or a separately configured highly available client-access
solution. Configure its routing, DNS and security rules, and use the actual client source
CIDR as `ClientCidr`. This network stack does not create a VPN, client authorization rules,
private DNS zone, ACM certificate or DNS record. Verify clients can reach both AZs before
cutover; a single access gateway can remain a failure dependency outside this stack.

## Public HTTPS with authenticated application access

The default `LoadBalancerScheme=internal` remains private. To publish a service such as
`https://conductor.engineering`, explicitly add these parameters to the service parameter
array (keep `PrivateSubnetA/B` set to the private service subnets):

```json
{"ParameterKey":"LoadBalancerScheme","ParameterValue":"internet-facing"},
{"ParameterKey":"PublicSubnetA","ParameterValue":"subnet-REPLACE-PUBLIC-A"},
{"ParameterKey":"PublicSubnetB","ParameterValue":"subnet-REPLACE-PUBLIC-B"}
```

Set `PublicUrl` to the HTTPS origin matching the issued certificate and set `ClientCidr`
to the intended IPv4 audience. `0.0.0.0/0` is accepted only with explicit `internet-facing`
mode. Keep a narrower CIDR when only known client networks need access. The public subnets
must belong to the same VPC, span the same two AZs as the private subnets, and each have an
active default route to an internet gateway attached to this VPC. Use the network stack's
public subnet outputs; an outbound NAT route is insufficient for a public ALB.

Only the ALB becomes public. Its security group accepts TCP 443 from `ClientCidr`; no HTTP
port 80 listener is created. ECS tasks still receive no public IP, accept application
traffic only from the ALB security group, and connect to private RDS over verified TLS.
Public HTTPS responses include `Strict-Transport-Security: max-age=31536000` at the ALB;
the policy deliberately does not include subdomains or request browser preload enrollment.
Switching an existing ALB's scheme requires replacement: inspect the change set and plan
DNS cutover rather than treating this as an in-place networking change.

Conductor runs with forced `--security-mode enhanced`: local passwordless sign-in is
disabled, and project/task/session/secret APIs require Conductor authentication and their
normal authorization checks. The ALB does not add a separate login layer or configure SSO.
The dashboard's static/login pages and public protocol surfaces remain reachable without a
bearer token: `/v1/live` and `/v1/readyz` return minimal status, while legacy `/v1/health`
also exposes the service version and database status. Local/SSO login metadata and configured
SSO/GitHub callbacks remain reachable and apply their own validation. Detailed `/v1/ready`
requires authentication; `/metrics` is denied to remote callers by default with this proxy
configuration unless a dedicated metrics token is configured.

After deployment, point the domain's DNS alias at the ALB, verify the trusted certificate,
and confirm that an unauthenticated request to `/v1/projects` returns 401 while an authorized
client can access its project. Use a fresh private bootstrap credential; never embed a token
in DNS, URLs, CloudFormation parameters, build logs or dashboard source. In internal mode,
leave `PublicSubnetA/B` empty; the preflight rejects unused public subnet settings to prevent
ambiguous deployment intent.

## Prepare and inspect

From the repository root, authenticate Docker to the intended ECR repository and build
explicitly for Linux/amd64, including when the build host is Apple Silicon:

```sh
docker buildx build --platform linux/amd64 -f deploy/aws/ha/Dockerfile \
  --tag ACCOUNT.dkr.ecr.REGION.amazonaws.com/conductor:ha-COMMIT --push .
```

Resolve the resulting ECR digest. Use that digest, never the mutable tag, in `parameters.json`:

```json
[
  {"ParameterKey":"VpcId","ParameterValue":"vpc-REPLACE"},
  {"ParameterKey":"PrivateSubnetA","ParameterValue":"subnet-REPLACE-A"},
  {"ParameterKey":"PrivateSubnetB","ParameterValue":"subnet-REPLACE-B"},
  {"ParameterKey":"ImageUri","ParameterValue":"ACCOUNT.dkr.ecr.REGION.amazonaws.com/conductor@sha256:REPLACE"},
  {"ParameterKey":"CertificateArn","ParameterValue":"arn:aws:acm:REGION:ACCOUNT:certificate/REPLACE"},
  {"ParameterKey":"PublicUrl","ParameterValue":"https://conductor.example.com"},
  {"ParameterKey":"ClientCidr","ParameterValue":"10.0.0.0/8"},
  {"ParameterKey":"SealingKeySecretArn","ParameterValue":"arn:aws:secretsmanager:REGION:ACCOUNT:secret:REPLACE"},
  {"ParameterKey":"AlarmTopicArn","ParameterValue":"arn:aws:sns:REGION:ACCOUNT:REPLACE"}
]
```

Keep only resource identifiers in this file. Pin the AWS account, region and profile for
all commands. Run the read-only preflight before preparing a change set (Python 3 with `boto3` installed):

```sh
python3 deploy/aws/ha/preflight.py --region us-west-2 --parameters parameters.json
```

It checks resource metadata and service availability without retrieving secrets or changing
AWS resources. Use your intended region; warnings about real network reachability still
require a private-network smoke test. The following creates a reviewable change set without
executing it:

```sh
cfn-lint deploy/aws/ha/stack.yaml
aws cloudformation validate-template --template-body file://deploy/aws/ha/stack.yaml
aws cloudformation create-change-set \
  --stack-name conductor-ha --change-set-name initial-ha --change-set-type CREATE \
  --template-body file://deploy/aws/ha/stack.yaml \
  --parameters file://parameters.json --capabilities CAPABILITY_IAM
aws cloudformation describe-change-set --stack-name conductor-ha --change-set-name initial-ha
```

Review the account, region, monthly resource cost, IAM roles, network boundaries, database
retention and all replacement actions before executing the change set. An existing stack
requires `--change-set-type UPDATE`. Database replacement must receive separate migration
review; the snapshot policy is recovery protection, not transparent data migration.

After execution, wait for `CREATE_COMPLETE`/`UPDATE_COMPLETE`, inspect ECS service events,
verify at least two healthy targets **in distinct AZs**, then check `/v1/readyz` and the
authenticated `/v1/ready` through the intended HTTPS name. A task reaching `RUNNING` alone
is insufficient. Bootstrap a fresh install explicitly via a one-off task using `bootstrap
--no-login` and the inherited database environment; its printed token is a secret and must
not be routed into shared CloudWatch logs. For an existing deployment, preserve the current
identities and tokens by restoring its database using the controlled migration procedure.

## Migration and cutover

1. Record the source version, database schema, row counts, active tasks/leases and endpoint.
   Verify the destination image includes every migration already applied at the source.
2. Restore a rehearsal copy into an isolated destination first, using a separately authorized
   database export and existing sealing key. Run read-only API checks and verify that sealed
   credentials decrypt. Keep background integrations disabled during rehearsal so copies do
   not dispatch work or send notifications.
3. Arrange a write pause for the final consistent transfer. Stop source API writes and its
   schedulers before the final export. Restore and compare the same core tables and counts;
   do not let both databases accept production writes.
4. Start the destination tasks, verify both replicas and readiness, then perform DNS/client
   cutover. Keep the old service stopped and its data intact during the rollback window.
5. Run the drills below with a dedicated validation project and preserve timestamped results.
   Rollback after destination writes requires data reconciliation; pointing DNS back alone
   would discard those writes. Define that boundary before cutover.

This template intentionally does not execute these steps or transfer existing secrets.

## Failure behavior and acceptance drills

The `HA deployment` GitHub Actions workflow lints the template, runs the mocked preflight
tests, builds the Linux/amd64 image, and runs `bash deploy/aws/ha/smoke.sh IMAGE`.
The smoke test creates an isolated Docker network and disposable PostgreSQL container,
starts two replicas concurrently with read-only root filesystems, restarts PostgreSQL,
and stops one replica. It requires readiness recovery without daemon restarts and readiness
of the surviving replica. Run it locally with the same command when Docker is available.
It tests connection recovery; it does not substitute for RDS promotion, TLS, ALB or AZ drills.

The ALB and ECS use **`/v1/live`**, which tests process availability and shutdown state without
querying PostgreSQL. ECS also replaces tasks after ALB health failures; using a database
probe there would restart every replica during a shared database failover. `/v1/readyz`
checks database availability separately and returns a sanitized 503 when it is unavailable.
Monitor it through the intended private or public HTTPS endpoint in addition to the
authenticated detailed `/v1/ready`.

A PostgreSQL failover interrupts in-flight transactions and can cause temporary API errors.
This deployment provides automatic recovery, not uninterrupted transactions or a fixed RTO.
Clients should reconnect and retry safe requests, and reconcile ambiguous mutation outcomes
instead of blindly replaying them. Existing scheduler outage recovery preserves open leases
when all scheduler replicas have stopped making progress; measure that behavior in drills.

- **Replica failure:** create a validation task/session, stop one ECS task, and continuously
  read it through the HTTPS endpoint. Record failed requests and recovery time, confirm ECS
  restores the configured replica count in distinct AZs, and verify task and lease state.
- **Database failover:** with explicit authorization for the interruption, use
  `aws rds reboot-db-instance --db-instance-identifier DB_ID --force-failover`. Record
  `/v1/live`, `/v1/readyz`, request results and ECS task IDs before/during/after. Expect
  temporary readiness failures, unchanged running task IDs, automatic database reconnection,
  preserved committed state and renewed worker leases. Confirm RDS events reach SNS.
- **Rolling update:** deploy a known-good new image digest. Confirm at least the desired
  number of healthy targets while replacement tasks start and old tasks drain. Test a bad
  image in a staging stack and verify the ECS deployment circuit breaker rolls back.
- **Restore:** periodically restore an RDS point-in-time copy into an isolated instance,
  run a compatible image against it without schedulers or integrations, and verify counts,
  schema and decryption using the separately retained sealing key. Never use a production
  hostname for the restore drill.
- **AZ failure:** evaluate subnet routes, NAT, VPC endpoints and private client connectivity
  as well as task and database placement. A two-AZ application cannot compensate for a
  single-AZ VPN gateway or shared network appliance outside this template.

RDS failover events, missing healthy replicas, ALB/backend 5xx and low database storage
alert the supplied SNS topic. Dependency-only failures without traffic need a
readiness monitor on the selected endpoint; the 5xx alarms cannot detect an idle outage by themselves.

## Runtime image

The runtime uses `scratch` with two statically linked Go binaries and public CA bundles.
It contains no shell, package manager, curl, or curl's native compression libraries.
The ECS health command is `/usr/local/bin/conductor-healthcheck`, which probes the local
liveness endpoint with a four-second timeout, without proxies or redirects. The deployment
smoke test exercises this exact binary before and during database loss.

Build tools and certificate-download dependencies remain in build stages. Go builds use the
build host architecture and cross-compile to the requested target, including Linux/amd64
from an ARM Mac. Both static binaries still require the normal Go dependency vulnerability
checks; an OS-package scanner that cannot scan `scratch` is not a clean vulnerability scan.
Use the image manifest and runtime file inventory to verify that excluded libraries are absent.

## Sessions, upgrades and secrets

REST state is in PostgreSQL. MCP HTTP session state remains local to each daemon. ALB cookie
stickiness is enabled for clients that retain cookies; it cannot provide affinity for a
client without a cookie jar. Use stdio MCP clients against the shared REST endpoint, or
verify HTTP clients retain affinity cookies and reinitialize their MCP session after a
replica loss. Existing HTTP MCP sessions do not transparently migrate across replicas.

ECS gives the process 60 seconds to stop, longer than conductord's default 25-second drain;
ALB deregistration waits 30 seconds. Long-lived event streams reconnect. Forward-only schema
migrations mean automatic image rollback is safe only when the previous image supports the
new schema; test compatibility before any schema-changing rolling deployment.

The generated database secret does **not** enable automatic password rotation: ECS injects
secrets only at task start, so a password changed without replacing tasks eventually breaks
new connections. Coordinate password changes in RDS and Secrets Manager with a forced ECS
service deployment, verify every new replica, and retain a tested rollback path. Do not
assume editing a secret alone updates RDS or live containers. Automatic rotation requires
a separately tested rotation/redeployment workflow. The deployment uses the database owner
because conductord applies migrations on startup; restrict access to its execution role.

Changing the sealing key requires Conductor's documented resealing/reconfiguration procedure;
it is not ordinary database password rotation. Back it up separately from database snapshots.
Database deletion protection must be deliberately disabled before removing the database;
CloudFormation retains a snapshot, the generated credential secret and the log group.

AWS references: [ECS service replacement and health checks](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/update-service-parameters.html),
[Availability Zone rebalancing](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/service-rebalancing.html),
[Secrets Manager dynamic references](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/dynamic-references-secretsmanager.html).
