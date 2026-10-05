package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/githubapp"
)

// The GitHub App integration.
//
// Conductor already knows which files every in-flight task holds. A pull request is the
// moment that knowledge matters most and is least visible: someone is about to merge
// changes to files another person or agent is in the middle of rewriting. With the app
// installed, conductord reads each pull request's changed files and posts a "Conductor"
// check run saying whether any of them are reserved by other open work — on GitHub, where
// the reviewer is already looking.
//
// Setup is two clicks (GitHub's manifest flow, see internal/githubapp): `conductor github
// setup` opens a page with one button that creates the app with exactly the permissions it
// needs, and GitHub hands back the credentials. A laptop GitHub cannot reach polls for pull
// requests instead of receiving webhooks, so the same flow works with no public URL at all.
//
// What reaches GitHub is coordination metadata only — task refs, owner handles, resource
// keys — never a task's title or summary, because a check run is visible to everyone who
// can read the repository, member of the project or not.

// GitHubOptions configures the integration.
type GitHubOptions struct {
	// CredentialsPath is where the app's credentials are kept (0600).
	CredentialsPath string
	// API and Web override api.github.com and github.com (GitHub Enterprise, tests).
	API, Web string
	// BaseURL is how a browser reaches this conductord; GitHub redirects back to it.
	BaseURL string
	// WebhookURL is where GitHub can deliver events. Empty: poll instead.
	WebhookURL string
	// Poll is how often open pull requests are re-checked; 0 means two minutes, negative
	// disables polling.
	Poll   time.Duration
	Logger *slog.Logger
	Getenv func(string) string
}

// GitHub is the running integration.
type GitHub struct {
	opts   GitHubOptions
	store  *db.Store
	logger *slog.Logger
	kick   chan struct{}

	mu       sync.Mutex
	client   *githubapp.Client
	setups   map[string]*githubSetup
	posted   map[string]string // owner/repo@sha → fingerprint of the last check posted
	lastPoll time.Time
	lastErr  string
}

type githubSetup struct {
	org, name string
	by        domain.ID
	expires   time.Time
	used      bool
}

// NewGitHub loads any saved credentials. A missing file is not an error: the integration
// then serves only its setup flow. A file that exists but cannot sign is reported.
func NewGitHub(opts GitHubOptions) (*GitHub, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	g := &GitHub{opts: opts, logger: opts.Logger, kick: make(chan struct{}, 1),
		setups: map[string]*githubSetup{}, posted: map[string]string{}}
	creds, ok, err := githubapp.Load(opts.CredentialsPath, opts.Getenv)
	if err != nil {
		return g, err
	}
	if ok {
		c, err := githubapp.New(opts.API, creds)
		if err != nil {
			return g, err
		}
		g.client = c
	}
	return g, nil
}

// Configured reports whether an app's credentials are loaded.
func (g *GitHub) Configured() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.client != nil
}

func (g *GitHub) appClient() *githubapp.Client {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.client
}

