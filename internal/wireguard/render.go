package wireguard

import (
	"fmt"
	"net"
	"strings"
)

const privateKeyPlaceholder = "REPLACE_WITH_THIS_NODES_LOCAL_PRIVATE_KEY"

// Render returns templates and instructions containing public inventory only.
// They intentionally cannot be activated until each owner adds their local key.
func (m *Manifest) Render() (map[string][]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(m.Gateway.Endpoint)
	if m.Gateway.ListenPort != 0 {
		port = fmt.Sprint(m.Gateway.ListenPort)
	}
	var gateway strings.Builder
	fmt.Fprintf(&gateway, "# Template: add the private key locally before activation.\n[Interface]\nPrivateKey = %s\nAddress = %s/32\nListenPort = %s\n", privateKeyPlaceholder, m.Gateway.Address, port)
	files := make(map[string][]byte, len(m.Nodes)+2)
	for _, node := range m.Nodes {
		fmt.Fprintf(&gateway, "\n# %s\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n", node.Name, node.PublicKey, node.Address)
		worker := fmt.Sprintf("# Template: add the private key locally before activation.\n[Interface]\nPrivateKey = %s\nAddress = %s/32\n\n[Peer]\nPublicKey = %s\nEndpoint = %s\nAllowedIPs = %s\nPersistentKeepalive = 25\n", privateKeyPlaceholder, node.Address, m.Gateway.PublicKey, m.Gateway.Endpoint, m.Network.CIDR)
		files[node.Name+".conf"] = []byte(worker)
	}
	files[m.Gateway.Name+".conf"] = []byte(gateway.String())
	files["README.md"] = []byte(fmt.Sprintf(`# WireGuard configuration templates

Overlay: %s
Gateway: %s (%s), endpoint %s

These files are inert templates. No host settings have been changed.

1. Transfer only each node's own .conf to that node using an approved channel.
2. Generate or use the private key on its owning node with WireGuard tooling.
   Verify its public key matches the manifest, then replace
   %s in that node's file. Never send private keys to Conductor or put them in
   the manifest. Keep filled configurations readable only by the owner (0600).
3. Have the network administrator confirm the gateway endpoint is reachable
   before the tunnel starts, allow its UDP port, enable IPv4 forwarding, and
   explicitly permit intended forwarding between the assigned overlay addresses
   on the gateway interface. Use the operating system's firewall policy and
   prevent unintended forwarding to other interfaces. On Linux, after enabling
   forwarding, disable both send_redirects and accept_redirects under
   net.ipv4.conf.all and net.ipv4.conf.default. Also disable both settings under
   net.ipv4.conf.<gateway-interface> once that interface exists (for example,
   wg0; use the actual interface name). Persist these with the approved network
   manager. Same-interface hub forwarding otherwise emits ICMP redirects and
   can disrupt worker routes. No forwarding, firewall,
   NAT, DNS, subnet-router, or internet-exit policy is generated or activated.
4. Install and activate each reviewed configuration with the platform's existing
   WireGuard tooling. Configurations use /32 addresses, /32 gateway peer routes,
   and worker routes limited to %s. Ensure the overlay does not overlap local
   LAN, container, VPN, or infrastructure routes on any participant.
5. Install internal TLS trust, bind Conductor to its private overlay address,
   use enhanced authentication, and point workers at its HTTPS endpoint. Configure
   internal DNS or hosts entries separately. Use explicit conductor-mesh-dns
   identity if appropriate (docs/LEADER.md).
6. Check WireGuard handshakes and permitted application traffic, including two
   workers communicating through the gateway. Verify forbidden traffic is denied
   by the actual firewall and application policy before sharing the network.

The gateway decrypts and re-encrypts traffic between its separate peer tunnels.
Application TLS or mTLS is necessary to protect payloads from the gateway.
Network membership does not provide workspace isolation: this topology routes
the private overlay through one gateway. Enforce project authorization in
Conductor and any required network isolation with administrator-managed firewall
rules or separate overlays. There is no default route, automatic relay/NAT
traversal, failover, key enrollment/rotation, or packet forwarding validation here.
`, m.Network.CIDR, m.Gateway.Name, m.Gateway.Address, m.Gateway.Endpoint, privateKeyPlaceholder, m.Network.CIDR))
	return files, nil
}
