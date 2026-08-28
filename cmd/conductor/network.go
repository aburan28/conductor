package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/adamburan/conductor/internal/client"
)

// Networking diagnosis for `conductor doctor`.
//
// Conductor is hub-and-spoke: every tool — the CLI, `conductor wrap`, the MCP gateway, runners,
// the dashboard — connects *outbound* to one control plane, and nothing ever connects to
// anything else. So a machine's own NAT never matters; the only host that must be reachable is
// `conductord`. The failure people actually hit is inviting a teammate to a control plane bound
// to 127.0.0.1, which only the operator's own machine can reach. This turns that into a plain
// answer: can a teammate behind NAT reach the endpoint you would hand them?

// networkDiag is the verdict on whether the configured endpoint is reachable by others.
type networkDiag struct {
	Endpoint  string   `json:"endpoint"`
	Scheme    string   `json:"scheme"`
	Loopback  bool     `json:"loopback"`
	TLS       bool     `json:"tls"`
	Reachable bool     `json:"health_reachable"`
	Error     string   `json:"error,omitempty"`
	Verdict   string   `json:"verdict"` // local_only | reachable_tls | reachable_plaintext | unreachable | unknown
	Summary   string   `json:"summary"`
	Advice    []string `json:"advice,omitempty"`
}

// diagnoseNetworking classifies the endpoint and probes its health. The probe is unauthenticated
// (GET /v1/health needs no token) and short, so `doctor` stays fast and works before login.
func diagnoseNetworking(ctx context.Context, endpoint string) networkDiag {
	d := networkDiag{Endpoint: endpoint}
	if endpoint == "" {
		d.Verdict, d.Summary = "unknown", "no endpoint configured — run `conductor login` or `conductor join`"
		return d
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		d.Verdict, d.Summary = "unknown", "endpoint is not a valid URL: "+err.Error()
		return d
	}
	d.Scheme = u.Scheme
	d.TLS = u.Scheme == "https"
	d.Loopback = isLoopbackEndpoint(endpoint)

	// Live reachability: this is the same request a teammate's first call makes.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var health struct {
		Status string `json:"status"`
	}
	perr := client.New(endpoint, "").Get(probeCtx, "/v1/health", &health)
	d.Reachable = perr == nil
	if perr != nil {
		d.Error = perr.Error()
	}

	d.Verdict, d.Summary, d.Advice = networkVerdict(d.Loopback, d.TLS, d.Reachable)
	return d
}

// networkVerdict is the pure classification: given whether the endpoint is loopback, TLS, and
// currently answering, decide whether a NATed teammate can use it and what to do about it.
func networkVerdict(loopback, tls, reachable bool) (verdict, summary string, advice []string) {
	switch {
	case loopback:
		return "local_only",
			"bound to a loopback address — only this machine can reach it; teammates behind NAT cannot",
			exposureAdvice()
	case !reachable:
		return "unreachable",
			"the endpoint did not answer /v1/health — check the server is running and the address is right",
			exposureAdvice()
	case !tls:
		return "reachable_plaintext",
			"reachable, but over plain HTTP — bearer tokens would cross the network in the clear",
			[]string{
				"Put it behind TLS: conductord --addr 0.0.0.0:8080 --tls-cert cert.pem --tls-key key.pem",
				"…or --behind-proxy behind a proxy that terminates TLS,",
				"…or keep it on a private network (VPN / Tailscale / WireGuard), where plaintext is your choice to accept.",
			}
	default:
		return "reachable_tls",
			"reachable over TLS — a teammate behind NAT can connect to this",
			nil
	}
}

// exposureAdvice is the menu for making one control plane reachable to a NATed team. Conductor
// never opens tool-to-tool connections, so this is the only host that needs a routable address.
func exposureAdvice() []string {
	return []string{
		"Run conductord where everyone can reach it, then invite with that URL:",
		"  • cloud VM + TLS:   conductord --addr 0.0.0.0:8080 --tls-cert cert.pem --tls-key key.pem",
		"                      conductor invite <them> --endpoint https://your-host:8080",
		"  • private network:  put the host on Tailscale/WireGuard; every machine dials its tailnet IP",
		"                      (all machines can stay fully behind NAT — only the plane needs the address)",
		"  • reverse tunnel:   cloudflared/ngrok to a public URL, or --behind-proxy behind your own proxy",
		"Tools never connect to each other — each only needs an outbound path to this one endpoint.",
	}
}

func printNetworking(d networkDiag) {
	fmt.Printf("\nNetworking (can teammates reach this?)\n")
	fmt.Printf("  %-12s %s\n", "endpoint", orDash(d.Endpoint))
	reach := "no"
	if d.Reachable {
		reach = "yes"
	}
	if d.Error != "" {
		reach += " (" + d.Error + ")"
	}
	fmt.Printf("  %-12s %s\n", "reachable", reach)
	mark := map[string]string{
		"local_only": "LOCAL ONLY", "unreachable": "UNREACHABLE",
		"reachable_plaintext": "INSECURE", "reachable_tls": "ok", "unknown": "unknown",
	}[d.Verdict]
	fmt.Printf("  %-12s %s — %s\n", "verdict", mark, d.Summary)
	for _, a := range d.Advice {
		fmt.Printf("  %s\n", a)
	}
}
