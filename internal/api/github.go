package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/adamburan/conductor/internal/metrics"
	"github.com/adamburan/conductor/internal/secretbox"
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
// Trust boundaries:
//
//   - The app is the machine owner's. Only the owner can create or replace it, and only
//     projects in the owner's organization can be linked to repositories, so a second tenant
//     on a shared control plane can neither swap the app nor read another organization's
//     reservations through it.
//   - A check run is visible to everyone who can read the repository, member or not. It
//     never carries a task title or summary. Private tasks appear as "a private task", a
//     public repository gets no names at all, and a file another attempt has already changed
//     is attributed only for tasks shared at team_artifacts or above.
//
// Replicas: everything the integration must agree on lives in Postgres — the app's
// credentials, pending setups (GitHub's callback can land on any replica), and which check
// run was posted on which commit with what result. Polling is gated by an advisory lock, so
// one replica polls at a time and another takes over if it dies.

// GitHubOptions configures the integration.
type GitHubOptions struct {
	// CredentialsPath is the credentials file conductord used before the app moved into the
	// database. An app found there is imported once, when no app is stored yet.
	CredentialsPath string
	// SecretKey seals the app's private key and secrets before they are stored
	// (github_secrets.go). Without one, an app can be neither set up nor imported.
	SecretKey *secretbox.Source
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

	mu     sync.Mutex
	client *githubapp.Client
	loaded string // fingerprint of the credentials client was built from
	synced time.Time
	// file is the legacy credentials file's content, imported into the database once.
	file      githubapp.Credentials
	fileFound bool
	holder    string

	// sweep remembers, per repository, when closed pull requests were last swept, so the
	// poller pages only through what closed since (github_merge.go).
	sweep pullSweep
}

// credsRefresh bounds how long a replica keeps serving an app another replica has replaced.
const credsRefresh = 15 * time.Second

// pollerLock names the advisory lock that makes one replica the poller.
const pollerLock = "conductor/github-poller"

var githubPolls = metrics.Default.NewCounter("conductor_github_polls_total",
	"GitHub pull request polls, by outcome (ok, error, skipped: another replica polled or holds the poller).", "outcome")

// NewGitHub reads the legacy credentials file and the environment. Until the store is
// attached (Refresh), that is the app it serves; from then on, the database's. A missing
// file is not an error: the integration then serves only its setup flow. A file that exists
// but cannot sign is reported.
func NewGitHub(opts GitHubOptions) (*GitHub, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	host, _ := os.Hostname()
	g := &GitHub{opts: opts, logger: opts.Logger, kick: make(chan struct{}, 1),
		holder: host + ":" + strconv.Itoa(os.Getpid())}
	file, found, err := githubapp.LoadFile(opts.CredentialsPath)
	if err != nil {
		return g, err
	}
	g.file, g.fileFound = file, found
	creds, ok, err := githubapp.Overlay(file, opts.Getenv)
	if err != nil {
		return g, err
	}
	return g, g.use(creds, ok)
}

