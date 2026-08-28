package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// Onboarding a teammate with one link.
//
// `conductor invite <handle>` mints a teammate their own access token and bundles the
// endpoint, project, and token into a single join link. `conductor join <link>` is the other
// end: the teammate pastes the link and is logged in, ready to contribute to the swarm.
//
// The token rides in the URL *fragment* (the part after '#'), not the query string. A
// fragment is never sent to the server by a browser, so the credential stays out of every
// request line, access log, and proxy along the way — a better default than the query form
// for a link that may pass through a chat app. The CLI reads it as plain text either way; the
// web dashboard reads the fragment on load, then strips it from the address bar.

// ---------------------------------------------------------------------------
// invite
// ---------------------------------------------------------------------------

func cmdInvite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	role := fs.String("role", "contributor", "role to grant: contributor | reviewer | maintainer | observer | project_admin | runner")
	expires := fs.String("expires", "", "token lifetime, e.g. 7d, 12h, 2w (default: the server's 90-day human cap; 'never' only with --role runner)")
	endpoint := fs.String("endpoint", "", "public URL teammates use to reach this server (default: your saved endpoint or CONDUCTOR_PUBLIC_URL)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor invite — give a teammate access with one link

Mints a token for a teammate and prints a single join link that carries the endpoint,
project, and token. They run `+"`conductor join <link>`"+` (or open it in a browser) and they
are in — no flags to copy, no token to paste separately.

  conductor invite rachel
  conductor invite rachel --role maintainer --expires 7d
  conductor invite ci-runner --role runner --expires never --endpoint https://conductor.team

The link contains a bearer token: send it over a channel you trust, exactly as you would a
password. It is shown once and stored only as a hash.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 {
		return errors.New("usage: conductor invite <handle> [--role …] [--expires …]")
	}
	handle := positional[0]

	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	// The endpoint teammates will use: an explicit --endpoint, then CONDUCTOR_PUBLIC_URL,
	// then whatever this operator is logged in against.
	joinEndpoint := firstNonEmptyString(*endpoint, os.Getenv("CONDUCTOR_PUBLIC_URL"), creds.Endpoint)
	joinEndpoint = strings.TrimRight(joinEndpoint, "/")

	isRunner := *role == string(domain.RoleRunner)
	body := map[string]any{"handle": handle, "role": *role, "kind": "human"}
	if isRunner {
		body["kind"] = "runner_service"
	}
	switch strings.ToLower(strings.TrimSpace(*expires)) {
	case "", "default":
		// Let the server apply its default (a 90-day cap for a human; no expiry for a runner).
	case "never", "0":
		// The server deliberately caps human tokens, so "never" is meaningful only for a
		// service identity. Saying so plainly beats silently handing back a 90-day token.
		if !isRunner {
			return fmt.Errorf("a human token cannot be set to never expire: the server caps it at 90 days. " +
				"For a long-lived service credential use --role runner, or pass a duration like --expires 30d")
		}
		body["token_ttl"] = "0s"
	default:
		ttl, err := parseFriendlyDuration(*expires)
		if err != nil {
			return err
		}
		body["token_ttl"] = ttl.String()
	}

	var result struct {
		Handle    string `json:"handle"`
		Role      string `json:"role"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
		Created   bool   `json:"created_principal"`
	}
	if err := api.Post(ctx, "/v1/projects/"+ref+"/members", body, &result); err != nil {
		return err
	}
	if result.Token == "" {
		return errors.New("the server added the member but returned no token; they may already have one")
	}

	link := joinLink(joinEndpoint, ref, result.Token)
	loopback := isLoopbackEndpoint(joinEndpoint)

	if *asJSON {
		return emit(map[string]any{
			"handle": result.Handle, "role": result.Role, "created": result.Created,
			"endpoint": joinEndpoint, "project": ref, "token": result.Token,
			"expires_at": result.ExpiresAt, "join_url": link,
			"endpoint_reachable": !loopback,
		})
	}

	verb := "Invited"
	if !result.Created {
		verb = "Re-invited"
	}
	fmt.Printf("%s %s as %s on %s.\n\n", verb, result.Handle, result.Role, ref)
	fmt.Println("Send them this link, once, over a channel you trust:")
	fmt.Println()
	fmt.Println("  " + link)
	fmt.Println()
	fmt.Println("They run:  conductor join \"<link>\"     (or open it in a browser)")
	if result.ExpiresAt != "" {
		fmt.Printf("\nThe token expires %s.\n", result.ExpiresAt)
	} else {
		fmt.Println("\nThe token does not expire; revoke it with `conductor member remove` if it leaks.")
	}
	if loopback {
		fmt.Fprintf(os.Stderr, `
Warning: this link points at %s, which only reaches your own machine. A teammate on
another computer cannot use it. Expose the control plane and re-run with the public URL:

  conductord --addr 0.0.0.0:8080 --tls-cert cert.pem --tls-key key.pem   # or --behind-proxy
  conductor invite %s --endpoint https://your-host:8080
`, joinEndpoint, handle)
	}
	fmt.Fprintln(os.Stderr, "\nThis link contains a bearer token. It is shown once and stored only as a hash.")
	return nil
}

// ---------------------------------------------------------------------------
// join
// ---------------------------------------------------------------------------

func cmdJoin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	runner := fs.Bool("runner", false, "start a runner immediately after joining")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor join — accept an invite link and log in

Paste the link a teammate gave you (from `+"`conductor invite`"+`). It saves your endpoint,
token, and project, verifies them, and tells you how to start contributing.

  conductor join "https://conductor.team/#project=myrepo&token=cdt_…"

You can also pass the three values by hand if you prefer:

  conductor join --endpoint https://conductor.team --token cdt_… --project myrepo

Flags:
`)
		fs.PrintDefaults()
	}
	// Allow the link to appear as a positional arg, and the three fields as flags.
	endpoint := fs.String("endpoint", "", "control plane URL")
	token := fs.String("token", "", "your access token")
	projectFlag := fs.String("project", "", "project id or slug")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	creds := client.Credentials{
		Endpoint: *endpoint, Token: *token, Project: *projectFlag,
	}
	if len(positional) > 0 {
		parsed, err := parseJoinLink(positional[0])
		if err != nil {
			return err
		}
		// Explicit flags win over the link, so a teammate can override a stale endpoint.
		creds = mergeCreds(parsed, creds)
	}
	if creds.Endpoint == "" {
		// Fall back to a saved endpoint if the link carried only token+project.
		creds.Endpoint = client.LoadCredentials().Endpoint
	}
	if creds.Token == "" {
		return errors.New("no token in the link: paste the full `conductor join` link, or pass --token")
	}

	saved, projects, err := connectAndSave(ctx, creds)
	if err != nil {
		return fmt.Errorf("the link did not work against %s: %w", creds.Endpoint, err)
	}

	if *asJSON {
		return emit(map[string]any{
			"endpoint": saved.Endpoint, "handle": saved.Handle,
			"project": saved.Project, "projects": projects,
		})
	}
	fmt.Printf("Joined %s as %s.\n", saved.Endpoint, saved.Handle)
	if saved.Project != "" {
		fmt.Printf("Default project: %s\n", saved.Project)
	}
	for _, p := range projects {
		fmt.Printf("  %-24s %s\n", p.Slug, p.Role)
	}
	printContributeGuidance()

	if *runner {
		fmt.Println("\nStarting a runner…")
		return cmdWorker(ctx, nil)
	}
	return nil
}

