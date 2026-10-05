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

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/githubapp"
	"github.com/aburan28/conductor/internal/tracker"
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
	case "issues":
		return githubIssues(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, `conductor github — connect Conductor to GitHub

  conductor github setup [--org ORG]   create Conductor's GitHub App (opens a browser; one click)
  conductor github install             open the page that installs it on repositories
  conductor github link [owner/repo]   tell Conductor which repository this project is (default: origin)
  conductor github status              the app, where it is installed, which projects are linked
  conductor github check owner/repo#12 check one pull request now
  conductor github issues enable|disable|status|sync
                                       turn the repository's issues into tasks (conductor github issues -h)

The app reads pull requests and writes a "Conductor" check run; with issue sync on, it also
comments on, labels, and closes the synced issues. It cannot push, merge, or change settings.
A Conductor GitHub cannot reach (a laptop) polls; one with a public --public-url receives
webhooks.
`)
		return nil
	default:
		return fmt.Errorf("unknown github subcommand %q (setup, install, link, status, check, issues)", args[0])
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
	InstallURL     string                   `json:"install_url"`
	Installations  []githubapp.Installation `json:"installations"`
	InstallErr     string                   `json:"installations_error"`
	PermissionGaps []struct {
		Scope   string `json:"scope"`
		Missing string `json:"missing"`
		Fix     string `json:"fix"`
		URL     string `json:"url"`
	} `json:"permission_gaps"`
	Linked    []map[string]string `json:"linked"`
	LastPoll  *time.Time          `json:"last_poll"`
	LastError string              `json:"last_error"`
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
		// A permission the app asks for that GitHub has not granted: an app created before
		// issue sync, or an installation whose owner has not accepted the new permission yet.
		for _, gap := range st.PermissionGaps {
			who := "the app"
			if gap.Scope != "app" {
				who = "the installation on " + gap.Scope
			}
			fmt.Printf("Needs       %s for %s (issue sync). %s\n", gap.Missing, who, gap.Fix)
			if gap.URL != "" {
				fmt.Printf("            %s\n", gap.URL)
			}
		}
	}
	if len(st.Linked) == 0 {
		fmt.Println("\nNo project of yours is linked to a GitHub repository. In a checkout: conductor github link")
		return nil
	}
	fmt.Println("\nLinked projects")
	for _, l := range st.Linked {
		issues := "issues not synced"
		switch v := l["issues"]; {
		case v == "all":
			issues = "syncs every open issue"
		case strings.HasPrefix(v, "label:"):
			issues = "syncs issues labelled " + strings.TrimPrefix(v, "label:")
		}
		fmt.Printf("  %-24s %-32s %s\n", l["project"], l["repository"], issues)
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

// `conductor github issues` turns a linked repository's issues into tasks and tells each
// issue when its task is claimed and done (internal/tracker).
func githubIssues(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "enable":
		return githubIssuesEnable(ctx, args[1:], true)
	case "disable":
		return githubIssuesEnable(ctx, args[1:], false)
	case "status":
		return githubIssuesStatus(ctx, args[1:])
	case "sync":
		return githubIssuesSync(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(os.Stderr, `conductor github issues — GitHub Issues as Conductor tasks

  conductor github issues enable [--label conductor | --all] [--in-progress-label in-progress | --no-progress-label]
                                 [--public-visibility team_artifacts]
  conductor github issues disable
  conductor github issues status
  conductor github issues sync     import now instead of waiting for the next poll

Open issues on the project's linked repository that carry the label (or every open issue,
with --all) become ready tasks, with external_ref github:owner/repo#N. Issue edits reach the
task unless the task was edited in Conductor since (the later edit wins); closing an issue
cancels its task, reopening it brings the task back. A claimed task gets one comment on its
issue ("Claimed by <handle> via Conductor") and the in-progress label; a done task gets one
comment linking its pull request, and its issue is closed. Nothing written in Conductor is
ever sent to GitHub, and a private task is never written back to a public repository's issue.

Example:
  conductor github issues enable --label conductor
`)
		return nil
	default:
		return fmt.Errorf("unknown github issues subcommand %q (enable, disable, status, sync)", args[0])
	}
}

type issueSyncView struct {
	Project          string     `json:"project"`
	Repository       string     `json:"repository"`
	Enabled          bool       `json:"enabled"`
	Label            string     `json:"label"`
	All              bool       `json:"all"`
	InProgressLabel  string     `json:"in_progress_label"`
	PublicVisibility string     `json:"public_visibility"`
	LastSyncAt       *time.Time `json:"last_sync_at"`
	LastError        string     `json:"last_error"`
	WriteBackError   string     `json:"writeback_error"`
	Imported         int        `json:"imported"`
	Open             int        `json:"open"`
}

func printIssueSync(v issueSyncView) {
	if !v.Enabled {
		fmt.Printf("Issue sync is off for %s. Turn it on: conductor github issues enable\n", v.Project)
		return
	}
	which := "open issues labelled " + v.Label
	if v.All {
		which = "every open issue"
	}
	fmt.Printf("Project     %s ← %s (%s)\n", v.Project, orDash(v.Repository), which)
	label := v.InProgressLabel
	if label == "" {
		label = "none"
	}
	fmt.Printf("Write-back  claim and done comments; in-progress label: %s\n", label)
	fmt.Printf("Public repo imported tasks are %s\n", v.PublicVisibility)
	fmt.Printf("Imported    %d task(s), %d open\n", v.Imported, v.Open)
	if v.LastSyncAt != nil {
		fmt.Printf("Last sync   %s ago\n", time.Since(*v.LastSyncAt).Round(time.Second))
	}
	if v.LastError != "" {
		fmt.Printf("Problem     %s\n", v.LastError)
	}
	if v.WriteBackError != "" {
		fmt.Printf("Write-back  failing: %s\n", v.WriteBackError)
	}
}

func githubIssuesEnable(ctx context.Context, args []string, enable bool) error {
	name := "github issues disable"
	if enable {
		name = "github issues enable"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var label, progress, public *string
	var all, noProgress *bool
	if enable {
		label = fs.String("label", "", "import only open issues with this label (default conductor, or the label set before)")
		all = fs.Bool("all", false, "import every open issue, labelled or not")
		progress = fs.String("in-progress-label", "", "label put on an issue while its task is worked (default in-progress)")
		noProgress = fs.Bool("no-progress-label", false, "write no in-progress label")
		public = fs.String("public-visibility", "", "visibility of tasks imported from a public repository (default team_artifacts)")
	}
	if err := fs.Parse(args); err != nil {
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
	body := map[string]any{"enabled": enable}
	if enable {
		if *label != "" {
			body["label"] = *label
		}
		body["all"] = *all
		switch {
		case *noProgress:
			body["in_progress_label"] = ""
		case *progress != "":
			body["in_progress_label"] = *progress
		}
		if *public != "" {
			body["public_visibility"] = *public
		}
	}
	var out issueSyncView
	if err := api.Post(ctx, "/v1/projects/"+ref+"/github/issues", body, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	if enable {
		fmt.Println("Issue sync is on; the first import runs with the next poll (or now: conductor github issues sync).")
	}
	printIssueSync(out)
	return nil
}

func githubIssuesStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("github issues status", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
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
	var out issueSyncView
	if err := api.Get(ctx, "/v1/projects/"+ref+"/github/issues", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	printIssueSync(out)
	return nil
}

func githubIssuesSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("github issues sync", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
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
	var rep struct {
		Repository  string `json:"repository"`
		Seen        int    `json:"seen"`
		Created     int    `json:"created"`
		Linked      int    `json:"linked"`
		Updated     int    `json:"updated"`
		Cancelled   int    `json:"cancelled"`
		Revived     int    `json:"revived"`
		NotModified bool   `json:"not_modified"`
		More        bool   `json:"more"`
	}
	if err := api.Post(ctx, "/v1/projects/"+ref+"/github/issues/sync", map[string]any{}, &rep); err != nil {
		return err
	}
	if *asJSON {
		return emit(rep)
	}
	if rep.NotModified {
		fmt.Printf("%s: nothing changed since the last sync.\n", rep.Repository)
		return nil
	}
	fmt.Printf("%s: read %d issue(s); %d imported, %d linked to existing tasks, %d updated, %d cancelled, %d reopened.\n",
		rep.Repository, rep.Seen, rep.Created, rep.Linked, rep.Updated, rep.Cancelled, rep.Revived)
	if rep.More {
		fmt.Println("More remain; the poller continues from here, or run this again.")
	}
	return nil
}

// issueLink is the web address, under web (empty: github.com), of the GitHub issue a task
// names in its external_ref, or "" for a task that names none.
func issueLink(web, externalRef string) string {
	name, key, ok := tracker.ParseRef(externalRef)
	if !ok || name != db.TrackerGitHub {
		return ""
	}
	u, _ := githubapp.IssueURL(web, key)
	return u
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
