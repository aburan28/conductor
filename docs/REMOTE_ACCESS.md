# Remote access

How Conductor instances, workspaces, and coding agents connect across machines. Prefer a
private overlay; do not punch holes in home NATs from Conductor itself.

## Choose what needs to be shared

| Goal | Connection | Where tasks live |
|---|---|---|
| Agents and workspaces on several hosts coordinate on the same projects | Point each CLI/MCP client at one Conductor endpoint over Tailscale Serve | The one control plane's PostgreSQL database |
| Several `conductord` processes serve the same projects | Give every replica the same PostgreSQL DSN and server secret key (see [Operations](OPERATIONS.md#running-more-than-one-replica)) | Their shared PostgreSQL database |
| Independent control planes see whether each other is reachable | Configure the mutual-TLS peer link over a raw tailnet TCP port | Each control plane keeps its own tasks; peering does not replicate them |

MagicDNS names devices so clients can reach a known host. It does not discover Conductor
projects or synchronize databases. Pick the first topology for agents in different tools
and checkouts that need one task ledger.

The [private AWS control-plane runbook](../deploy/aws/README.md) provisions that
topology on EC2 and describes a verified database cutover. Tailscale Serve exposes
the loopback API to the tailnet without opening an EC2 inbound rule.

## Recommended path: Tailscale (or Headscale)

1. Install and sign in to [Tailscale](https://tailscale.com/) (or a self-hosted
   [Headscale](https://headscale.net/) coordination server) on the host that runs
   `conductord` and on every teammate who should join.
2. Keep `conductord` on loopback (`127.0.0.1:<port>`). Publish only through Tailscale:

   ```bash
   tailscale serve --bg 8080   # plain HTTP backend on 127.0.0.1:8080
   # If the loopback daemon itself serves HTTPS, as on this Mac's port 8443:
   tailscale serve --bg https+insecure://127.0.0.1:8443
   ```

   Use the command that matches the daemon's actual listener. In the second form,
   `https+insecure` affects only Serve's connection to the loopback backend; clients
   still use Tailscale's HTTPS endpoint.

3. Use the machine's **MagicDNS** name as the public URL everywhere Conductor needs one:

   ```bash
   # restart or configure conductord with:
   --public-url https://your-mac.tailnet.ts.net

   # invites and saved client endpoints:
   conductor invite brother --endpoint https://your-mac.tailnet.ts.net
   ```

4. Share that MagicDNS host with teammates who are not yet on the tailnet from the
   Tailscale admin console (or your Headscale ACL). Stop publishing with
   `tailscale serve reset`.

`conductor doctor` reports Tailscale when it is connected and, for a loopback endpoint,
prints the `tailscale serve` + `invite --endpoint` commands. `conductor publish` runs the
serve step itself (and `conductor invite` prints the same advice when the join URL would
otherwise point at `127.0.0.1`).

| Surface | URL on the tailnet |
|---|---|
| Dashboard | `https://<magicdns>/` |
| Join link | from `conductor invite … --endpoint https://<magicdns>` |
| MCP (Streamable HTTP) | `https://<magicdns>/mcp` (or `/mcp/{project}`) |
| SSO callbacks | need `--public-url https://<magicdns>` and a browser on the tailnet |
| GitHub | a private tailnet URL is unreachable by GitHub webhooks; use Conductor's GitHub poller |

On each agent host, join the same tailnet, verify that the MagicDNS name resolves, then
save the endpoint and install the integration in each checkout:

```bash
conductor login --endpoint https://your-mac.tailnet.ts.net --token <your-own-token> --project <project>
conductor integrate codex --transport http --endpoint https://your-mac.tailnet.ts.net
conductor integrate claude --transport http --endpoint https://your-mac.tailnet.ts.net
conductor integrate cursor --transport http --endpoint https://your-mac.tailnet.ts.net
```

Run only the integrations for tools in use. Give each agent its own Conductor token and
project membership. `conductor peers` is a daemon link check; successful client login,
MCP calls, conflict checks, and task claims verify the shared workflow.

## Linking independent control planes

The dashboard Serve route above terminates HTTPS at Tailscale. Conductor's peer route
requires the daemons to exchange mesh client certificates during the TLS handshake, so
the dashboard route cannot carry peer mutual TLS. Expose a separate raw TCP forward on
each daemon host while keeping `conductord` bound to loopback:

```bash
# Run on each host; the daemon behind this port must serve its mesh certificate.
tailscale serve --bg --tcp=8444 tcp://127.0.0.1:8443

# On host A, name host B by its MagicDNS FQDN and raw-forward port.
conductord --addr 127.0.0.1:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/host-a.your-tailnet.ts.net/cert.pem \
  --peer-key .conductor/certs/host-a.your-tailnet.ts.net/key.pem \
  --peer host-b.your-tailnet.ts.net=https://host-b.your-tailnet.ts.net:8444
```

Configure host B symmetrically. On a secure provisioning machine, run
`scripts/gen-peer-certs.sh host-a.your-tailnet.ts.net host-b.your-tailnet.ts.net` to
put each exact MagicDNS FQDN in its certificate DNS SAN. Certificates generated for
short names alone will fail hostname verification at `*.ts.net`. Share the mesh CA
certificate and each host's own certificate/key through a secure channel; keep the CA
key off agent hosts.
Allow the peer port in the tailnet access policy and confirm `conductor peers` reports
`up` on both sides. Peer links report identity and reachability only. Use the shared
database topology above when tasks must appear on every instance.

`--peer-discover-dns` needs a DNS SRV record set. MagicDNS registers device names but
does not create Conductor's SRV directory. Use explicit `--peer` entries or run
`conductor-mesh-dns` (see "Service discovery over the tailnet" below) as that separate
DNS service.

Local sign-in never applies through the MagicDNS name: those requests are remote and use
the token in the invite link (see `docs/MACOS_APP.md`).

Headscale users follow the same pattern: MagicDNS (or equivalent) hostname + HTTPS in
front of the loopback port. Conductor does not talk to the Tailscale API; it only reads
`tailscale status --json` for advice.

## Second choice: Cloudflare Tunnel

If a private tailnet is not an option, put a tunnel in front of loopback and set
`--public-url` to the tunnel hostname. Same invite and MCP URLs as above. Prefer Tailscale
when every teammate can join the overlay; a public tunnel exposes more surface than a
tailnet ACL.

## Publish on the internet (Funnel)

`tailscale funnel` takes the Serve setup above and exposes the same HTTPS endpoint on the
public internet, no port forwarding or DNS of your own. `conductor publish --funnel` runs
the whole recipe; by hand it is (for the self-signed-TLS daemon on `127.0.0.1:8443`):

```bash
tailscale serve --bg --https=443 https+insecure://127.0.0.1:8443   # tailnet only
tailscale funnel --bg https+insecure://127.0.0.1:8443               # public internet
```

The MagicDNS name comes from `tailscale status --json` (`Self.DNSName`, minus the
trailing dot); afterwards the dashboard is `https://<magicdns>/` and MCP is
`https://<magicdns>/mcp`, and `--public-url` / `conductor invite … --endpoint` use the
same URL as with Serve. Funnel needs HTTPS certificates and Funnel enabled for the tailnet
in the admin console. Stop publishing with `tailscale funnel off` (or
`tailscale serve reset` to also drop the tailnet route).

Two footguns, both learned the hard way:

- **Always pass the full URL form to funnel.** `tailscale funnel --bg 8443` treats the
  number as a *backend target* and re-points the public route at plain
  `http://127.0.0.1:8443`, breaking the TLS backend. Same for serve: pass
  `https+insecure://127.0.0.1:8443`, not the port.
- **Re-running funnel commands can clobber the root route.** After any funnel change,
  check `tailscale serve status` (or re-run `conductor publish`, which verifies it) to
  confirm `<magicdns>:443` still proxies to the intended backend.
- On macOS the CLI lives at `/Applications/Tailscale.app/Contents/MacOS/Tailscale`.
  Invoke it by that absolute path or through PATH; a plain symlink **crashes** with
  "bundleIdentifier unknown to the registry".

Prefer plain Serve (tailnet only) whenever teammates can join the tailnet: Funnel exposes the
endpoint to everyone on the internet, so use it only for machines that cannot join.

## Service discovery over the tailnet

MagicDNS names one host; it does not publish *what* runs on it. `conductor-mesh-dns`
fills that gap: a tiny hand-rolled DNS server that answers one directory of SRV records —
conductor's daemon links, the dashboard, cairn's HTTP API and cairn p2p — and nothing
else. Run it on the machine that publishes the services:

```bash
conductor-mesh-dns --listen 0.0.0.0:15353
```

On loopback (the default `127.0.0.1:15353`) only local daemons see it, which is what
conductord's `--peer-discover-dns _conductor-mesh._tcp.mesh.internal` expects with
`CONDUCTOR_PEER_DNS_SERVER=127.0.0.1:15353`. With `--listen 0.0.0.0:15353` any node on
the tailnet can query it by pointing `dig` (or `resolvectl`, or a per-domain resolver) at
the publisher's tailscale IP:

```bash
dig -p 15353 @100.64.0.1 _cairn-p2p._tcp.mesh.internal SRV
# _cairn-p2p._tcp.mesh.internal. 60 IN SRV 0 0 9000 host-a.tail1234.ts.net.
```

The SRV target is the machine's MagicDNS FQDN — `--hostname` pins it explicitly, and
without the flag the server auto-detects it from `tailscale status --json`
(`Self.DNSName`), falling back to `localhost.` when the CLI is unavailable. The same
status JSON supplies the node's tailscale IP, which the server returns as an A record for
its own hostname, so SRV target resolution completes in one server.

The directory:

| SRV name | Ports | Service |
|---|---|---|
| `_conductor-mesh._tcp.mesh.internal.` | 8443, 8444 | conductord mesh peering (`--peer-discover-dns`) |
| `_conductor._tcp.mesh.internal.` | 443 | conductor dashboard/API behind `tailscale serve` |
| `_cairn-http._tcp.mesh.internal.` | 8443 | cairn HTTP API/UI |
| `_cairn-p2p._tcp.mesh.internal.` | 9000 | cairn p2p |

Set `MESH_DNS_SERVICES` (JSON, name → list of ports) to replace any of those or add new
ones, e.g. `MESH_DNS_SERVICES='{"_mytool._tcp.mesh.internal.":[7777]}'`. Everything not
in the directory gets NXDOMAIN.

Conductor peers consume the `_conductor-mesh` set through the same
`--peer-discover-dns` as on one machine: each daemon resolves the name on its DNS server,
dials the discovered targets (`https://<target>:<port>`), and verifies identity through
the mesh certificates rather than the DNS answer. A cairn node joining the mesh takes the
`_cairn-p2p` answer and uses the MagicDNS name plus port 9000 as its seed:

```bash
CAIRN_SEEDS=host-a.tail1234.ts.net:9000
```

Binding mesh-dns to `0.0.0.0` is tailnet-trust: everyone the tailnet ACLs allow can read
the directory (and reach the published ports). That is exactly the trust model of Serve
itself, but keep it off Funnel — there is no reason for the public internet to enumerate
your services.

## What Conductor does not build

- **UPnP / NAT-PMP** port mapping from the control plane
- **WebRTC** data channels as a second transport
- A Conductor-owned **STUN/TURN** relay

Those belong to the network layer you already chose, not to the coordination API.

## Live updates: SSE stays (no WebSocket)

Conductor streams project events with **Server-Sent Events**
(`GET /v1/projects/{project}/events/stream`). The dashboard and the planned macOS app use
the same stream. Agents talk over ordinary HTTPS (REST and MCP Streamable HTTP), not a
persistent browser socket.

Over Tailscale HTTPS this is enough:

- One-way server → client updates match how the product is used (presence, conflicts,
  offers). There is no interactive browser approval channel that needs client → server
  frames on the same socket.
- SSE rides ordinary HTTP/1.1 or HTTP/2; `tailscale serve` and typical reverse proxies
  already carry it (see DESIGN.md: prefer SSE through load balancers).
- A browser `EventSource` cannot set headers, so the stream already accepts `?token=` on
  that route only; WebSocket would not remove that special case.

Add WebSocket only if a future feature needs true bidirectional interactive traffic that
cannot be expressed as REST + SSE. Remote access over Tailscale is not that feature.

## STUN / TURN and “NAT traversal”

Tailscale (and compatible Headscale deployments) already perform NAT traversal for the
overlay: direct peer paths when possible, and Tailscale’s own DERP relays when not. From
Conductor’s point of view, teammates simply reach `https://<magicdns>/…` like any other
HTTPS host.

If product messaging mentions STUN/TURN in the same breath as remote access, frame it as
**provided by Tailscale’s network**, not as a Conductor-operated relay. A separate
STUN/TURN stack would only matter for a future non-Tailscale path (for example browser
WebRTC to a host outside the tailnet). That path is not planned; do not stand up TURN for
Conductor today.

## Quick checks

```bash
conductor doctor --json          # look at .tailscale and .tailscale_advice
tailscale status                 # BackendState Running, MagicDNS name present
tailscale serve --bg <port>
conductor invite <handle> --endpoint https://<magicdns>
```

See also: [MACOS_APP.md](MACOS_APP.md) (Invite sheet + Tailscale), README “Sharing with
someone by text”, DESIGN.md § on SSE vs WebSocket.