// JoinedProject is one project a token grants access to, as /v1/whoami reports it.
type JoinedProject struct {
	Slug string `json:"slug"`
	Role string `json:"role"`
}

// connectAndSave verifies a credential against /v1/whoami, fills in the handle, defaults the
// project when the token grants exactly one, and persists the result. It is the shared core
// of `conductor login`, `conductor join`, and `conductor swarm join`, so the three cannot
// drift in how they resolve and store a login.
func connectAndSave(ctx context.Context, creds client.Credentials) (client.Credentials, []JoinedProject, error) {
	api := client.New(creds.Endpoint, creds.Token)
	var who struct {
		Principal struct {
			Handle string `json:"handle"`
		} `json:"principal"`
		Projects []JoinedProject `json:"projects"`
	}
	if err := api.Get(ctx, "/v1/whoami", &who); err != nil {
		return creds, nil, err
	}
	creds.Handle = who.Principal.Handle
	if creds.Project == "" && len(who.Projects) == 1 {
		creds.Project = who.Projects[0].Slug
	}
	if err := client.SaveCredentials(creds); err != nil {
		return creds, who.Projects, err
	}
	return creds, who.Projects, nil
}

// printContributeGuidance is the shared "you're in, here's how to help" footer for join and
// swarm join.
func printContributeGuidance() {
	fmt.Println("\nContribute capacity:")
	fmt.Println("  • interactive work:  conductor wrap claude       (your sessions take work offered to them)")
	fmt.Println("  • autonomous work:   conductor worker            (this machine executes queued tasks)")
	fmt.Println("  • spare budget:      conductor budget share <teammate> <tokens>")
	fmt.Println("\nSee the whole team with `conductor swarm`, and the dashboard with `conductor dashboard`.")
}

