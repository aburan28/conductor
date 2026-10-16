package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/client"
)

// Publishing the control plane over Tailscale (docs/REMOTE_ACCESS.md).
//
// `conductor publish` is one command for the verified recipe: conductord listens on
// loopback with self-signed TLS, `tailscale serve --bg --https=443` publishes it to the
// tailnet at https://<magicdns>/, and `--funnel` also exposes it publicly. The backend
// is always passed as a full URL: `funnel --bg 8443` treats the number as a backend
// target and would re-point the public route at plain HTTP, breaking the TLS backend.

// defaultPublishBackend is the loopback HTTPS listener `conductor up`/`conductor leader`
// use on this Mac when TLS is on.
const defaultPublishBackend = "https+insecure://127.0.0.1:8443"

// locateTailscaleCLI finds the tailscale command: PATH first, then the macOS app bundle.
// The bundle binary is exec'd by absolute path (argv[0] is the binary itself, which is
// safe); only a symlink into the bundle crashes with "bundleIdentifier unknown to the
// registry", so none is created.
func locateTailscaleCLI(lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	if p, err := lookPath("tailscale"); err == nil {
		return p, nil
	}
	for _, p := range []string{
		"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
	} {
		if info, err := stat(p); err == nil && !info.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("tailscale CLI not found")
}

// publishBackend derives the backend URL `tailscale serve` proxies to from the saved
// endpoint. A loopback https endpoint means self-signed TLS, so serve connects with
// https+insecure (that affects only the hop to the loopback daemon; clients still get
// Tailscale's HTTPS). Anything unrecognizable falls back to the TLS default.
func publishBackend(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Port() == "" {
		return defaultPublishBackend
	}
	if !isLoopbackHost(u.Hostname()) {
		return defaultPublishBackend
	}
	if u.Scheme == "https" {
		return "https+insecure://127.0.0.1:" + u.Port()
	}
	if u.Scheme == "http" {
		return "http://127.0.0.1:" + u.Port()
	}
	return defaultPublishBackend
}

// publishCommands is the exact argument vectors `conductor publish` runs, in order. Every
// backend is a full URL, never a bare port: `tailscale funnel --bg 8443` would treat
// 8443 as the backend target and re-point the public route at plain HTTP.
func publishCommands(funnel bool, backend string) [][]string {
	cmds := [][]string{{"serve", "--bg", "--https=443", backend}}
	if funnel {
		cmds = append(cmds, []string{"funnel", "--bg", backend})
	}
	return cmds
}

// publishServeStatus is the part of `tailscale serve status --json` publish verifies
// afterwards: re-running funnel commands can clobber the root route, so the route is
// re-read and checked rather than trusted.
type publishServeStatus struct {
	Web         map[string]publishSite `json:"Web"`
	AllowFunnel map[string]bool        `json:"AllowFunnel"`
}

type publishSite struct {
	Handlers map[string]struct {
		Proxy string `json:"Proxy"`
	} `json:"Handlers"`
}

// serveRootMatches reports whether the site at hostPort (e.g. "mac.tailnet.ts.net:443")
// proxies its root route to backend, and whether Funnel is on for it.
func serveRootMatches(statusJSON []byte, hostPort, backend string) (rootOK, funnel bool) {
	var st publishServeStatus
	if json.Unmarshal(statusJSON, &st) != nil {
		return false, false
	}
	site, ok := st.Web[hostPort]
	if !ok {
		return false, st.AllowFunnel[hostPort]
	}
	root, ok := site.Handlers["/"]
	rootOK = ok && strings.TrimSuffix(root.Proxy, "/") == backend
	return rootOK, st.AllowFunnel[hostPort]
}

// runTailscaleCLI runs the tailscale command; a variable so tests can stub it.
var runTailscaleCLI = func(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, args...).Output()
}

func cmdPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	funnel := fs.Bool("funnel", false, "also expose the control plane publicly (tailscale funnel); without it, only your tailnet")
	endpoint := fs.String("endpoint", "", "loopback endpoint conductord listens on (default: your saved endpoint)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor publish — share the control plane over Tailscale in one command

Runs `+"`tailscale serve --bg --https=443 <backend>`"+` against the loopback daemon, so the
dashboard and MCP are reachable at https://<magicdns>/ from your tailnet. With --funnel,
`+"`tailscale funnel --bg <backend>`"+` also exposes them on the public internet.

  conductor publish
  conductor publish --funnel

After publishing, set the printed URL as conductord --public-url (restart required) and
pass it to `+"`conductor invite <handle> --endpoint …`"+` — see docs/REMOTE_ACCESS.md.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cli, err := locateTailscaleCLI(exec.LookPath, os.Stat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Tailscale is not installed.")
		fmt.Fprintln(os.Stderr, "  Install it from https://tailscale.com/download, then sign in to your tailnet.")
		return err
	}

	statusJSON, err := runTailscaleCLI(ctx, cli, "status", "--json")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Tailscale is installed but not answering; is the app running and signed in?")
		fmt.Fprintln(os.Stderr, "  Open Tailscale and sign in, then run `conductor publish` again.")
		return fmt.Errorf("tailscale status: %w", err)
	}
	dnsName, ok := parseTailscaleStatus(statusJSON)
	if !ok {
		fmt.Fprintln(os.Stderr, "Tailscale is not connected (no running MagicDNS name).")
		fmt.Fprintln(os.Stderr, "  Open Tailscale and sign in — `tailscale up` inside the app — then run `conductor publish` again.")
		return errors.New("tailscale is not logged in")
	}

	creds := client.LoadCredentials()
	ep := strings.TrimRight(firstNonEmptyString(*endpoint, creds.Endpoint, "https://127.0.0.1:8443"), "/")
	backend := publishBackend(ep)

	fmt.Printf("MagicDNS name: %s\n", dnsName)
	hostPort := dnsName + ":443"
	for _, args := range publishCommands(*funnel, backend) {
		fmt.Printf("Running: tailscale %s\n", strings.Join(args, " "))
		if _, err := runTailscaleCLI(ctx, cli, args...); err != nil {
			return fmt.Errorf("tailscale %s failed: %w", args[0], err)
		}
	}

	// Re-reading the serve config is the check: re-running funnel commands can clobber
	// the root route, and the exit status alone would not say.
	serveJSON, err := runTailscaleCLI(ctx, cli, "serve", "status", "--json")
	if err != nil {
		return fmt.Errorf("verify with `tailscale serve status`: %w", err)
	}
	rootOK, funnelOn := serveRootMatches(serveJSON, hostPort, backend)
	if !rootOK {
		return fmt.Errorf("tailscale serve did not leave %s proxying to %s; run `tailscale serve status` and re-point the root route (a re-run funnel command may have clobbered it)", hostPort, backend)
	}
	if *funnel && !funnelOn {
		return errors.New("tailscale funnel did not enable Funnel for " + hostPort + "; check that HTTPS certificates and Funnel are enabled for this tailnet")
	}

	publicURL := "https://" + dnsName + "/"
	scope := "your tailnet"
	if funnelOn {
		scope = "the public internet"
	}
	fmt.Printf("\nConductor is published to %s:\n", scope)
	fmt.Printf("  dashboard: %s\n", publicURL)
	fmt.Printf("  MCP:       %s\n", publicURL+"mcp")
	fmt.Println()
	fmt.Printf("Restart conductord with --public-url %s so SSO, GitHub webhooks, and\n", publicURL)
	fmt.Printf("dashboard/MCP links use it, and invite teammates with:\n")
	fmt.Printf("  conductor invite <handle> --endpoint %s\n", publicURL)
	if !funnelOn {
		fmt.Println()
		fmt.Println("Publish to the public internet too with `conductor publish --funnel`.")
	}
	return nil
}
