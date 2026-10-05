package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/harness"
	"github.com/adamburan/conductor/internal/integrations"
	"github.com/adamburan/conductor/internal/version"
)

// defaultLocalDBAddr is where `conductor up` expects the local Postgres when DATABASE_URL is
// unset. Doctor checks only that something listens there, never the credentials, so it
// carries no password.
const defaultLocalDBAddr = "localhost:55432"

// doctorReport is what `conductor doctor --json` prints.
type doctorReport struct {
	Endpoint       string `json:"endpoint"`
	Reachable      bool   `json:"reachable"`
	Principal      string `json:"principal,omitempty"`
	Project        string `json:"project,omitempty"`
	ClientVersion  string `json:"client_version"`
	ServerVersion  string `json:"server_version,omitempty"`
	VersionWarning string `json:"version_warning,omitempty"`
	// Database is the control plane's own view of its database when it answers, and
	// otherwise, for a control plane on this machine, whether anything listens at the DSN.
	Database      string                 `json:"database"`
	DatabaseOK    bool                   `json:"database_ok"`
	Conductord    string                 `json:"conductord,omitempty"`
	Git           string                 `json:"git,omitempty"`
	Docker        string                 `json:"docker"`
	Harnesses     []harness.Capabilities `json:"harnesses"`
	Disabled      []harness.Capabilities `json:"disabled_harnesses,omitempty"`
	Repository    string                 `json:"repository,omitempty"`
	Integrations  []integrations.Status  `json:"integrations"`
	loggedIn      bool
	serverHealthy bool
}

// doctorEnv is everything doctor reads from the machine, so tests can supply a machine.
type doctorEnv struct {
	lookPath   func(string) (string, error)
	output     func(name string, args ...string) (string, error)
	daemon     func() (string, error)
	portOpen   func(host, port string) bool
	dockerUp   func() bool
	getenv     func(string) string
	clientVers string
}

func realDoctorEnv() doctorEnv {
	return doctorEnv{
		lookPath: exec.LookPath,
		output: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).Output()
			return strings.TrimSpace(string(out)), err
		},
		daemon:     locateDaemon,
		portOpen:   portOpen,
		dockerUp:   dockerAvailable,
		getenv:     os.Getenv,
		clientVers: version.Version(),
	}
}

func cmdDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Resolved before the registry, not after, because a repository may declare harnesses of
	// its own and doctor's whole job is reporting what this machine will actually run.
	repoRoot, _ := config.FindRoot(".")
	out := collectDoctor(ctx, client.LoadCredentials(), repoRoot, realDoctorEnv())
	if *asJSON {
		return emit(out)
	}
	printDoctor(os.Stdout, out)
	return nil
}