// use makes creds the app this integration acts as.
func (g *GitHub) use(creds githubapp.Credentials, ok bool) error {
	sum := ""
	if ok {
		data, _ := json.Marshal(creds)
		h := sha256.Sum256(data)
		sum = hex.EncodeToString(h[:])
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sum == g.loaded {
		return nil
	}
	if !ok {
		g.client, g.loaded = nil, ""
		return nil
	}
	c, err := githubapp.New(g.opts.API, creds)
	if err != nil {
		return err
	}
	g.client, g.loaded = c, sum
	return nil
}

// Refresh loads the app from the database, importing the legacy file first if the database
// has none, then applies the environment's overrides. conductord calls it once at startup;
// afterwards the integration refreshes itself at most every credsRefresh, so a replica picks
// up an app another replica set up.
func (g *GitHub) Refresh(ctx context.Context) error { return g.sync(ctx, true) }

func (g *GitHub) sync(ctx context.Context, force bool) error {
	if g.store == nil {
		return nil
	}
	g.mu.Lock()
	if !force && time.Since(g.synced) < credsRefresh {
		g.mu.Unlock()
		return nil
	}
	g.synced = time.Now()
	g.mu.Unlock()

	raw, found, err := g.store.GitHubApp(ctx)
	if err != nil {
		return fmt.Errorf("read github app: %w", err)
	}
	if !found && g.fileFound {
		data, err := sealCredentials(g.opts.SecretKey, g.file)
		if err != nil {
			return fmt.Errorf("import github app: %w", err)
		}
		imported, err := g.store.ImportGitHubApp(ctx, data)
		if err != nil {
			return fmt.Errorf("import github app: %w", err)
		}
		if imported {
			g.logger.Info("github app credentials imported into the database; every replica now serves this app and the file is no longer read",
				"path", g.opts.CredentialsPath)
		}
		if raw, found, err = g.store.GitHubApp(ctx); err != nil {
			return fmt.Errorf("read github app: %w", err)
		}
	}
	var base githubapp.Credentials
	if found {
		var plaintext bool
		if base, plaintext, err = openCredentials(g.opts.SecretKey, raw); err != nil {
			return err
		}
		if plaintext {
			g.reseal(ctx, raw, base)
		}
	}
	creds, ok, err := githubapp.Overlay(base, g.opts.Getenv)
	if err != nil {
		return err
	}
	return g.use(creds, ok)
}

// reseal replaces a row stored before credentials were sealed with its sealed form, unless
// another replica has changed the row since it was read. A failure leaves the plaintext row
// working as before; the next refresh tries again.
func (g *GitHub) reseal(ctx context.Context, old []byte, creds githubapp.Credentials) {
	sealed, err := sealCredentials(g.opts.SecretKey, creds)
	if err == nil {
		var done bool
		if done, err = g.store.ResealGitHubApp(ctx, old, sealed); err == nil && done {
			g.logger.Info("github app secrets in the database are now sealed with this server's secret key")
		}
	}
	if err != nil {
		g.logger.Warn("could not seal the github app's stored secrets; they remain readable to anyone with a database backup", "error", err)
	}
}

// current refreshes from the database if due and returns the client, nil when no app is
// configured. A failed refresh keeps the app already loaded.
func (g *GitHub) current(ctx context.Context) *githubapp.Client {
	if err := g.sync(ctx, false); err != nil {
		g.logger.Warn("github app refresh failed", "error", err)
	}
	return g.appClient()
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

// Kick asks the poller to run soon (after an installation changes, say). Kicks are rate
// limited by the poller, because one of the routes that kicks is unauthenticated.
func (g *GitHub) Kick() {
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

// minKickInterval is how soon after a poll a kick may start another.
const minKickInterval = 30 * time.Second

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
	// The issue write-back runs here too, more often than a poll: a claim should reach its
	// issue in seconds, and finding nothing due costs one query (github_issues.go).
	wb := time.NewTicker(writeBackEvery)
	defer wb.Stop()
	var last time.Time
	// The first poll, and one after a kick, may follow another replica's closely; a timed
	// poll defers to one any replica ran within half an interval.
	minGap := minKickInterval
	for {
		if c := g.current(ctx); c != nil && g.store != nil {
			last = time.Now()
			pollCtx, cancel := context.WithTimeout(ctx, interval)
			outcome, err := g.pollExclusive(pollCtx, minGap)
			cancel()
			githubPolls.Inc(outcome)
			if err != nil && ctx.Err() == nil {
				g.logger.Warn("github poll failed", "error", err)
			}
		}
	wait:
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			minGap = interval / 2
		case <-g.kick:
			if time.Since(last) < minKickInterval {
				goto wait
			}
			minGap = minKickInterval
		case <-wb.C:
			if c := g.current(ctx); c != nil && g.store != nil {
				g.writeBackExclusive(ctx)
			}
			goto wait
		}
	}
}

// pollExclusive polls unless another replica holds the poller lock or polled within minGap,
// and records the outcome in the shared heartbeat that /v1/ready and /v1/github/status read.
func (g *GitHub) pollExclusive(ctx context.Context, minGap time.Duration) (string, error) {
	outcome := "skipped"
	_, err := g.store.TryExclusive(ctx, pollerLock, func(ctx context.Context) error {
		hb, found, err := g.store.GetHeartbeat(ctx, db.ComponentGitHubPoller)
		if err != nil {
			return err
		}
		if found && time.Since(hb.LastRunAt) < minGap {
			return nil
		}
		pollErr := g.pollOnce(ctx)
		outcome = "ok"
		msg := ""
		if pollErr != nil {
			outcome, msg = "error", pollErr.Error()
		}
		if err := g.store.RecordHeartbeat(context.WithoutCancel(ctx), db.ComponentGitHubPoller, g.holder, msg); err != nil {
			g.logger.Warn("record github poll failed", "error", err)
		}
		return pollErr
	})
	if err != nil && outcome == "skipped" {
		outcome = "error"
	}
	return outcome, err
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
				if _, err := g.checkPull(ctx, inst.ID, repo.Owner, repo.Name, repo.Private, pr, projects); err != nil {
					errs = append(errs, fmt.Errorf("%s#%d: %w", repo.FullName, pr.Number, err))
				}
			}
			// Without a webhook, this is how a merge completes its task.
			if err := g.syncPullLifecycle(ctx, inst.ID, repo.Owner, repo.Name, pulls, projects); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", repo.FullName, err))
			}
			// Without a webhook, this is how issues reach their tasks (github_issues.go).
			if err := g.syncRepoIssues(ctx, inst, repo, projects); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// What changed on synced tasks during the pass — a merge just completed, say — is
	// written back in the same pass rather than a tick later.
	if err := g.issueWriteBack(ctx, true); err != nil {
		errs = append(errs, fmt.Errorf("issue write-back: %w", err))
	}
	return errors.Join(errs...)
}

