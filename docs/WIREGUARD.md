# Static WireGuard gateway plans

`conductor wireguard render` turns a reviewed public inventory into configuration
templates for one gateway and its worker nodes. It is an offline operation: it
does not contact Conductor, invoke Tailscale, generate private keys, or modify
network interfaces, routes, or firewall rules.

The gateway gives workers a known endpoint so a first deployment can avoid a
dynamic peer-discovery/NAT-traversal system. Workers initiate tunnels and use
25-second persistent keepalives. The endpoint must already be reachable over the
underlying network; WireGuard cannot create a path through an inaccessible
firewall or cross a physical air gap.

## Prepare the inventory

Generate keys with WireGuard tooling on each node. Keep the private key on that
node and transfer only its public key into this manifest:

```yaml
version: 1
network:
  cidr: 10.88.0.0/24
gateway:
  name: gateway
  address: 10.88.0.1
  public_key: REPLACE_WITH_GATEWAY_PUBLIC_KEY
  endpoint: gateway.internal:51820
  # listen_port: 51820  # optional; defaults to endpoint port
nodes:
  - name: worker-a
    address: 10.88.0.2
    public_key: REPLACE_WITH_WORKER_A_PUBLIC_KEY
  - name: worker-b
    address: 10.88.0.3
    public_key: REPLACE_WITH_WORKER_B_PUBLIC_KEY
```

Replace all public-key placeholders with canonical base64 WireGuard public keys
(32 bytes). The gateway endpoint names its reachable underlay address, rather
than an address that only becomes reachable after starting the tunnel. Its DNS
must work before tunnel activation. A literal IPv4 address or bracketed IPv6
address is also accepted; the overlay itself is IPv4 in this version.
Literal endpoints inside the overlay are rejected to prevent recursive routing;
ensure hostname-based endpoints resolve outside the overlay as well.

The manifest is a strict single YAML document. Unknown fields, including private
keys, script hooks, and exit-route settings, are rejected. The overlay must be a
canonical RFC1918 subnet with usable host addresses. Names, public keys, and
addresses must be unique; names are valid interface/file names of at most 15
lowercase letters, digits, or hyphens, starting with a letter. Network and
broadcast addresses cannot be assigned. Ensure the chosen overlay does not
overlap existing LAN, container, VPN, or infrastructure routes on any node.

## Render and deploy

```bash
conductor wireguard render --manifest mesh.yaml --out ./wireguard-plan
```

The output directory must be new. The command refuses existing paths and
symlinks so a rerun cannot overwrite a locally filled private configuration.
It creates the directory with owner-only permissions and writes owner-only
`gateway.conf`, `worker-a.conf`, `worker-b.conf`, and `README.md` files.

Configuration files contain an intentionally invalid local private-key
placeholder. Transfer only each node's own file to that node, replace the
placeholder locally, and keep its permissions restricted. Confirm that the
private key's public key matches the public inventory. The renderer provides
no private-key custody or distribution service.

Have the network administrator review the generated instructions, allow the
gateway's UDP listener, enable IP forwarding, and configure firewall rules for
intended traffic between the assigned overlay addresses. Then activate the
reviewed files with the platform's WireGuard tooling. The command does not
generate or run activation, forwarding, NAT, or firewall hooks.

For a Linux gateway forwarding worker traffic back through the same WireGuard
interface, disable both sending and accepting ICMP redirects after enabling
forwarding. Otherwise Linux may send host redirects to workers and disrupt the
intended hub routes. An administrator-managed example, after `wg0` exists:

```bash
sysctl -w net.ipv4.ip_forward=1
sysctl -w net.ipv4.conf.all.send_redirects=0 net.ipv4.conf.default.send_redirects=0 net.ipv4.conf.wg0.send_redirects=0
sysctl -w net.ipv4.conf.all.accept_redirects=0 net.ipv4.conf.default.accept_redirects=0 net.ipv4.conf.wg0.accept_redirects=0
```

Replace `wg0` with the actual gateway interface (`gateway` when activated from
`gateway.conf` with `wg-quick`). Set the `all` and `default` values before
activating new interfaces, apply the interface values after creation, and
persist these settings through the approved network manager. Keep forwarding
firewall rules limited to intended peers and interfaces. These settings do not
provide project isolation or replace that firewall policy.

Each node receives a `/32` interface address. Gateway peers each have exactly
their worker's `/32` in `AllowedIPs`. Workers route the configured private
overlay through their single gateway peer. No default internet route or subnet
outside that overlay is emitted. Other workers can communicate through the
gateway when its forwarding policy permits it.

The gateway decrypts and re-encrypts traffic between its separate WireGuard
tunnels. Use application TLS or mTLS to keep payloads private from the gateway.
This shared overlay does not enforce project or workspace network isolation.
Use Conductor's authenticated project permissions, and administrator-managed
firewall rules or separate overlays for any additional network boundary.

For Conductor, bind its HTTPS listener to the private address, keep enhanced
authentication enabled, and advertise its internal DNS name. See
[leader deployment](LEADER.md#wireguard-and-isolated-private-networks) for explicit
service discovery, and [isolated deployments](AIR_GAPPED.md) for offline operation.

## Validation and limits

See [packet validation evidence](WIREGUARD_VALIDATION.md) for the preserved failed, partial, and successful userspace routing checks.
The [follow-up coverage matrix](WIREGUARD_TEST_COVERAGE.md) distinguishes normal
Go CI coverage from opt-in gateway lifecycle and native-team-over-overlay tests.

Unit and CLI tests cover strict inventory validation, route generation, local
private-key placeholders, file permissions, and refusal to overwrite existing
configuration. DNS tests separately verify real UDP SRV/A/AAAA lookups without
Tailscale.

Before relying on a deployment, verify actual handshakes, worker-to-leader and
worker-to-worker traffic, permitted forwarding, denied traffic, internal TLS
trust, and behavior when the gateway stops. Packet forwarding and NAT behavior
require tests on real WireGuard interfaces; rendering does not establish them.

Dynamic enrollment, address allocation, key rotation/revocation distribution,
NAT traversal, opaque relay fallback, gateway failover, IPv6 overlays, and
workspace firewall policy generation remain separate network control-plane
work. Revoking a Conductor token does not remove a WireGuard peer; the network
operator must remove its public key/configuration separately.
