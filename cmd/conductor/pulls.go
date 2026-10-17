package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/privacy"
)

// Pull requests (docs/COLLABORATION.md §4.5).
//
//	conductor pulls [--open|--mine|--unlinked|--needs-review|--failing] [--all] [--json]
//	conductor pull 118                       one pull request: task, checks, review, attempts
//	conductor task link-pr T-42 118          link by hand; outranks every automatic link
//	conductor task unlink-pr T-42 118
//
// Everything here reads what conductord recorded from GitHub: titles, states, and counts. No
// pull request body, comment, or log is stored, so none can be shown.

// pullsQuery is what `conductor pulls` asks for.
type pullsQuery struct {
	State       string // open | merged | closed | all
	Mine        bool
	Unlinked    bool
	NeedsReview bool
	Failing     bool
	Task        string
	Repository  string
	Number      int
	Limit       int
}

func (q pullsQuery) path(project string) string {
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return ""
	}
	number := ""
	if q.Number > 0 {
		number = strconv.Itoa(q.Number)
	}
	limit := ""
	if q.Limit > 0 {
		limit = strconv.Itoa(q.Limit)
	}
	return "/v1/projects/" + url.PathEscape(project) + "/pulls" + client.Query(
		"state", q.State, "mine", flag(q.Mine), "unlinked", flag(q.Unlinked), "needs_review", flag(q.NeedsReview),
		"failing", flag(q.Failing), "task", q.Task, "repository", q.Repository, "number", number, "limit", limit)
}

func cmdPulls(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pulls", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	open := fs.Bool("open", false, "open pull requests (the default)")
	all := fs.Bool("all", false, "include merged and closed pull requests")
	mine := fs.Bool("mine", false, "opened by you (by your linked GitHub account) or for a task you own")
	unlinked := fs.Bool("unlinked", false, "not linked to any task")
	needsReview := fs.Bool("needs-review", false, "open, ready for review, and not yet approved or sent back")
	failing := fs.Bool("failing", false, "at least one failed check")
	task := fs.String("task", "", "only this task's pull requests (T-42)")
	limit := fs.Int("limit", 0, "at most this many (default 200)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if *open && *all {
		return errors.New("--open and --all contradict each other")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	q := pullsQuery{State: "open", Mine: *mine, Unlinked: *unlinked, NeedsReview: *needsReview,
		Failing: *failing, Task: *task, Limit: *limit}
	if *all {
		q.State = "all"
	}
	return runPulls(ctx, api, ref, q, *asJSON, os.Stdout)
}

// runPulls lists pull requests.
func runPulls(ctx context.Context, api *client.Client, project string, q pullsQuery, asJSON bool, w io.Writer) error {
	var out struct {
		Pulls []privacy.PullRequestView `json:"pulls"`
	}
	if err := api.Get(ctx, q.path(project), &out); err != nil {
		return err
	}
	if asJSON {
		if out.Pulls == nil {
			out.Pulls = []privacy.PullRequestView{}
		}
		return emitTo(w, out.Pulls)
	}
	if len(out.Pulls) == 0 {
		fmt.Fprintln(w, "No pull requests.")
		return nil
	}
	multi := false
	for _, p := range out.Pulls {
		multi = multi || p.Repository != out.Pulls[0].Repository
	}
	for _, p := range out.Pulls {
		fmt.Fprintln(w, pullLine(p, multi))
	}
	return nil
}

// pullLine is one pull request on one line.
func pullLine(p privacy.PullRequestView, withRepo bool) string {
	name := "#" + strconv.Itoa(p.Number)
	if withRepo {
		name = p.Repository + name
	}
	if p.Redacted {
		return fmt.Sprintf("  %-8s %-7s (pull request of a private task)", name, p.State)
	}
	state := p.State
	if p.Draft && p.State == "open" {
		state = "draft"
	}
	task := p.TaskRef
	if task == "" {
		task = "—"
	}
	title := p.Title
	if title == "" {
		title = "(no title yet)"
	}
	return fmt.Sprintf("  %-8s %-7s %-7s %-10s %-12s %-8s %s", name, state, task, firstNonEmpty(p.AuthorHandle, p.Author, "—"),
		checksText(p.Checks), reviewText(p.ReviewDecision), title)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func checksText(c *privacy.PullChecks) string {
	switch {
	case c == nil:
		return "no checks"
	case c.Failed > 0:
		return fmt.Sprintf("%d/%d failed", c.Failed, c.Total)
	case c.Pending > 0:
		return fmt.Sprintf("%d/%d pending", c.Pending, c.Total)
	}
	return fmt.Sprintf("%d/%d passed", c.Passed, c.Total)
}

func reviewText(decision string) string {
	switch decision {
	case "approved":
		return "approved"
	case "changes_requested":
		return "changes"
	case "review_required":
		return "review"
	}
	return "—"
}

// pullArg is a pull request as a person names it: a URL, owner/repo#N, #N, or N.
type pullArg struct {
	Repository string // empty: the project's own repository
	Number     int
}

var (
	pullArgURL = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/([0-9]+)/?$`)
	pullArgKey = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#?([0-9]+)$`)
)

func parsePullArg(s string) (pullArg, error) {
	s = strings.TrimSpace(s)
	for _, re := range []*regexp.Regexp{pullArgURL, pullArgKey} {
		if m := re.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[2])
			if n > 0 {
				return pullArg{Repository: m[1], Number: n}, nil
			}
		}
	}
	return pullArg{}, fmt.Errorf("%q is not a pull request: use a number, owner/repo#N, or the pull request's URL", s)
}