// ownerOrg is the organization of the machine's owner, which the GitHub App serves.
func (g *GitHub) ownerOrg(ctx context.Context) (domain.ID, error) {
	settings, err := g.store.GetServerSettings(ctx)
	if err != nil {
		return "", err
	}
	if settings.LocalOwnerID == "" {
		return "", nil
	}
	owner, err := g.store.GetPrincipal(ctx, settings.LocalOwnerID)
	if err != nil {
		return "", err
	}
	return owner.OrganizationID, nil
}

// projectsFor finds the projects in the owner's organization whose canonical remote is
// owner/repo. Projects of other organizations on the same control plane are never matched.
func (g *GitHub) projectsFor(ctx context.Context, owner, repo string) ([]domain.Project, error) {
	org, err := g.ownerOrg(ctx)
	if err != nil || org == "" {
		return nil, err
	}
	all, err := g.store.ProjectsWithRemote(ctx)
	if err != nil {
		return nil, err
	}
	var out []domain.Project
	for _, p := range all {
		if p.OrganizationID != org {
			continue
		}
		o, r, ok := githubapp.ParseRemote(p.CanonicalRemote)
		if ok && strings.EqualFold(o, owner) && strings.EqualFold(r, repo) {
			out = append(out, p)
		}
	}
	return out, nil
}

