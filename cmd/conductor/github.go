package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/githubapp"
)

// `conductor github` connects Conductor to GitHub through a GitHub App: one click creates the
// app with exactly the permissions it needs, one click installs it, and from then on every
// pull request on a linked repository gets a "Conductor" check saying whether it touches files
// other in-flight work has reserved.
func cmdGitHub(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "setup", "create":
		return githubSetup(ctx, args[1:])
	case "status":
		return githubStatus(ctx, args[1:])
	case "link":
		return githubLink(ctx, args[1:])
	case "check":
		return githubCheck(ctx, args[1:])
	case "install":
		return githubInstall(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, `conductor github — connect Conductor to GitHub

  conductor github setup [--org ORG]   create Conductor's GitHub App (opens a browser; one click)
  conductor github install             open the page that installs it on repositories
  conductor github link [owner/repo]   tell Conductor which repository this project is (default: origin)
  conductor github status              the app, where it is installed, which projects are linked
  conductor github check owner/repo#12 check one pull request now

The app reads pull requests and writes a "Conductor" check run. It cannot push, merge, or
change settings. A Conductor GitHub cannot reach (a laptop) polls for pull requests; one with
a public --public-url receives webhooks.
`)
		return nil
	default:
		return fmt.Errorf("unknown github subcommand %q (setup, install, link, status, check)", args[0])
	}
}

func githubSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("github setup", flag.ExitOnError)
	org := fs.String("org", "", "GitHub organisation that will own the app (default: your personal account)")
	name := fs.String("name", "", "app name shown on GitHub; must be unique on GitHub (default: Conductor <host> <random>)")
	replace := fs.Bool("replace", false, "create a new app in place of the one already connected")
	noOpen := fs.Bool("no-open", false, "print the setup link instead of opening a browser")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	var out struct {
		SetupURL  string    `json:"setup_url"`
		ExpiresAt time.Time `json:"expires_at"`
		Name      string    `json:"name"`
		Webhooks  bool      `json:"webhooks"`
	}
	if err := api.Post(ctx, "/v1/github/setup", map[string]any{"org": *org, "name": *name, "replace": *replace}, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	fmt.Printf("Creating the GitHub App %q.\n\n", out.Name)
	fmt.Println("  1. Click \"Create the GitHub App\" on the page that opens (GitHub asks you to confirm).")
	fmt.Println("  2. Click \"Install on repositories\" and pick the repositories Conductor coordinates.")
	fmt.Println("  3. Run `conductor github link` in each repository's checkout, if bootstrap did not already.")
	fmt.Printf("\nSetup page (works once, for an hour):\n  %s\n", out.SetupURL)
	if !out.Webhooks {
		fmt.Println("\nThis Conductor is not reachable from the internet, so it will poll GitHub for pull requests instead of receiving webhooks.")
	}
	if !*noOpen {
		openBrowser(out.SetupURL)
	}
	return nil
}

type githubStatusView struct {
	Configured bool   `json:"configured"`
	Mode       string `json:"mode"`
	App        *struct {
		ID    int64  `json:"id"`
		Slug  string `json:"slug"`
		Name  string `json:"name"`
		Owner string `json:"owner"`
		URL   string `json:"html_url"`
	} `json:"app"`
	InstallURL    string                   `json:"install_url"`
	Installations []githubapp.Installation `json:"installations"`
	InstallErr    string                   `json:"installations_error"`
	Linked        []map[string]string      `json:"linked"`
	LastPoll      *time.Time               `json:"last_poll"`
	LastError     string                   `json:"last_error"`
}

func fetchGitHubStatus(ctx context.Context) (githubStatusView, error) {
	var st githubStatusView
	api, _, err := mustClient()
	if err != nil {
		return st, err
	}
	return st, api.Get(ctx, "/v1/github/status", &st)
}

func githubStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("github status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := fetchGitHubStatus(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return emit(st)
	}
	if !st.Configured || st.App == nil {
		fmt.Println("No GitHub App is connected. `conductor github setup` creates one in two clicks.")
	} else {
		fmt.Printf("GitHub App  %s (%s), owned by %s\n", st.App.Name, st.App.Slug, orDash(st.App.Owner))
		fmt.Printf("Delivery    %s", st.Mode)
		if st.LastPoll != nil {
			fmt.Printf(", last poll %s ago", time.Since(*st.LastPoll).Round(time.Second))
		}
		fmt.Println()
		if st.LastError != "" {
			fmt.Printf("Last error  %s\n", st.LastError)
		}
		switch {
		case st.InstallErr != "":
			fmt.Printf("Installed   unknown (%s)\n", st.InstallErr)
		case len(st.Installations) == 0:
			fmt.Printf("Installed   nowhere yet. Install it: %s\n", st.InstallURL)
		default:
			var where []string
			for _, in := range st.Installations {
				where = append(where, in.Account)
			}
			fmt.Printf("Installed   on %s\n", strings.Join(where, ", "))
		}
	}
	if len(st.Linked) == 0 {
		fmt.Println("\nNo project of yours is linked to a GitHub repository. In a checkout: conductor github link")
		return nil
	}
	fmt.Println("\nLinked projects")
	for _, l := range st.Linked {
		fmt.Printf("  %-24s %s\n", l["project"], l["repository"])
	}
	return nil
}

func githubInstall(ctx context.Context, args []string) error {
	st, err := fetchGitHubStatus(ctx)
	if err != nil {
		return err
	}
	if st.InstallURL == "" {
		return errors.New("no GitHub App is connected yet: run `conductor github setup` first")
	}
	fmt.Println(st.InstallURL)
	openBrowser(st.InstallURL)
	return nil
}

func githubLink(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("github link", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	repo := ""
	if len(positional) > 0 {
		repo = positional[0]
	} else {
		remote, err := gitOutput(ctx, "remote", "get-url", "origin")
		if err != nil || strings.TrimSpace(remote) == "" {
			return errors.New("no origin remote here: name the repository, e.g. conductor github link acme/widgets")
		}
		repo = strings.TrimSpace(remote)
	}
	var out map[string]string
	if err := api.Post(ctx, "/v1/projects/"+ref+"/github", map[string]string{"repository": repo}, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	fmt.Printf("Project %s is linked to %s. Its pull requests will get a \"Conductor\" check once the app is installed there.\n",
		out["project"], out["repository"])
	return nil
}

var prRef = regexp.MustCompile(`^([A-Za-z0-9-]+/[A-Za-z0-9._-]+)#(\d+)$`)

func githubCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("github check", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor github check owner/repo#NUMBER")
	}
	m := prRef.FindStringSubmatch(positional[0])
	if m == nil {
		return fmt.Errorf("%q is not owner/repo#NUMBER", positional[0])
	}
	n, _ := strconv.Atoi(m[2])
	api, _, err := mustClient()
	if err != nil {
		return err
	}
	var rep struct {
		Conclusion string `json:"conclusion"`
		Files      int    `json:"files"`
		OwnTask    string `json:"own_task"`
		Posted     bool   `json:"posted"`
		Overlaps   []struct {
			File     string `json:"file"`
			Resource string `json:"resource"`
			TaskRef  string `json:"task_ref"`
			Owner    string `json:"owner"`
			Observed bool   `json:"observed"`
		} `json:"overlaps"`
	}
	if err := api.Post(ctx, "/v1/github/check", map[string]any{"repository": m[1], "number": n}, &rep); err != nil {
		return err
	}
	if *asJSON {
		return emit(rep)
	}
	fmt.Printf("%s: %s (%d files", positional[0], rep.Conclusion, rep.Files)
	if rep.OwnTask != "" {
		fmt.Printf(", own task %s", rep.OwnTask)
	}
	fmt.Println(")")
	for _, o := range rep.Overlaps {
		how := "reserved as " + o.Resource
		if o.Observed {
			how = "already changed"
		}
		fmt.Printf("  %-48s %s by %s (%s)\n", o.File, how, o.TaskRef, o.Owner)
	}
	if rep.Posted {
		fmt.Println("Posted the \"Conductor\" check run on GitHub.")
	} else {
		fmt.Println("The check on GitHub is already up to date.")
	}
	return nil
}

// openBrowser opens a URL in the user's browser, quietly doing nothing where it cannot.
func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", u)
	default:
		return
	}
	_ = cmd.Start()
}