func collectDoctor(ctx context.Context, creds client.Credentials, repoRoot string, env doctorEnv) doctorReport {
	configs := harnessConfigs(repoRoot)
	reg := harness.BuildRegistry(configs)
	out := doctorReport{
		Endpoint:      creds.Endpoint,
		Project:       creds.Project,
		ClientVersion: env.clientVers,
		Harnesses:     reg.CapabilityReport(ctx),
		Disabled:      disabledHarnesses(ctx, configs),
		Repository:    repoRoot,
		loggedIn:      creds.Token != "",
	}

	// Health needs no token, so the server's version and database are reported even before
	// the first login, which is exactly when a stale binary does the most confusing damage.
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	health, healthErr := fetchHealth(hctx, creds.Endpoint)
	cancel()
	out.serverHealthy = healthErr == nil
	out.ServerVersion = health.Version
	switch {
	case healthErr == nil:
		out.Database, out.DatabaseOK = "ok (as seen by the control plane)", true
	case health.Database != "":
		out.Database = "the control plane cannot reach it: " + health.Database
	default:
		out.Database, out.DatabaseOK = localDatabaseStatus(creds.Endpoint, env)
	}
	if healthErr == nil || health.Status != "" {
		out.VersionWarning = versionWarning(out.ClientVersion, out.ServerVersion)
	}

	if creds.Token != "" {
		api := client.New(creds.Endpoint, creds.Token)
		var who struct {
			Principal struct {
				Handle string `json:"handle"`
			} `json:"principal"`
		}
		if err := api.Get(ctx, "/v1/whoami", &who); err == nil {
			out.Reachable = true
			out.Principal = who.Principal.Handle
		}
	}

	if p, err := env.daemon(); err == nil {
		out.Conductord = p
	}
	if _, err := env.lookPath("git"); err == nil {
		if v, err := env.output("git", "--version"); err == nil {
			out.Git = strings.TrimPrefix(v, "git version ")
		} else {
			out.Git = "installed"
		}
	}
	switch {
	case env.dockerUp():
		out.Docker = "running"
	case lookOK(env.lookPath, "docker"):
		out.Docker = "installed, but the daemon is not reachable"
	default:
		out.Docker = "not installed"
	}

	// Which coding tools on this machine are wired to Conductor, and how.
	home, _ := os.UserHomeDir()
	out.Integrations = integrations.Statuses(integrations.Options{
		Root: repoRoot, Home: home, Getenv: env.getenv,
	})
	return out
}

func lookOK(lookPath func(string) (string, error), name string) bool {
	_, err := lookPath(name)
	return err == nil
}

type healthBody struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Database string `json:"database"`
}

// fetchHealth reads /v1/health. A degraded server answers 503 with the database error in
// the body, which is returned alongside the error so the caller can show it.
func fetchHealth(ctx context.Context, endpoint string) (healthBody, error) {
	var body healthBody
	err := client.New(endpoint, "").Get(ctx, "/v1/health", &body)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		_ = json.Unmarshal(apiErr.Body, &body)
	}
	return body, err
}