// ---------------------------------------------------------------------------
// link building and parsing
// ---------------------------------------------------------------------------

// joinLink builds the fragment-form onboarding URL: <endpoint>/#token=…&project=….
//
// The fragment keeps the token off the wire — a browser never sends it to the server — while
// the same URL still auto-connects the web dashboard, which reads the fragment on load.
func joinLink(endpoint, project, token string) string {
	frag := url.Values{}
	frag.Set("token", token)
	frag.Set("project", project)
	return strings.TrimRight(endpoint, "/") + "/#" + frag.Encode()
}

// parseJoinLink extracts endpoint, project, and token from a join link. It accepts the
// fragment form this tool emits, the older query form (`?token=&project=`) that
// `conductor dashboard` used, and a bare `token&project` fragment body.
func parseJoinLink(s string) (client.Credentials, error) {
	s = strings.TrimSpace(s)
	var creds client.Credentials
	u, err := url.Parse(s)
	if err != nil {
		return creds, fmt.Errorf("not a valid link: %w", err)
	}
	if u.Scheme != "" && u.Host != "" {
		creds.Endpoint = u.Scheme + "://" + u.Host
	}
	// Fragment first (the safer, preferred channel), then query.
	take := func(vals url.Values) {
		if v := vals.Get("token"); v != "" && creds.Token == "" {
			creds.Token = v
		}
		if v := vals.Get("project"); v != "" && creds.Project == "" {
			creds.Project = v
		}
	}
	// url.Parse leaves u.Fragment percent-decoded and keeps the raw form in RawFragment;
	// parse the raw form so a value is decoded exactly once, matching the query path.
	if rawFrag := u.RawFragment; rawFrag != "" {
		if vals, err := url.ParseQuery(rawFrag); err == nil {
			take(vals)
		}
	} else if u.Fragment != "" {
		if vals, err := url.ParseQuery(u.Fragment); err == nil {
			take(vals)
		}
	}
	take(u.Query())
	if creds.Token == "" {
		// Maybe the whole argument was just the fragment body, e.g. "token=…&project=…".
		if vals, err := url.ParseQuery(strings.TrimPrefix(s, "#")); err == nil {
			take(vals)
		}
	}
	if creds.Token == "" {
		return creds, errors.New("the link carries no token; make sure you pasted the whole thing")
	}
	return creds, nil
}

func mergeCreds(base, override client.Credentials) client.Credentials {
	if override.Endpoint != "" {
		base.Endpoint = override.Endpoint
	}
	if override.Token != "" {
		base.Token = override.Token
	}
	if override.Project != "" {
		base.Project = override.Project
	}
	return base
}

// isLoopbackEndpoint reports whether a URL points only at the local machine, so an invite
// against it cannot reach a teammate. Mirrors conductord's isLoopback, over a URL rather than
// a bind address.
func isLoopbackEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseFriendlyDuration accepts Go durations plus day and week suffixes ("7d", "2w"), which
// time.ParseDuration does not understand but people reach for constantly for token lifetimes.
func parseFriendlyDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	var d time.Duration
	switch {
	case strings.HasSuffix(s, "d"):
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: use forms like 7d, 2w, 12h, 90m", s)
		}
		d = scaleHours(days * 24)
	case strings.HasSuffix(s, "w"):
		weeks, err := strconv.ParseFloat(strings.TrimSuffix(s, "w"), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: use forms like 7d, 2w, 12h, 90m", s)
		}
		d = scaleHours(weeks * 7 * 24)
	default:
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: use forms like 7d, 2w, 12h, 90m", s)
		}
	}
	// A non-positive TTL would sail past the server's human-token cap (a zero/negative ttl is
	// treated as "no expiry"), which is exactly the footgun the cap exists to prevent. Reject
	// it here; the deliberate no-expiry path is `--expires never` with `--role runner`.
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive; for a non-expiring service token use --role runner --expires never", s)
	}
	return d, nil
}

// scaleHours converts a (possibly fractional) number of hours to a Duration, clamping an
// overflow to the max representable duration rather than wrapping to a negative one.
func scaleHours(hours float64) time.Duration {
	ns := hours * float64(time.Hour)
	const maxDur = float64(1<<63 - 1)
	if ns >= maxDur {
		return time.Duration(1<<63 - 1)
	}
	if ns <= -maxDur {
		return time.Duration(-1 << 63)
	}
	return time.Duration(ns)
}
