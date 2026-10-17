# Isolated deployments and team collaboration

Conductor can run inside a private network with self-hosted PostgreSQL, local
credentials, embedded dashboard assets, and internal model endpoints. WireGuard
can provide encrypted connectivity across permitted network paths. It cannot
cross a physical air gap. Each disconnected installation has its own control
plane, database, identities, task state, and audit history.

This document describes deployment using existing capabilities. It does not
claim an enforced application-wide egress policy or a complete WireGuard control
plane. Enforce network isolation with the environment's firewall and routing.

## Prepare outside the isolated environment

Transfer reviewed release binaries for the destination OS/architecture, their
checksums, PostgreSQL 16+ packages (or a preloaded supported database image),
WireGuard tooling, internal CA material, and any coding tools/model weights the
team needs. The runtime does not require a Go toolchain. Source builds require
the version in `go.mod`, the module dependency cache or an internal Go proxy, and
the build tools. Release installers, automatic toolchain downloads, container
image pulls, model downloads, and vulnerability database refreshes require an
external connection unless mirrored beforehand.

Keep private keys on their owning machines. Transfer secret references and
operator-provisioned credentials through the organization's approved mechanism.
There is no need to send machine private keys to a shared coordination server.

## Connectivity and bootstrap

Use the [offline WireGuard renderer](WIREGUARD.md) to prepare gateway and worker
templates from public inventory, then provision and activate WireGuard with your
network administrator before starting Conductor. A simple deployment
uses one internal gateway with a known reachable endpoint and routes connecting
worker tunnel addresses to the leader. The gateway need not be internet-facing.
Permit only the intended network paths and application ports; PostgreSQL should
remain on loopback or an explicitly authorized database network.

When a gateway forwards traffic between separate WireGuard tunnels, traffic is
decrypted and re-encrypted at that gateway. Use application TLS between workers
and Conductor; use mesh mutual TLS for daemon links. Network access does not
replace project membership, tokens, or application authorization.

Example leader configuration (replace addresses and paths):

```yaml
version: 1
server:
  addr: 10.88.0.1:8443
  public_url: https://leader.internal:8443
  tls_cert: /etc/conductor/server.pem
  tls_key: /etc/conductor/server.key
  security_mode: enhanced
database:
  mode: external
  url_file: /etc/conductor/database.url
nat:
  mode: off
notifications:
  poll: -1s
model_discovery:
  interval: -1s
delphi:
  interval: -1s
```

`public_url` is the address advertised to participants; it may be private. Supply
an existing database via an owner-readable secret file. Start with
`conductor leader --config leader.yaml --bootstrap --project myrepo`.
Install the internal CA in each client's trust store and make `leader.internal`
resolve through internal DNS or hosts entries. Do not disable certificate
verification to make a private certificate work.

The negative intervals disable the listed background integrations; they do not
block every possible outbound request. Omit public GitHub, identity, notification,
and model-provider configuration. The GitHub poller can be disabled with
`conductord --github-poll=-1s`; run the configured `conductord` directly with
that flag after bootstrap when disabling the poller.
Use configured internal inference endpoints and local coding tools for actual
execution. On-demand actions and external harnesses need their own destination
controls. See [operations](OPERATIONS.md) for integration configuration.

For an internal service directory, use the explicit `conductor-mesh-dns` example
in [leader deployment](LEADER.md#wireguard-and-isolated-private-networks).
`--hostname` bypasses Tailscale detection; `--address` publishes its address;
`--mesh-target` enables remote mesh targets. This is a static single-host
directory with operator-defined ports. The daemon's custom DNS option discovers
SRV records only; operating-system DNS must also resolve the advertised targets.
Bind the directory to a private interface and permit queries only from intended
participants. The directory is not an authorization service or recursive resolver.

## Team identity and work ownership

Use one shared leader for a team that needs shared task ownership. Invite members
with `conductor invite teammate`, then join using the invitation link and connect
workers to the private HTTPS URL. Conductor already provides organizations,
projects, memberships, task claims, scoped reservations, leases, handoffs, and
audit records. Its privacy boundary shares work intent and territory rather than
automatically publishing teammates' conversations.

Enhanced mode requires credentials even on the private network. An internal
identity provider can use existing SSO configuration when its discovery, login,
token, and key endpoints are reachable inside the installation. Otherwise use
locally issued credentials. Test the selected identity provider and revocation
behavior in the actual isolated environment; the DNS addition does not introduce
SSO, MFA, directory synchronization, or identity-provider availability guarantees.

Daemon mesh links establish certificate identity and report reachability. They
do not federate organizations or replicate tasks between independent databases.
Workers collaborating on one project must use the same control plane (or its
existing shared-database replicas). There is no live synchronization across a
physical air gap and no new disconnected workspace-merge format in this change.

## Operations and verification

Keep backups of the database and separately protected encryption keys. Preserve
runner-local session/checkpoint material as required by your recovery objectives.
Follow the logical backup and restore procedure in [operations](OPERATIONS.md),
including a restore drill before an upgrade. See [storage](STORAGE.md) before
enabling optional S3 storage; use an approved internal endpoint where required.

Validate a deployment with outbound internet access blocked:

1. Start from the transferred binaries and preinstalled database without pulling
   images or downloading tools. Open the embedded dashboard.
2. Bootstrap the leader, enroll two team members, and verify that a nonmember
   cannot access the project. Exercise token revocation.
3. Resolve SRV and A/AAAA records using internal DNS; verify that TLS succeeds
   with the internal CA and fails for an untrusted certificate.
4. Have members claim overlapping scopes and confirm a conflict. Run a task using
   a local model/tool, then test handoff and reconnection after a worker stops.
5. If using daemon mesh, verify a valid mesh certificate connects and an unrelated
   CA fails. Confirm the links do not imply cross-installation task visibility.
6. Restore a backup into an isolated test installation and inspect task/audit
   history. Check firewall/DNS logs for unexpected outbound attempts.

Directory tests cover explicit configuration and real UDP DNS lookups.
[Packet validation](WIREGUARD_VALIDATION.md) separately establishes bounded
userspace WireGuard forwarding and manual gateway peer withdrawal on an isolated
Linux test network. It does not establish kernel-module compatibility, customer
NAT/firewall policy, an organization's identity provider, or customer-specific
offline tooling.

## Remaining network control-plane work

The static WireGuard renderer prepares templates without collecting private keys
or changing the host. Key enrollment and rotation, address allocation, peer
configuration distribution, automatic gateway selection/failover, NAT traversal, relay fallback,
and exit/subnet-route authorization are not implemented here. Network operators
must supply them, or integrate an existing self-hosted WireGuard management
system. The explicit DNS path removes a Tailscale dependency from discovery while
retaining Conductor's existing application security and collaboration model.