// Kick asks the poller to run now (after an installation changes, say).
func (g *GitHub) Kick() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// Run polls open pull requests until ctx ends. It is cheap when nothing is configured and
// picks the app up the moment the setup callback stores credentials.
func (g *GitHub) Run(ctx context.Context) error {
	interval := g.opts.Poll
	if interval < 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	if interval == 0 {
		interval = 2 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if g.Configured() && g.store != nil {
			pollCtx, cancel := context.WithTimeout(ctx, interval)
			err := g.pollOnce(pollCtx)
			cancel()
			g.mu.Lock()
			g.lastPoll = time.Now().UTC()
			g.lastErr = ""
			if err != nil {
				g.lastErr = err.Error()
			}
			g.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				g.logger.Warn("github poll failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-g.kick:
		}
	}
}

// maxPullsPerRepo bounds one poll's work on a busy repository.
const maxPullsPerRepo = 50

func (g *GitHub) pollOnce(ctx context.Context) error {
	c := g.appClient()
	installs, err := c.Installations(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, inst := range installs {
		repos, err := c.InstallationRepositories(ctx, inst.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("installation %d: %w", inst.ID, err))
			continue
		}
		for _, repo := range repos {
			projects, err := g.projectsFor(ctx, repo.Owner, repo.Name)
			if err != nil {
				return err
			}
			if len(projects) == 0 {
				continue
			}
			pulls, err := c.OpenPullRequests(ctx, inst.ID, repo.Owner, repo.Name)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", repo.FullName, err))
				continue
			}
			for i, pr := range pulls {
				if i >= maxPullsPerRepo {
					break
				}
				if pr.Draft {
					continue
				}
				if _, err := g.checkPull(ctx, inst.ID, repo.Owner, repo.Name, pr, projects); err != nil {
					errs = append(errs, fmt.Errorf("%s#%d: %w", repo.FullName, pr.Number, err))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// projectsFor finds the projects whose canonical remote is owner/repo.
func (g *GitHub) projectsFor(ctx context.Context, owner, repo string) ([]domain.Project, error) {
	all, err := g.store.ProjectsWithRemote(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Project
	for _, p := range all {
		o, r, ok := githubapp.ParseRemote(p.CanonicalRemote)
		if ok && strings.EqualFold(o, owner) && strings.EqualFold(r, repo) {
			out = append(out, p)
		}
	}
	return out, nil
}

// pullOverlap is one line of the report: a file in the pull request that other open work
// has reserved or already changed.
type pullOverlap struct {
	Project  string    `json:"project"`
	File     string    `json:"file"`
	Resource string    `json:"resource"`
	Mode     string    `json:"mode,omitempty"`
	TaskRef  string    `json:"task_ref"`
	Owner    string    `json:"owner"`
	Since    time.Time `json:"since,omitzero"`
	Blocking bool      `json:"blocking"`
	Observed bool      `json:"observed"` // from another attempt's actual diff, not a reservation
}

// PullReport is the outcome of checking one pull request.
type PullReport struct {
	Repository string        `json:"repository"`
	Number     int           `json:"number"`
	HeadSHA    string        `json:"head_sha"`
	Branch     string        `json:"branch"`
	Files      int           `json:"files"`
	OwnTask    string        `json:"own_task,omitempty"`
	Projects   []string      `json:"projects"`
	Overlaps   []pullOverlap `json:"overlaps"`
	Conclusion string        `json:"conclusion"`
	Posted     bool          `json:"posted"`
}

var agentBranch = regexp.MustCompile(`^agent/([^/]+)/attempt-\d+$`)

// checkPull computes the report for one pull request and posts it as a check run when it
// differs from the last one posted for the same commit.
func (g *GitHub) checkPull(ctx context.Context, installationID int64, owner, repo string, pr githubapp.PullRequest, projects []domain.Project) (PullReport, error) {
	rep := PullReport{Repository: owner + "/" + repo, Number: pr.Number, HeadSHA: pr.Head.SHA, Branch: pr.Head.Ref}
	c := g.appClient()
	if c == nil {
		return rep, errors.New("the GitHub App is not configured")
	}
	files, err := c.PullRequestFiles(ctx, installationID, owner, repo, pr.Number)
	if err != nil {
		return rep, err
	}
	rep.Files = len(files)
	inPR := map[string]bool{}
	requests := make([]domain.ScopeRequest, 0, len(files))
	for _, f := range files {
		inPR[f] = true
		requests = append(requests, domain.ScopeRequest{Resource: "path:" + f, Mode: domain.ModeWriteExclusive})
	}

	for _, p := range projects {
		rep.Projects = append(rep.Projects, p.Slug)
		footprints, err := g.store.OpenFootprints(ctx, p.ID)
		if err != nil {
			return rep, err
		}
		// The pull request's own task, if it has one, is not a conflict with itself: match
		// the branch an attempt recorded, or Conductor's agent/<ref>/attempt-<n> convention.
		var own domain.ID
		for _, fp := range footprints {
			if fp.Branch != "" && fp.Branch == pr.Head.Ref {
				own, rep.OwnTask = fp.TaskID, fp.TaskRef
			}
		}
		if own == "" {
			if m := agentBranch.FindStringSubmatch(pr.Head.Ref); m != nil {
				if t, err := g.store.GetTaskByRef(ctx, p.ID, m[1]); err == nil {
					own, rep.OwnTask = t.ID, t.Ref
				}
			}
		}
		if len(requests) > 0 {
			conflicts, err := g.store.CheckScopes(ctx, db.CheckScopesParams{
				ProjectID: p.ID, ExcludeTask: own, Requests: requests,
				Policy: config.ScopePolicyFrom(p.Config),
			})
			if err != nil {
				return rep, err
			}
			for _, cf := range conflicts {
				rep.Overlaps = append(rep.Overlaps, pullOverlap{
					Project: p.Slug, File: strings.TrimPrefix(cf.Requested, "path:"), Resource: cf.ResourceKey,
					Mode: string(cf.HolderMode), TaskRef: cf.HolderTaskRef, Owner: cf.HolderOwner,
					Since: cf.HeldSince, Blocking: cf.Outcome.Blocks(),
				})
			}
		}
		// Merge risk: files another open attempt has actually changed, reserved or not.
		for _, fp := range footprints {
			if fp.TaskID == own {
				continue
			}
			for _, changed := range fp.ChangedPaths {
				if inPR[changed] {
					rep.Overlaps = append(rep.Overlaps, pullOverlap{
						Project: p.Slug, File: changed, Resource: "path:" + changed,
						TaskRef: fp.TaskRef, Owner: fp.Owner, Observed: true,
					})
				}
			}
		}
	}
	rep.Overlaps = dedupeOverlaps(rep.Overlaps)
	rep.Conclusion = "success"
	if len(rep.Overlaps) > 0 {
		// Neutral, not failure: the check informs a reviewer, it does not gate a merge. A
		// repository that wants it to gate can require the check in branch protection and
		// treat neutral as a stop.
		rep.Conclusion = "neutral"
	}

	title, summary, text := renderPullReport(rep)
	fingerprint := rep.Conclusion + "\x00" + text
	key := rep.Repository + "@" + pr.Head.SHA
	g.mu.Lock()
	same := g.posted[key] == fingerprint
	g.mu.Unlock()
	if same {
		return rep, nil
	}
	if err := c.PostCheckRun(ctx, installationID, owner, repo, githubapp.CheckRun{
		HeadSHA: pr.Head.SHA, Conclusion: rep.Conclusion, Title: title, Summary: summary, Text: text,
		ExternalID: "conductor:" + rep.Repository + "#" + strconv.Itoa(pr.Number),
	}); err != nil {
		return rep, err
	}
	g.mu.Lock()
	if len(g.posted) >= maxPostedChecks {
		// Forget everything rather than track recency: the cost is one repeated check run per
		// open pull request, once.
		g.posted = map[string]string{}
	}
	g.posted[key] = fingerprint
	g.mu.Unlock()
	rep.Posted = true
	for _, p := range projects {
		count := 0
		for _, o := range rep.Overlaps {
			if o.Project == p.Slug {
				count++
			}
		}
		_ = g.store.AppendEvent(ctx, p.OrganizationID, p.ID, "", "project", p.ID, "github.pr_checked",
			domain.VisibilityTeamSummary, map[string]any{
				"branch": pr.Head.Ref, "commit_sha": pr.Head.SHA, "outcome": rep.Conclusion, "count": count,
			})
	}
	return rep, nil
}

func dedupeOverlaps(in []pullOverlap) []pullOverlap {
	seen := map[string]bool{}
	var out []pullOverlap
	for _, o := range in {
		k := o.Project + "|" + o.File + "|" + o.TaskRef + "|" + strconv.FormatBool(o.Observed)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, o)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Observed != out[j].Observed {
			return !out[i].Observed
		}
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].TaskRef < out[j].TaskRef
	})
	return out
}

const maxReportRows = 60

// maxPostedChecks bounds the memory of which commits were already checked.
const maxPostedChecks = 20000

// renderPullReport writes the check run's title, summary, and Markdown body.
func renderPullReport(rep PullReport) (title, summary, text string) {
	var reserved, changed []pullOverlap
	for _, o := range rep.Overlaps {
		if o.Observed {
			changed = append(changed, o)
		} else {
			reserved = append(reserved, o)
		}
	}
	tasks := map[string]bool{}
	for _, o := range rep.Overlaps {
		tasks[o.TaskRef] = true
	}
	if len(rep.Overlaps) == 0 {
		title = "No overlap with in-flight work"
		summary = fmt.Sprintf("None of the %d file(s) this pull request changes are reserved or being changed by other open work in %s.",
			rep.Files, strings.Join(rep.Projects, ", "))
	} else {
		title = fmt.Sprintf("Overlaps %d in-flight task(s)", len(tasks))
		summary = fmt.Sprintf("%d of this pull request's files are reserved by other open work, and %d are already being changed by it. "+
			"Coordinate before merging: `conductor conflicts` shows each overlap and what to do about it.", countFiles(reserved), countFiles(changed))
	}
	if rep.OwnTask != "" {
		summary += fmt.Sprintf(" This pull request's own task (%s) is excluded.", rep.OwnTask)
	}
	var sb strings.Builder
	if len(reserved) > 0 {
		sb.WriteString("### Reserved by other open work\n\n| File | Reserved as | Task | Owner | Since |\n|---|---|---|---|---|\n")
		for i, o := range reserved {
			if i == maxReportRows {
				fmt.Fprintf(&sb, "\n_…and %d more._\n", len(reserved)-maxReportRows)
				break
			}
			since := ""
			if !o.Since.IsZero() {
				since = o.Since.UTC().Format("2006-01-02")
			}
			fmt.Fprintf(&sb, "| `%s` | `%s` (%s) | %s | %s | %s |\n", md(o.File), md(o.Resource), md(o.Mode), md(o.TaskRef), md(o.Owner), since)
		}
		sb.WriteString("\n")
	}
	if len(changed) > 0 {
		sb.WriteString("### Already changed by other open work (merge risk)\n\n| File | Task | Owner |\n|---|---|---|\n")
		for i, o := range changed {
			if i == maxReportRows {
				fmt.Fprintf(&sb, "\n_…and %d more._\n", len(changed)-maxReportRows)
				break
			}
			fmt.Fprintf(&sb, "| `%s` | %s | %s |\n", md(o.File), md(o.TaskRef), md(o.Owner))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Conductor reports where work overlaps, never what the work is: task titles and summaries are not shown here.\n")
	return title, summary, sb.String()
}

func countFiles(os []pullOverlap) int {
	seen := map[string]bool{}
	for _, o := range os {
		seen[o.File] = true
	}
	return len(seen)
}

// md keeps a value from breaking a Markdown table cell.
func md(s string) string {
	return strings.NewReplacer("|", "\\|", "`", "'", "\n", " ").Replace(s)
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

func (s *Server) githubRoutes(m *http.ServeMux) {
	if s.github == nil {
		return
	}
	s.github.store = s.store
	auth := s.authenticate
	m.HandleFunc("GET /v1/github/status", auth(s.githubStatus))
	m.HandleFunc("POST /v1/github/setup", auth(s.githubStartSetup))
	m.HandleFunc("POST /v1/github/check", auth(s.githubCheckNow))
	m.HandleFunc("POST /v1/projects/{project}/github", auth(s.githubLink))
	// Browser pages and GitHub's own callbacks. None takes a bearer token: the setup page is
	// authorized by its single-use state, the callback by the same state plus GitHub's code,
	// and the webhook by its HMAC signature.
	m.HandleFunc("GET /github/setup", s.githubSetupPage)
	m.HandleFunc("GET /github/callback", s.githubCallback)
	m.HandleFunc("GET /github/installed", s.githubInstalled)
	m.HandleFunc("POST /github/webhook", s.githubWebhook)
}

// canAdministerMachine: the machine's owner, or an administrator of any project here.
func (s *Server) canAdministerMachine(r *http.Request, p domain.Principal) (bool, error) {
	settings, err := s.store.GetServerSettings(r.Context())
	if err != nil {
		return false, err
	}
	if settings.LocalOwnerID != "" && settings.LocalOwnerID == p.ID {
		return true, nil
	}
	return s.isAdminSomewhere(r, p)
}

func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	g := s.github
	out := map[string]any{"configured": g.Configured()}
	g.mu.Lock()
	out["mode"] = "polling"
	if g.opts.WebhookURL != "" {
		out["mode"] = "webhook"
		out["webhook_url"] = g.opts.WebhookURL
	}
	if !g.lastPoll.IsZero() {
		out["last_poll"] = g.lastPoll
	}
	if g.lastErr != "" {
		out["last_error"] = g.lastErr
	}
	c := g.client
	g.mu.Unlock()
	if c != nil {
		creds := c.Credentials()
		out["app"] = map[string]any{"id": creds.AppID, "slug": creds.Slug, "name": creds.Name, "owner": creds.Owner, "html_url": creds.HTMLURL}
		out["install_url"] = creds.InstallURL(g.opts.Web)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		installs, err := c.Installations(ctx)
		cancel()
		if err != nil {
			out["installations_error"] = err.Error()
		} else {
			out["installations"] = installs
		}
	}
	// Linked projects the caller can see.
	mine, err := s.store.ListProjectsFor(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var linked []map[string]string
	for _, proj := range mine {
		if o, rn, ok := githubapp.ParseRemote(proj.CanonicalRemote); ok {
			linked = append(linked, map[string]string{"project": proj.Slug, "repository": o + "/" + rn})
		}
	}
	if linked == nil {
		linked = []map[string]string{}
	}
	out["linked"] = linked
	s.ok(w, r, http.StatusOK, out)
}

// githubStartSetup begins the manifest flow and returns the one-time page to open.
func (s *Server) githubStartSetup(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	ok, err := s.canAdministerMachine(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.fail(w, r, errors.Join(domain.ErrNotPermitted, errors.New("only the machine's owner or a project administrator can create the GitHub App")))
		return
	}
	var body struct {
		Org  string `json:"org"`
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Org != "" && !githubLogin.MatchString(body.Org) {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("org is not a GitHub organisation name")))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = defaultAppName()
	}
	if len(name) > 34 {
		name = name[:34]
	}
	state := randomState()
	g := s.github
	g.mu.Lock()
	now := time.Now()
	for k, v := range g.setups {
		if now.After(v.expires) {
			delete(g.setups, k)
		}
	}
	g.setups[state] = &githubSetup{org: body.Org, name: name, by: p.ID, expires: now.Add(time.Hour)}
	g.mu.Unlock()
	s.ok(w, r, http.StatusCreated, map[string]any{
		"setup_url":  strings.TrimRight(s.githubBase(), "/") + "/github/setup?state=" + state,
		"expires_at": now.Add(time.Hour).UTC(), "name": name, "org": body.Org,
		"webhooks": g.opts.WebhookURL != "",
	})
}

var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

func defaultAppName() string {
	host, _ := os.Hostname()
	host = strings.Split(host, ".")[0]
	if len(host) > 16 {
		host = host[:16]
	}
	suffix := randomState()[:4]
	if host == "" {
		return "Conductor " + suffix
	}
	return "Conductor " + host + " " + suffix
}

func randomState() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) githubBase() string {
	if s.github.opts.BaseURL != "" {
		return s.github.opts.BaseURL
	}
	return s.self
}

// takeSetup validates a setup state; consume marks it used.
func (g *GitHub) takeSetup(state string, consume bool) (*githubSetup, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.setups[state]
	if !ok || st.used || time.Now().After(st.expires) {
		return nil, false
	}
	if consume {
		st.used = true
	}
	return st, true
}

var githubPage = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Conductor · GitHub</title><link rel="stylesheet" href="/static/app.css"></head>
<body><div class="connect"><div class="card"><div class="body">
<div class="brand"><span class="name">Conductor</span></div>
<h2>{{.Heading}}</h2>
{{range .Paragraphs}}<p class="muted">{{.}}</p>{{end}}
{{if .Form}}<form method="post" action="{{.Form.Action}}">
<input type="hidden" name="manifest" value="{{.Form.Manifest}}">
<div class="btn-row"><button class="btn primary" type="submit">{{.Form.Button}}</button></div>
</form>{{end}}
{{if .Link}}<div class="btn-row"><a class="btn primary" href="{{.Link.Href}}">{{.Link.Text}}</a></div>{{end}}
{{if .Code}}<pre>{{.Code}}</pre>{{end}}
</div></div></div></body></html>`))

type githubPageData struct {
	Heading    string
	Paragraphs []string
	Form       *struct{ Action, Manifest, Button string }
	Link       *struct{ Href, Text string }
	Code       string
}

func (s *Server) renderGitHubPage(w http.ResponseWriter, status int, d githubPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = githubPage.Execute(w, d)
}

func (s *Server) githubSetupPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.github.takeSetup(r.URL.Query().Get("state"), false)
	if !ok {
		s.renderGitHubPage(w, http.StatusNotFound, githubPageData{
			Heading:    "This setup link has expired",
			Paragraphs: []string{"Setup links work once and for an hour. Start again with:"},
			Code:       "conductor github setup",
		})
		return
	}
	manifest, err := githubapp.Manifest(githubapp.ManifestOptions{
		Name: st.name, BaseURL: s.githubBase(), WebhookURL: s.github.opts.WebhookURL,
	})
	if err != nil {
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "Could not build the app manifest", Paragraphs: []string{err.Error()}})
		return
	}
	owner := "your GitHub account"
	if st.org != "" {
		owner = "the " + st.org + " organisation"
	}
	delivery := "This Conductor is not reachable from the internet, so it will poll GitHub for pull requests every couple of minutes instead of receiving webhooks."
	if s.github.opts.WebhookURL != "" {
		delivery = "GitHub will deliver pull request events to " + s.github.opts.WebhookURL + "."
	}
	s.renderGitHubPage(w, http.StatusOK, githubPageData{
		Heading: "Connect Conductor to GitHub",
		Paragraphs: []string{
			"This creates a GitHub App named “" + st.name + "” owned by " + owner + ". It can read repository contents and pull requests, and write one thing: a “Conductor” check run saying whether a pull request touches files other in-flight work has reserved.",
			"It cannot push code, merge, or change settings. You choose which repositories it sees when you install it on the next screen.",
			delivery,
		},
		Form: &struct{ Action, Manifest, Button string }{
			Action:   githubapp.ManifestFormURL(s.github.opts.Web, st.org, r.URL.Query().Get("state")),
			Manifest: string(manifest), Button: "Create the GitHub App",
		},
	})
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if _, ok := s.github.takeSetup(state, true); !ok || code == "" {
		s.renderGitHubPage(w, http.StatusBadRequest, githubPageData{
			Heading:    "This setup link is not valid",
			Paragraphs: []string{"It was used already, has expired, or did not come from this Conductor. Start again with:"},
			Code:       "conductor github setup",
		})
		return
	}
	anon, _ := githubapp.New(s.github.opts.API, githubapp.Credentials{})
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	creds, err := anon.ConvertManifest(ctx, code)
	if err != nil {
		s.renderGitHubPage(w, http.StatusBadGateway, githubPageData{Heading: "GitHub did not hand over the app", Paragraphs: []string{err.Error()}})
		return
	}
	if err := githubapp.Save(s.github.opts.CredentialsPath, creds); err != nil {
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "Could not save the app's credentials", Paragraphs: []string{err.Error()}})
		return
	}
	client, err := githubapp.New(s.github.opts.API, creds)
	if err != nil {
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "The app's key did not load", Paragraphs: []string{err.Error()}})
		return
	}
	s.github.mu.Lock()
	s.github.client = client
	s.github.mu.Unlock()
	s.github.Kick()
	s.logger.Info("github app created", "app", creds.Slug, "owner", creds.Owner)
	s.renderGitHubPage(w, http.StatusOK, githubPageData{
		Heading: "Created " + creds.Name,
		Paragraphs: []string{
			"Conductor saved the app's credentials on this machine (" + s.github.opts.CredentialsPath + ", readable only by you).",
			"Last step: install it on the repositories Conductor coordinates.",
		},
		Link: &struct{ Href, Text string }{Href: creds.InstallURL(s.github.opts.Web), Text: "Install on repositories"},
	})
}

func (s *Server) githubInstalled(w http.ResponseWriter, r *http.Request) {
	s.github.Kick()
	s.renderGitHubPage(w, http.StatusOK, githubPageData{
		Heading: "Installed",
		Paragraphs: []string{
			"Conductor will check pull requests on the repositories you chose and post a “Conductor” check on each one.",
			"Each Conductor project must name its repository. From inside the checkout:",
		},
		Code: "conductor github link",
	})
}

// maxWebhookBody is GitHub's own cap on a delivery.
const maxWebhookBody = 25 << 20

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	c := s.github.appClient()
	if c == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil || len(body) > maxWebhookBody {
		s.ok(w, r, http.StatusRequestEntityTooLarge, ErrorBody{Error: "delivery too large", Code: "invalid_argument"})
		return
	}
	if !githubapp.VerifySignature(c.Credentials().WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		s.ok(w, r, http.StatusUnauthorized, ErrorBody{Error: "bad signature", Code: "unauthenticated"})
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		s.ok(w, r, http.StatusOK, map[string]any{"ok": true})
		return
	case "installation", "installation_repositories":
		s.github.Kick()
		s.ok(w, r, http.StatusAccepted, map[string]any{"ok": true})
		return
	case "pull_request":
	default:
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": true})
		return
	}
	var ev struct {
		Action      string                `json:"action"`
		PullRequest githubapp.PullRequest `json:"pull_request"`
		Repository  struct {
			Name  string `json:"name"`
			Owner struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		s.ok(w, r, http.StatusBadRequest, ErrorBody{Error: "not a pull_request payload", Code: "invalid_argument"})
		return
	}
	switch ev.Action {
	case "opened", "reopened", "synchronize", "ready_for_review", "edited":
	default:
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": ev.Action})
		return
	}
	if ev.PullRequest.Draft || ev.Installation.ID == 0 {
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": "draft or not installed"})
		return
	}
	owner, repo := ev.Repository.Owner.Login, ev.Repository.Name
	// Answer GitHub at once (it times a delivery out after ten seconds) and check in the
	// background.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		projects, err := s.github.projectsFor(ctx, owner, repo)
		if err != nil || len(projects) == 0 {
			return
		}
		if _, err := s.github.checkPull(ctx, ev.Installation.ID, owner, repo, ev.PullRequest, projects); err != nil {
			s.logger.Warn("github check failed", "repo", owner+"/"+repo, "pr", ev.PullRequest.Number, "error", err)
		}
	}()
	s.ok(w, r, http.StatusAccepted, map[string]any{"checking": ev.PullRequest.Number})
}

// githubCheckNow checks one pull request on demand: `conductor github check owner/repo#12`.
func (s *Server) githubCheckNow(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	c := s.github.appClient()
	if c == nil {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_configured", Error: "no GitHub App is configured; run `conductor github setup`"})
		return
	}
	var body struct {
		Repository string `json:"repository"`
		Number     int    `json:"number"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	owner, repo, found := strings.Cut(body.Repository, "/")
	if !found || owner == "" || repo == "" || body.Number <= 0 {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("give repository as owner/name and a pull request number")))
		return
	}
	projects, err := s.github.projectsFor(r.Context(), owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The caller must belong to at least one project the repository maps to. The check itself
	// covers every linked project, exactly as the poller and the webhook do: a check run
	// belongs to the repository, not to whoever asked, and two views of it would re-post over
	// each other.
	allowed := false
	for _, proj := range projects {
		if _, err := s.svc.Authorize(r.Context(), p, proj.ID, domain.RoleContributor); err == nil {
			allowed = true
			break
		}
	}
	if !allowed {
		s.fail(w, r, errors.Join(domain.ErrNotFound, fmt.Errorf("no project you belong to is linked to %s; run `conductor github link` in its checkout", body.Repository)))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	inst, err := c.RepoInstallation(ctx, owner, repo)
	if err != nil {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_installed", Error: "the GitHub App is not installed on " + body.Repository + ": " + err.Error()})
		return
	}
	pulls, err := c.OpenPullRequests(ctx, inst, owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, pr := range pulls {
		if pr.Number == body.Number {
			rep, err := s.github.checkPull(ctx, inst, owner, repo, pr, projects)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			s.ok(w, r, http.StatusOK, rep)
			return
		}
	}
	s.fail(w, r, errors.Join(domain.ErrNotFound, fmt.Errorf("%s#%d is not an open pull request", body.Repository, body.Number)))
}

// githubLink records which repository a project governs.
func (s *Server) githubLink(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleProjectAdmin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Repository string `json:"repository"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	remote := strings.TrimSpace(body.Repository)
	owner, repo, ok := githubapp.ParseRemote(remote)
	if !ok {
		// Also accept the short owner/name form.
		o, n, found := strings.Cut(remote, "/")
		if !found || !githubLogin.MatchString(o) || n == "" || strings.Contains(n, "/") {
			s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("repository must be owner/name or a GitHub remote URL")))
			return
		}
		owner, repo = o, n
		remote = "https://github.com/" + owner + "/" + repo
	}
	if err := s.store.SetProjectRemote(r.Context(), project.ID, remote); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "github.linked", "project", project.ID, nil)
	s.github.Kick()
	s.ok(w, r, http.StatusOK, map[string]any{"project": project.Slug, "repository": owner + "/" + repo, "remote": remote})
}