// localDatabaseStatus answers for a control plane that is not running. Only a control plane
// on this machine has a database this machine is responsible for; for a remote one there is
// nothing local to check.
func localDatabaseStatus(endpoint string, env doctorEnv) (string, bool) {
	if !isLoopbackEndpoint(endpoint) {
		return "unknown (the control plane is not reachable)", false
	}
	addr := defaultLocalDBAddr
	if dsn := env.getenv("DATABASE_URL"); dsn != "" {
		u, err := url.Parse(dsn)
		if err != nil || u.Host == "" {
			return "DATABASE_URL is not a URL this command can read", false
		}
		addr = u.Host
		if u.Port() == "" {
			addr = net.JoinHostPort(u.Hostname(), "5432")
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "cannot parse the database address " + addr, false
	}
	if !isLoopbackHost(host) {
		return "at " + addr + " (not on this machine; not checked)", false
	}
	if env.portOpen(host, port) {
		return "Postgres is listening at " + addr + "; `conductor up` starts the control plane", true
	}
	return "nothing is listening at " + addr + "; `conductor up` starts Postgres in Docker, " +
		"or set DATABASE_URL to a Postgres 16+ you already run", false
}

// versionWarning explains a client/server version mismatch, or returns "" when there is
// nothing to say.
func versionWarning(clientVersion, serverVersion string) string {
	if serverVersion == "" {
		return "the control plane does not report its version, so it is older than this CLI; " +
			"upgrade conductord (make install, or the release installer) and restart it"
	}
	if version.Mismatch(clientVersion, serverVersion) {
		return fmt.Sprintf("this CLI is %s but the control plane is %s; install matching builds "+
			"(make install, or the release installer) and restart the server with `conductor down && conductor up`",
			clientVersion, serverVersion)
	}
	return ""
}

// disabledHarnesses probes the harnesses this project turned off, so doctor can still say
// whether, say, codex is installed and how to turn it on.
func disabledHarnesses(ctx context.Context, configs map[string]harness.HarnessConfig) []harness.Capabilities {
	var names []string
	for name, cfg := range configs {
		if !cfg.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var out []harness.Capabilities
	for _, name := range names {
		cfg := configs[name]
		cfg.Enabled = true
		reg := harness.BuildRegistry(map[string]harness.HarnessConfig{name: cfg})
		d, err := reg.Get(name)
		if err != nil {
			continue
		}
		out = append(out, d.Capabilities(ctx))
	}
	return out
}

func printDoctor(w io.Writer, out doctorReport) {
	fmt.Fprintf(w, "Control plane\n  %-12s %s\n", "endpoint", out.Endpoint)
	switch {
	case out.Reachable:
		fmt.Fprintf(w, "  %-12s reachable as %s\n", "status", out.Principal)
	case !out.loggedIn && out.serverHealthy && isLoopbackEndpoint(out.Endpoint):
		fmt.Fprintf(w, "  %-12s running; not logged in yet (`conductor login` signs you in on this machine, no token needed)\n", "status")
	case !out.loggedIn && out.serverHealthy:
		fmt.Fprintf(w, "  %-12s running; not logged in (%s)\n", "status", notLoggedInHint(out.Endpoint))
	case !out.loggedIn:
		fmt.Fprintf(w, "  %-12s not running and not logged in (%s)\n", "status", notLoggedInHint(out.Endpoint))
	default:
		fmt.Fprintf(w, "  %-12s unreachable\n", "status")
	}
	if out.Project != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "project", out.Project)
	}
	if out.Repository != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "repository", out.Repository)
	}
	fmt.Fprintf(w, "  %-12s %s\n", "database", out.Database)

	fmt.Fprintf(w, "\nVersions\n  %-12s %s\n", "conductor", out.ClientVersion)
	if out.ServerVersion != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "server", out.ServerVersion)
	}
	if out.VersionWarning != "" {
		fmt.Fprintf(w, "  %-12s %s\n", "WARNING", out.VersionWarning)
	}

	fmt.Fprintf(w, "\nThis machine\n")
	fmt.Fprintf(w, "  %-12s %s\n", "conductord", orMissing(out.Conductord,
		"not found next to conductor or on PATH (needed to run a control plane here)"))
	fmt.Fprintf(w, "  %-12s %s\n", "git", orMissing(out.Git, "not found (required)"))
	fmt.Fprintf(w, "  %-12s %s\n", "docker", out.Docker+" (only needed when no Postgres is reachable)")

	fmt.Fprintf(w, "\nHarnesses\n%s", harness.Describe(out.Harnesses))
	for _, c := range out.Disabled {
		state := "not installed"
		if c.Available {
			state = "installed"
			if c.Version != "" {
				state += " (" + c.Version + ")"
			}
		}
		fmt.Fprintf(w, "  %-10s %s; disabled for this project (enable under harnesses: in .conductor/project.yaml)\n",
			c.Kind, state)
	}

	fmt.Fprintf(w, "\nIntegrations\n")
	var absent []string
	for _, st := range out.Integrations {
		if !st.Detected && !st.Configured {
			absent = append(absent, st.Tool)
			continue
		}
		state := "not connected"
		if st.Configured {
			state = "connected"
			if st.Transport != "" {
				state += " (" + st.Transport + ")"
			}
			if st.HooksSupported {
				if st.Hooks {
					state += " · hooks on"
				} else {
					state += " · hooks off"
				}
			}
		}
		fix := ""
		if !st.Configured || (st.HooksSupported && !st.Hooks) {
			fix = st.Fix
		}
		fmt.Fprintf(w, "  %-11s %-32s %s\n", st.Tool, state, fix)
	}
	if len(absent) > 0 {
		fmt.Fprintf(w, "  %-11s %s\n", "not found", strings.Join(absent, ", "))
	}
}

func orMissing(v, missing string) string {
	if v == "" {
		return missing
	}
	return v
}
