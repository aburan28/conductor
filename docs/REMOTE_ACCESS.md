# Remote access

How teammates reach a Conductor control plane that is not on their own machine. Prefer a
private overlay; do not punch holes in home NATs from Conductor itself.

## Recommended path: Tailscale (or Headscale)

1. Install and sign in to [Tailscale](https://tailscale.com/) (or a self-hosted
   [Headscale](https://headscale.net/) coordination server) on the host that runs
   `conductord` and on every teammate who should join.
2. Keep `conductord` on loopback (`127.0.0.1:<port>`). Publish only through Tailscale:

   ```bash
   tailscale serve --bg 8080   # port must match conductord's --addr
   ```

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
prints the `tailscale serve` + `invite --endpoint` commands. `conductor invite` does the
same when the join URL would otherwise point at `127.0.0.1`.

| Surface | URL on the tailnet |
|---|---|
| Dashboard | `https://<magicdns>/` |
| Join link | from `conductor invite … --endpoint https://<magicdns>` |
| MCP (Streamable HTTP) | `https://<magicdns>/mcp` (or `/mcp/{project}`) |
| SSO / GitHub webhooks | need `--public-url https://<magicdns>` on `conductord` |

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
