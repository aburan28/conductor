package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/adamburan/conductor/internal/peer"
)

// cmdPeers reports the daemon-to-daemon mesh: which peers this control plane dials, and
// whether the mutual-TLS link to each is up.
func cmdPeers(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}

	var body struct {
		Peers []peer.LinkStatus `json:"peers"`
	}
	if err := api.Get(ctx, "/v1/peers", &body); err != nil {
		return err
	}
	if *asJSON {
		return emit(body.Peers)
	}

	if len(body.Peers) == 0 {
		fmt.Println("No peers configured. Run conductord with --peer-ca, --peer-cert and")
		fmt.Println("--peer-key to join the mesh (see scripts/gen-peer-certs.sh).")
		return nil
	}
	fmt.Printf("%-14s %-32s %-5s %6s  %s\n", "PEER", "ADDRESS", "STATE", "RTT", "LAST CHECK")
	for _, p := range body.Peers {
		rtt := "-"
		if p.State == peer.StateUp {
			rtt = fmt.Sprintf("%dms", p.RTTMillis)
		}
		fmt.Printf("%-14s %-32s %-5s %6s  %s\n",
			p.Name, p.URL, p.State, rtt, shortAgo(p.LastCheck))
		if p.LastError != "" {
			fmt.Printf("  └─ %s\n", p.LastError)
		}
	}
	fmt.Println()
	fmt.Println("Link state recomputes on every probe; down peers are retried, not dropped.")
	return nil
}

func shortAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%s ago", d)
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}