func cmdPull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	repo := fs.String("repo", "", "owner/name, when the project has pull requests from more than one")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor pull <number | owner/repo#N | URL>")
	}
	arg, err := parsePullArg(positional[0])
	if err != nil {
		return err
	}
	if arg.Repository == "" {
		arg.Repository = *repo
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	return runPull(ctx, api, ref, arg, *asJSON, os.Stdout)
}

// pullDetail mirrors GET .../pulls/{owner}/{repo}/{number}.
type pullDetail struct {
	Pull     privacy.PullRequestView `json:"pull"`
	Task     *privacy.TaskView       `json:"task,omitempty"`
	Attempts []privacy.AttemptView   `json:"attempts,omitempty"`
}

// findPull resolves a pull request named without its repository: the project's pull requests
// numbered N, which must be one.
func findPull(ctx context.Context, api *client.Client, project string, arg pullArg) (string, error) {
	if arg.Repository != "" {
		return arg.Repository, nil
	}
	q := pullsQuery{State: "all", Number: arg.Number}
	var out struct {
		Pulls []privacy.PullRequestView `json:"pulls"`
	}
	if err := api.Get(ctx, q.path(project), &out); err != nil {
		return "", err
	}
	switch len(out.Pulls) {
	case 0:
		return "", fmt.Errorf("no pull request #%d in this project (conductor pulls --all lists the ones Conductor has seen)", arg.Number)
	case 1:
		return out.Pulls[0].Repository, nil
	}
	repos := make([]string, 0, len(out.Pulls))
	for _, p := range out.Pulls {
		repos = append(repos, p.Repository)
	}
	return "", fmt.Errorf("#%d exists in several repositories (%s); name one: conductor pull owner/repo#%d", arg.Number, strings.Join(repos, ", "), arg.Number)
}

func runPull(ctx context.Context, api *client.Client, project string, arg pullArg, asJSON bool, w io.Writer) error {
	repo, err := findPull(ctx, api, project, arg)
	if err != nil {
		return err
	}
	var d pullDetail
	if err := api.Get(ctx, "/v1/projects/"+url.PathEscape(project)+"/pulls/"+repo+"/"+strconv.Itoa(arg.Number), &d); err != nil {
		return err
	}
	if asJSON {
		return emitTo(w, d)
	}
	printPull(w, d)
	return nil
}