// pullOverlap is one line of the report: a file in the pull request that other open work
// has reserved or already changed. TaskRef and Owner are empty when the task may not be
// named where the check is published.
type pullOverlap struct {
	Project  string    `json:"project"`
	File     string    `json:"file"`
	Resource string    `json:"resource"`
	Mode     string    `json:"mode,omitempty"`
	TaskRef  string    `json:"task_ref,omitempty"`
	Owner    string    `json:"owner,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Blocking bool      `json:"blocking"`
	Observed bool      `json:"observed"` // from another attempt's actual diff, not a reservation
	Private  bool      `json:"private,omitempty"`
}

// PullReport is the outcome of checking one pull request.
type PullReport struct {
	Repository string        `json:"repository"`
	Number     int           `json:"number"`
	HeadSHA    string        `json:"head_sha"`
	Branch     string        `json:"branch"`
	Files      int           `json:"files"`
	Public     bool          `json:"public_repository"`
	OwnTask    string        `json:"own_task,omitempty"`
	Projects   []string      `json:"projects"`
	Overlaps   []pullOverlap `json:"overlaps"`
	Conclusion string        `json:"conclusion"`
	Posted     bool          `json:"posted"`

	ownProject string
}

var agentBranch = regexp.MustCompile(`^agent/([^/]+)/attempt-\d+$`)

// checkPull computes the report for one pull request and posts it as a check run when it
// differs from the last one posted for the same commit. private is the repository's
// visibility; when unknown, pass false — a public repository is the stricter assumption.
func (g *GitHub) checkPull(ctx context.Context, installationID int64, owner, repo string, private bool, pr githubapp.PullRequest, projects []domain.Project) (PullReport, error) {
	rep := PullReport{Repository: owner + "/" + repo, Number: pr.Number, HeadSHA: pr.Head.SHA, Branch: pr.Head.Ref, Public: !private}
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
		vis := map[domain.ID]domain.Visibility{}
		for _, fp := range footprints {
			vis[fp.TaskID] = fp.Visibility
		}
		// The pull request's own task is not a conflict with itself. Only a branch in the
		// repository itself counts — a fork's branch name is whatever its author typed — and
		// only an open task's: either the branch an attempt recorded, or Conductor's
		// agent/<ref>/attempt-<n> convention.
		var own domain.ID
		if !pr.FromFork() {
			m := agentBranch.FindStringSubmatch(pr.Head.Ref)
			for _, fp := range footprints {
				if (fp.Branch != "" && fp.Branch == pr.Head.Ref) || (m != nil && fp.TaskRef == m[1]) {
					own, rep.OwnTask, rep.ownProject = fp.TaskID, fp.TaskRef, p.Slug
					break
				}
			}
		}
		named := func(id domain.ID, observed bool) bool {
			v := vis[id]
			if rep.Public || v == domain.VisibilityPrivate || v == "" {
				return false
			}
			return !observed || v == domain.VisibilityTeamArtifacts || v == domain.VisibilitySharedDebug
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
				o := pullOverlap{
					Project: p.Slug, File: strings.TrimPrefix(cf.Requested, "path:"), Resource: cf.ResourceKey,
					Mode: string(cf.HolderMode), Since: cf.HeldSince, Blocking: cf.Outcome.Blocks(),
					Private: vis[cf.HolderTaskID] == domain.VisibilityPrivate,
				}
				if named(cf.HolderTaskID, false) {
					o.TaskRef, o.Owner = cf.HolderTaskRef, cf.HolderOwner
				}
				rep.Overlaps = append(rep.Overlaps, o)
			}
		}
		// Merge risk: files another open attempt has actually changed, reserved or not.
		for _, fp := range footprints {
			if fp.TaskID == own {
				continue
			}
			for _, changed := range fp.ChangedPaths {
				if !inPR[changed] {
					continue
				}
				o := pullOverlap{Project: p.Slug, File: changed, Resource: "path:" + changed, Observed: true,
					Private: fp.Visibility == domain.VisibilityPrivate}
				if named(fp.TaskID, true) {
					o.TaskRef, o.Owner = fp.TaskRef, fp.Owner
				}
				rep.Overlaps = append(rep.Overlaps, o)
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
	sum := sha256.Sum256([]byte(rep.Conclusion + "\x00" + text))
	fingerprint := hex.EncodeToString(sum[:])
	// What was last posted on this commit is in the database, so a restart, a second
	// replica, or the webhook and the poller seeing the same commit do not post it again.
	prev, found, err := g.store.GitHubCheckRun(ctx, rep.Repository, pr.Head.SHA)
	if err != nil {
		return rep, err
	}
	if found && prev.Fingerprint == fingerprint {
		return rep, nil
	}
	run := githubapp.CheckRun{
		HeadSHA: pr.Head.SHA, Conclusion: rep.Conclusion, Title: title, Summary: summary, Text: text,
		ExternalID: "conductor:" + rep.Repository + "#" + strconv.Itoa(pr.Number),
	}
	// A changed result replaces the run on that commit rather than stacking another; a run
	// deleted on GitHub's side is created afresh.
	runID := prev.ID
	if found && prev.ID != 0 {
		err = c.UpdateCheckRun(ctx, installationID, owner, repo, prev.ID, run)
		var apiErr *githubapp.APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			runID, err = c.PostCheckRun(ctx, installationID, owner, repo, run)
		}
	} else {
		runID, err = c.PostCheckRun(ctx, installationID, owner, repo, run)
	}
	if err != nil {
		return rep, err
	}
	if err := g.store.SaveGitHubCheckRun(ctx, rep.Repository, pr.Head.SHA, db.CheckRunRecord{ID: runID, Fingerprint: fingerprint}); err != nil {
		g.logger.Warn("record github check run failed", "repo", rep.Repository, "sha", pr.Head.SHA, "error", err)
	}
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

// forCaller trims a report to the projects a caller belongs to, for an on-demand check
// answered over the API.
func (rep PullReport) forCaller(allowed map[string]bool) PullReport {
	out := rep
	out.Projects, out.Overlaps = nil, nil
	for _, p := range rep.Projects {
		if allowed[p] {
			out.Projects = append(out.Projects, p)
		}
	}
	for _, o := range rep.Overlaps {
		if allowed[o.Project] {
			out.Overlaps = append(out.Overlaps, o)
		}
	}
	if !allowed[rep.ownProject] {
		out.OwnTask = ""
	}
	if out.Overlaps == nil {
		out.Overlaps = []pullOverlap{}
	}
	return out
}

func dedupeOverlaps(in []pullOverlap) []pullOverlap {
	seen := map[string]bool{}
	var out []pullOverlap
	for _, o := range in {
		k := o.Project + "|" + o.File + "|" + o.Resource + "|" + o.TaskRef + "|" + strconv.FormatBool(o.Observed)
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

// who renders the task cell of a report row.
func (o pullOverlap) who() (task, owner string) {
	switch {
	case o.TaskRef != "":
		return o.TaskRef, o.Owner
	case o.Private:
		return "a private task", "—"
	default:
		return "in-flight work", "—"
	}
}

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
	if len(rep.Overlaps) == 0 {
		title = "No overlap with in-flight work"
		summary = fmt.Sprintf("None of the %d file(s) this pull request changes are reserved or being changed by other open work.", rep.Files)
	} else {
		title = fmt.Sprintf("%d file(s) overlap in-flight work", countFiles(rep.Overlaps))
		summary = fmt.Sprintf("%d of this pull request's files are reserved by other open work, and %d are already being changed by it. "+
			"Coordinate before merging: `conductor conflicts` shows each overlap and what to do about it.", countFiles(reserved), countFiles(changed))
	}
	if rep.OwnTask != "" && !rep.Public {
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
			task, owner := o.who()
			fmt.Fprintf(&sb, "| `%s` | `%s` (%s) | %s | %s | %s |\n", md(o.File), md(o.Resource), md(o.Mode), md(task), md(owner), since)
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
			task, owner := o.who()
			fmt.Fprintf(&sb, "| `%s` | %s | %s |\n", md(o.File), md(task), md(owner))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("Conductor reports where work overlaps, never what the work is: task titles and summaries are not shown here")
	if rep.Public {
		sb.WriteString(", and on a public repository neither are task references or owners")
	}
	sb.WriteString(".\n")
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
	m.HandleFunc("GET /v1/projects/{project}/github/issues", auth(s.githubIssuesStatus))
	m.HandleFunc("POST /v1/projects/{project}/github/issues", auth(s.githubIssuesConfigure))
	m.HandleFunc("POST /v1/projects/{project}/github/issues/sync", auth(s.githubIssuesSync))
	m.HandleFunc("GET /v1/tasks/{task}/issue", auth(s.githubTaskIssue))
	// Browser pages and GitHub's own callbacks. None takes a bearer token: the setup page is
	// authorized by its single-use state, the callback by the same state plus GitHub's code,
	// and the webhook by its HMAC signature.
	m.HandleFunc("GET /github/setup", s.githubSetupPage)
	m.HandleFunc("GET /github/callback", s.githubCallback)
	m.HandleFunc("GET /github/installed", s.githubInstalled)
	m.HandleFunc("POST /github/webhook", s.githubWebhook)
}

// isMachineOwner reports whether p owns this machine.
func (s *Server) isMachineOwner(r *http.Request, p domain.Principal) (bool, error) {
	settings, err := s.store.GetServerSettings(r.Context())
	if err != nil {
		return false, err
	}
	return settings.LocalOwnerID != "" && settings.LocalOwnerID == p.ID, nil
}

func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	g := s.github
	out := map[string]any{"configured": g.Configured()}
	sameOrg, err := s.inOwnersOrg(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !sameOrg {
		// Another tenant on this control plane: the app is not theirs to inspect.
		out["available"] = false
		out["linked"] = []map[string]string{}
		s.ok(w, r, http.StatusOK, out)
		return
	}
	out["available"] = true
	out["mode"] = "polling"
	if g.opts.WebhookURL != "" {
		out["mode"] = "webhook"
		out["webhook_url"] = g.opts.WebhookURL
	}
	// The last poll by whichever replica holds the poller.
	if hb, found, err := s.store.GetHeartbeat(r.Context(), db.ComponentGitHubPoller); err == nil && found {
		out["last_poll"] = hb.LastRunAt.UTC()
		if hb.LastError != "" {
			out["last_error"] = hb.LastError
		}
	}
	c := g.current(r.Context())
	out["configured"] = c != nil
	if c != nil {
		creds := c.Credentials()
		out["app"] = map[string]any{"id": creds.AppID, "slug": creds.Slug, "name": creds.Name, "owner": creds.Owner, "html_url": creds.HTMLURL}
		out["install_url"] = creds.InstallURL(g.opts.Web)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		installs, err := c.Installations(ctx)
		if err != nil {
			out["installations_error"] = err.Error()
		} else {
			out["installations"] = installs
			out["permission_gaps"] = g.permissionGaps(ctx, c, installs)
		}
		cancel()
	}
	// Linked projects the caller can see.
	mine, err := s.store.ListProjectsFor(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	linked := []map[string]string{}
	for _, proj := range mine {
		if o, rn, ok := githubapp.ParseRemote(proj.CanonicalRemote); ok {
			entry := map[string]string{"project": proj.Slug, "repository": o + "/" + rn}
			// Which issues the project syncs, if any: "label:<name>", or "all".
			if cfg, found, err := s.store.TrackerConfigFor(r.Context(), proj.ID, db.TrackerGitHub); err != nil {
				s.fail(w, r, err)
				return
			} else if found && cfg.Enabled {
				entry["issues"] = "label:" + cfg.Label
				if cfg.Label == "" {
					entry["issues"] = "all"
				}
			}
			linked = append(linked, entry)
		}
	}
	out["linked"] = linked
	s.ok(w, r, http.StatusOK, out)
}

// githubStartSetup begins the manifest flow and returns the one-time page to open. Only the
// machine's owner may create the app, and replacing an existing one must be said out loud:
// whoever creates it owns it on GitHub, and with it read access to every repository it is
// installed on.
func (s *Server) githubStartSetup(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	owner, err := s.isMachineOwner(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !owner {
		s.fail(w, r, errors.Join(domain.ErrNotPermitted, errors.New("only this machine's owner can connect a GitHub App (the owner is whoever first ran `conductord bootstrap` here)")))
		return
	}
	var body struct {
		Org     string `json:"org"`
		Name    string `json:"name"`
		Replace bool   `json:"replace"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.github.current(r.Context()) != nil && !body.Replace {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "already_configured",
			Error: "a GitHub App is already connected; pass replace (conductor github setup --replace) to create a new one in its place"})
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
	now := time.Now()
	// In the database, not this process: GitHub's callback may reach another replica.
	if err := s.store.CreateGitHubSetup(r.Context(), hashState(state), db.GitHubSetup{
		Org: body.Org, Name: name, By: p.ID, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		s.fail(w, r, err)
		return
	}
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

// hashState is how a setup state is stored: the state itself is a bearer secret in a URL.
func hashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// takeSetup validates a setup state; consume marks it used.
func (g *GitHub) takeSetup(ctx context.Context, state string, consume bool) (db.GitHubSetup, bool) {
	if state == "" || g.store == nil {
		return db.GitHubSetup{}, false
	}
	st, ok, err := g.store.GitHubSetupByState(ctx, hashState(state), consume)
	if err != nil {
		g.logger.Warn("github setup lookup failed", "error", err)
		return db.GitHubSetup{}, false
	}
	return st, ok
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
	st, ok := s.github.takeSetup(r.Context(), r.URL.Query().Get("state"), false)
	if !ok {
		s.renderGitHubPage(w, http.StatusNotFound, githubPageData{
			Heading:    "This setup link has expired",
			Paragraphs: []string{"Setup links work once and for an hour. Start again with:"},
			Code:       "conductor github setup",
		})
		return
	}
	manifest, err := githubapp.Manifest(githubapp.ManifestOptions{
		Name: st.Name, BaseURL: s.githubBase(), WebhookURL: s.github.opts.WebhookURL,
	})
	if err != nil {
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "Could not build the app manifest", Paragraphs: []string{err.Error()}})
		return
	}
	owner := "your GitHub account"
	if st.Org != "" {
		owner = "the " + st.Org + " organisation"
	}
	delivery := "This Conductor is not reachable from the internet, so it will poll GitHub for pull requests every couple of minutes instead of receiving webhooks."
	if s.github.opts.WebhookURL != "" {
		delivery = "GitHub will deliver pull request events to " + s.github.opts.WebhookURL + "."
	}
	s.renderGitHubPage(w, http.StatusOK, githubPageData{
		Heading: "Connect Conductor to GitHub",
		Paragraphs: []string{
			"This creates a GitHub App named “" + st.Name + "” owned by " + owner + ". It can read repository contents and pull requests, and write a “Conductor” check run saying whether a pull request touches files other in-flight work has reserved. " +
				"For projects that turn on issue sync, it also comments on, labels, and closes the issues their tasks came from.",
			"It cannot push code, merge, or change settings. You choose which repositories it sees when you install it on the next screen.",
			delivery,
		},
		Form: &struct{ Action, Manifest, Button string }{
			Action:   githubapp.ManifestFormURL(s.github.opts.Web, st.Org, r.URL.Query().Get("state")),
			Manifest: string(manifest), Button: "Create the GitHub App",
		},
	})
}

func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	st, ok := s.github.takeSetup(r.Context(), state, true)
	if !ok || code == "" {
		s.renderGitHubPage(w, http.StatusBadRequest, githubPageData{
			Heading:    "This setup link is not valid",
			Paragraphs: []string{"It was used already, has expired, or did not come from this Conductor. Start again with:"},
			Code:       "conductor github setup",
		})
		return
	}
	// The person who started the setup must still own the machine when it completes.
	if settings, err := s.store.GetServerSettings(r.Context()); err != nil || settings.LocalOwnerID != st.By {
		s.renderGitHubPage(w, http.StatusForbidden, githubPageData{
			Heading:    "Setup was started by someone who no longer owns this machine",
			Paragraphs: []string{"Ask the machine's owner to start again with:"},
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
	if err := creds.Validate(); err != nil {
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "The app's key did not load", Paragraphs: []string{err.Error()}})
		return
	}
	data, err := sealCredentials(s.github.opts.SecretKey, creds)
	if err == nil {
		err = s.store.SaveGitHubApp(ctx, data)
	}
	if err != nil {
		s.logger.Error("save github app failed", "request_id", requestID(r), "error", err)
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "Could not save the app's credentials",
			Paragraphs: []string{"Conductor could not write them to its database. Start again with:"}, Code: "conductor github setup"})
		return
	}
	if err := s.github.Refresh(ctx); err != nil {
		s.renderGitHubPage(w, http.StatusInternalServerError, githubPageData{Heading: "The app's key did not load", Paragraphs: []string{err.Error()}})
		return
	}
	s.github.Kick()
	s.logger.Info("github app created", "app", creds.Slug, "owner", creds.Owner)
	s.renderGitHubPage(w, http.StatusOK, githubPageData{
		Heading: "Created " + creds.Name,
		Paragraphs: []string{
			"Conductor saved the app's credentials in its database, so every conductord sharing it serves this app.",
			"Last step: install it on the repositories Conductor coordinates.",
		},
		Link: &struct{ Href, Text string }{Href: creds.InstallURL(s.github.opts.Web), Text: "Install on repositories"},
	})
}