func printPull(w io.Writer, d pullDetail) {
	p := d.Pull
	if p.Redacted {
		fmt.Fprintf(w, "%s#%d  %s\n  A pull request of a private task: Conductor shows its number and state, nothing more.\n  %s\n", p.Repository, p.Number, p.State, p.URL)
		return
	}
	state := p.State
	if p.Draft && p.State == "open" {
		state = "draft"
	}
	fmt.Fprintf(w, "%s#%d  %s\n", p.Repository, p.Number, p.Title)
	author := firstNonEmpty(p.Author, "unknown")
	if p.AuthorHandle != "" {
		author += " (" + p.AuthorHandle + ")"
	}
	fmt.Fprintf(w, "  state    %s, by %s\n  %s\n", state, author, p.URL)
	if p.HeadRef != "" {
		head := p.HeadSHA
		if len(head) > 8 {
			head = head[:8]
		}
		fork := ""
		if p.FromFork {
			fork = " (from a fork)"
		}
		fmt.Fprintf(w, "  branch   %s → %s%s  head %s\n", p.HeadRef, firstNonEmpty(p.BaseRef, "?"), fork, head)
	}
	if p.TaskRef != "" {
		extra := ""
		if p.LinkSource != "" {
			extra = ", linked by " + strings.ReplaceAll(p.LinkSource, "_", " ")
		}
		if p.Primary {
			extra += ", primary"
		}
		fmt.Fprintf(w, "  task     %s (%s%s)\n", p.TaskRef, p.TaskStatus, extra)
	} else {
		fmt.Fprintf(w, "  task     none — conductor task link-pr <task> %d\n", p.Number)
	}
	if p.Checks != nil {
		fmt.Fprintf(w, "  checks   %d passed, %d failed, %d pending\n", p.Checks.Passed, p.Checks.Failed, p.Checks.Pending)
	} else {
		fmt.Fprintln(w, "  checks   none reported")
	}
	review := strings.ReplaceAll(firstNonEmpty(p.ReviewDecision, "no decision"), "_", " ")
	if p.UnresolvedThreads > 0 {
		review += fmt.Sprintf(", %d unresolved thread(s)", p.UnresolvedThreads)
	}
	fmt.Fprintf(w, "  review   %s\n", review)
	if p.Mergeable != "" {
		fmt.Fprintf(w, "  merge    %s\n", p.Mergeable)
	}
	if p.ChangedFiles > 0 {
		fmt.Fprintf(w, "  size     +%d −%d in %d file(s)\n", p.Additions, p.Deletions, p.ChangedFiles)
	}
	if len(d.Attempts) > 0 {
		fmt.Fprintln(w, "  attempts")
		for _, a := range d.Attempts {
			fmt.Fprintf(w, "    #%d  %-10s %-8s %s\n", a.AttemptNumber, a.State, a.Harness, a.Branch)
		}
	}
}

// taskLinkPR is `conductor task link-pr <task> <pull request>`.
func taskLinkPR(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task link-pr", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: conductor task link-pr <task> <number | owner/repo#N | URL>")
	}
	if _, err := parsePullArg(positional[1]); err != nil {
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
	var out struct {
		Linked  bool                    `json:"linked"`
		TaskRef string                  `json:"task_ref"`
		Pull    privacy.PullRequestView `json:"pull"`
	}
	if err := api.Post(ctx, "/v1/tasks/"+url.PathEscape(positional[0])+"/pull-requests"+client.Query("project", ref),
		map[string]any{"pull_request": positional[1]}, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	if out.Linked {
		fmt.Printf("Linked %s#%d to %s. It stays linked: a person's link outranks the branch, trailer and body rules.\n", out.Pull.Repository, out.Pull.Number, out.TaskRef)
	} else {
		fmt.Printf("%s#%d is already linked to %s.\n", out.Pull.Repository, out.Pull.Number, out.TaskRef)
	}
	return nil
}

// taskUnlinkPR is `conductor task unlink-pr <task> <pull request>`.
func taskUnlinkPR(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("task unlink-pr", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("usage: conductor task unlink-pr <task> <number | owner/repo#N | URL>")
	}
	arg, err := parsePullArg(positional[1])
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
	return runUnlinkPR(ctx, api, ref, positional[0], arg, *asJSON, os.Stdout)
}

func runUnlinkPR(ctx context.Context, api *client.Client, project, task string, arg pullArg, asJSON bool, w io.Writer) error {
	if arg.Repository == "" {
		// A bare number names the pull request among the task's own.
		var out struct {
			Pulls []privacy.PullRequestView `json:"pulls"`
		}
		if err := api.Get(ctx, pullsQuery{State: "all", Task: task, Number: arg.Number}.path(project), &out); err != nil {
			return err
		}
		if len(out.Pulls) != 1 {
			return fmt.Errorf("%s has no pull request #%d linked; conductor pulls --all --task %s lists its pull requests", task, arg.Number, task)
		}
		arg.Repository = out.Pulls[0].Repository
	}
	var out struct {
		Unlinked bool   `json:"unlinked"`
		TaskRef  string `json:"task_ref"`
	}
	if err := api.Delete(ctx, "/v1/tasks/"+url.PathEscape(task)+"/pull-requests/"+arg.Repository+"/"+strconv.Itoa(arg.Number)+
		client.Query("project", project), &out); err != nil {
		return err
	}
	if asJSON {
		return emitTo(w, out)
	}
	fmt.Fprintf(w, "Unlinked %s#%d from %s. It stays unlinked until you link it again.\n", arg.Repository, arg.Number, out.TaskRef)
	return nil
}