func (s *Server) githubInstalled(w http.ResponseWriter, r *http.Request) {
	s.github.Kick() // rate limited by the poller
	s.renderGitHubPage(w, http.StatusOK, githubPageData{
		Heading: "Installed",
		Paragraphs: []string{
			"Conductor will check pull requests on the repositories you chose and post a “Conductor” check on each one.",
			"Each Conductor project must name its repository. From inside the checkout:",
		},
		Code: "conductor github link",
	})
}

// maxWebhookBody bounds a delivery Conductor will read. GitHub allows 25 MB, but the events
// Conductor acts on are a few kilobytes; a larger body is refused rather than buffered.
const maxWebhookBody = 5 << 20

// webhookEvents are the deliveries the handler acts on; anything else is acknowledged
// without reading the body.
var webhookEvents = map[string]bool{"ping": true, "installation": true, "installation_repositories": true,
	"pull_request": true, "issues": true}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	c := s.github.current(r.Context())
	if c == nil {
		http.NotFound(w, r)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	sig := r.Header.Get("X-Hub-Signature-256")
	if sig == "" {
		s.ok(w, r, http.StatusUnauthorized, ErrorBody{Error: "unsigned delivery", Code: "unauthenticated"})
		return
	}
	if !webhookEvents[event] {
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": true})
		return
	}
	// A slow sender cannot hold the connection open indefinitely.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		s.ok(w, r, http.StatusRequestEntityTooLarge, ErrorBody{Error: "delivery too large or too slow", Code: "invalid_argument"})
		return
	}
	if !githubapp.VerifySignature(c.Credentials().WebhookSecret, body, sig) {
		s.ok(w, r, http.StatusUnauthorized, ErrorBody{Error: "bad signature", Code: "unauthenticated"})
		return
	}
	switch event {
	case "ping":
		s.ok(w, r, http.StatusOK, map[string]any{"ok": true})
		return
	case "installation", "installation_repositories":
		s.github.Kick()
		s.ok(w, r, http.StatusAccepted, map[string]any{"ok": true})
		return
	case "issues":
		s.githubIssueEvent(w, r, body)
		return
	}
	var ev struct {
		Action      string                `json:"action"`
		PullRequest githubapp.PullRequest `json:"pull_request"`
		Repository  struct {
			Name    string `json:"name"`
			Private bool   `json:"private"`
			Owner   struct {
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
	owner, repo := ev.Repository.Owner.Login, ev.Repository.Name
	switch ev.Action {
	case "opened", "reopened", "synchronize", "ready_for_review", "edited":
	case "closed":
		s.githubPullClosed(w, r, owner, repo, ev.PullRequest)
		return
	default:
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": ev.Action})
		return
	}
	if ev.PullRequest.Draft || ev.Installation.ID == 0 || !githubapp.ValidRepo(owner, repo) {
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": "draft, not installed, or not a repository"})
		return
	}
	// Answer GitHub at once (it times a delivery out after ten seconds) and check in the
	// background — under the server's lifetime, not the request's (which ends with this
	// response) and not context.Background (which would outlive the store at shutdown), and
	// within a bounded pool. A delivery past the pool is left to the poller.
	started := s.goBackground(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		projects, err := s.github.projectsFor(ctx, owner, repo)
		if err != nil || len(projects) == 0 {
			return
		}
		if err := s.github.linkPulls(ctx, []githubapp.PullRequest{ev.PullRequest}, projects); err != nil {
			s.logger.Warn("github link failed", "repo", owner+"/"+repo, "pr", ev.PullRequest.Number, "error", err)
		}
		if _, err := s.github.checkPull(ctx, ev.Installation.ID, owner, repo, ev.Repository.Private, ev.PullRequest, projects); err != nil {
			s.logger.Warn("github check failed", "repo", owner+"/"+repo, "pr", ev.PullRequest.Number, "error", err)
		}
	})
	if !started {
		webhookDeferred.Inc()
		s.github.Kick()
		s.ok(w, r, http.StatusAccepted, map[string]any{"deferred": ev.PullRequest.Number})
		return
	}
	s.ok(w, r, http.StatusAccepted, map[string]any{"checking": ev.PullRequest.Number})
}

// githubCheckNow checks one pull request on demand: `conductor github check owner/repo#12`.
func (s *Server) githubCheckNow(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	c := s.github.current(r.Context())
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
	if !found || !githubapp.ValidRepo(owner, repo) || body.Number <= 0 {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("give repository as owner/name and a pull request number")))
		return
	}
	projects, err := s.github.projectsFor(r.Context(), owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The caller must belong to at least one project the repository maps to. The check run
	// itself covers every linked project, exactly as the poller and the webhook do — two
	// views of one repository would re-post over each other — but the answer returned here
	// is trimmed to the caller's own projects.
	allowed := map[string]bool{}
	for _, proj := range projects {
		if _, err := s.svc.Authorize(r.Context(), p, proj.ID, domain.RoleContributor); err == nil {
			allowed[proj.Slug] = true
		}
	}
	if len(allowed) == 0 {
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
	info, err := c.RepositoryInfo(ctx, inst, owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pulls, err := c.OpenPullRequests(ctx, inst, owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, pr := range pulls {
		if pr.Number == body.Number {
			rep, err := s.github.checkPull(ctx, inst, owner, repo, info.Private, pr, projects)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			s.ok(w, r, http.StatusOK, rep.forCaller(allowed))
			return
		}
	}
	s.fail(w, r, errors.Join(domain.ErrNotFound, fmt.Errorf("%s#%d is not an open pull request", body.Repository, body.Number)))
}

// githubLink records which repository a project governs. Only projects in the machine
// owner's organization can be linked: the app is theirs, and a project of another tenant
// linked to their repository would read its pull requests and write into its checks.
func (s *Server) githubLink(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleProjectAdmin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	org, err := s.github.ownerOrg(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if org == "" || org != project.OrganizationID {
		s.fail(w, r, errors.Join(domain.ErrNotPermitted, errors.New("this control plane's GitHub App belongs to its owner's organization; projects of other organizations cannot be linked to it")))
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
		if !found || !githubLogin.MatchString(o) || !githubapp.ValidRepo(o, n) {
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
