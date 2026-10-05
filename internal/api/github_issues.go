package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/githubapp"
	"github.com/adamburan/conductor/internal/metrics"
	"github.com/adamburan/conductor/internal/tracker"
)

// GitHub Issues sync: the GitHub adapter of internal/tracker.
//
// A project opts in (`conductor github issues enable`). From then on its linked repository's
// open issues carrying the project's label become tasks, an issue's edits, closing, and
// reopening reach its task, and the task's claim and completion are written back to the issue.
// internal/tracker holds the rules; this file reads issues from GitHub and performs the
// write-back through the app.
//
// Delivery is the pull request integration's: `issues` webhooks when GitHub can reach this
// conductord, and the poller (under the same advisory lock, so one replica does it) for
// everything else. The poller lists only issues updated since its last pass, with a
// conditional request, so an idle repository costs one 304 per pass, which GitHub does not
// count against the rate limit. The write-back runs on the poller's goroutine too, every
// writeBackEvery, under the same lock: two replicas never comment on the same claim.

const (
	// maxIssuePages bounds one listing to a few hundred issues per repository; a larger
	// backlog is read over several passes, oldest change first.
	maxIssuePages = 3
	// writeBackBatch bounds how many tasks one write-back pass handles, so a burst of changes
	// (a bulk import being claimed) is spread over passes rather than sent to GitHub at once.
	writeBackBatch = 30
	// writeBackEvery is how often the poller's goroutine looks for write-back that is due.
	// Looking costs one query; GitHub is called only for tasks that changed.
	writeBackEvery = 15 * time.Second
)

var issueWriteBacks = metrics.Default.NewCounter("conductor_github_issue_writebacks_total",
	"Tasks whose GitHub issue the write-back updated, by outcome (ok, error, forbidden: the installation lacks issues: write, rate_limited).", "outcome")

// githubIssueRemote is tracker.Remote for one repository's issues, through one installation.
type githubIssueRemote struct {
	c           *githubapp.Client
	inst        int64
	owner, repo string
}

func (r githubIssueRemote) number(key string) (int, error) {
	o, rp, n, ok := githubapp.ParseIssueKey(key)
	if !ok || !strings.EqualFold(o, r.owner) || !strings.EqualFold(rp, r.repo) {
		return 0, fmt.Errorf("%q is not an issue of %s/%s", key, r.owner, r.repo)
	}
	return n, nil
}

func (r githubIssueRemote) Comment(ctx context.Context, key, body string) error {
	n, err := r.number(key)
	if err != nil {
		return err
	}
	return r.c.CommentOnIssue(ctx, r.inst, r.owner, r.repo, n, body)
}

func (r githubIssueRemote) SetLabel(ctx context.Context, key, label string, present bool) error {
	n, err := r.number(key)
	if err != nil {
		return err
	}
	if present {
		return r.c.AddIssueLabel(ctx, r.inst, r.owner, r.repo, n, label)
	}
	return r.c.RemoveIssueLabel(ctx, r.inst, r.owner, r.repo, n, label)
}

func (r githubIssueRemote) IsOpen(ctx context.Context, key string) (bool, error) {
	n, err := r.number(key)
	if err != nil {
		return false, err
	}
	is, err := r.c.GetIssue(ctx, r.inst, r.owner, r.repo, n)
	return is.State == "open", err
}

func (r githubIssueRemote) Close(ctx context.Context, key string) error {
	n, err := r.number(key)
	if err != nil {
		return err
	}
	return r.c.CloseIssue(ctx, r.inst, r.owner, r.repo, n)
}

// Shareable allows a pull request of the issue's own repository: everyone who can read the
// issue can read it. A pull request elsewhere (a private repository, say) is not linked.
func (r githubIssueRemote) Shareable(key, u string) bool {
	o, rp, _, ok := parsePullURL(u)
	return ok && strings.HasPrefix(u, "https://") && strings.EqualFold(o, r.owner) && strings.EqualFold(rp, r.repo)
}

// issueItem is a GitHub issue as the tracker engine reads it.
func issueItem(owner, repo string, public bool, is githubapp.Issue) tracker.Item {
	return tracker.Item{
		Tracker: db.TrackerGitHub, Key: githubapp.IssueKey(owner, repo, is.Number), URL: is.HTMLURL,
		Title: is.Title, Body: is.Body, Open: is.State == "open", Labels: is.LabelNames(),
		UpdatedAt: is.UpdatedAt, Public: public,
	}
}

// issueSyncReport counts what one import pass did.
type issueSyncReport struct {
	Repository  string `json:"repository"`
	Seen        int    `json:"seen"`
	Created     int    `json:"created"`
	Linked      int    `json:"linked"`
	Updated     int    `json:"updated"`
	Cancelled   int    `json:"cancelled"`
	Revived     int    `json:"revived"`
	NotModified bool   `json:"not_modified,omitempty"`
	More        bool   `json:"more,omitempty"`
}

func (rep *issueSyncReport) add(res db.TrackerResult) {
	switch {
	case res.Created:
		rep.Created++
	case res.Adopted:
		rep.Linked++
	case len(res.Updated) > 0:
		rep.Updated++
	}
	if res.From != res.Status {
		switch res.Status {
		case domain.TaskCancelled:
			rep.Cancelled++
		case domain.TaskReady:
			rep.Revived++
		}
	}
}

// missingIssuesPermission says what to do about an installation without the issues
// permission, in the words `conductor github status` and the project's sync status show.
func missingIssuesPermission(inst githubapp.Installation, need string) string {
	where := "the installation on " + inst.Account
	if inst.HTMLURL != "" {
		where += " (" + inst.HTMLURL + ")"
	}
	return "the GitHub App cannot " + need + " issues here: accept its new \"issues: write\" permission on " + where +
		"; `conductor github status` says what to click"
}

// syncRepoIssues is the poller's issue step for one repository: an import pass for each
// linked project that enabled issue sync.
func (g *GitHub) syncRepoIssues(ctx context.Context, inst githubapp.Installation, repo githubapp.Repository, projects []domain.Project) error {
	var errs []error
	for _, p := range projects {
		cfg, found, err := g.store.TrackerConfigFor(ctx, p.ID, db.TrackerGitHub)
		if err != nil {
			return err
		}
		if !found || !cfg.Enabled {
			continue
		}
		if !githubapp.Grants(inst.Permissions, "issues", "read") {
			// Reported, not retried against GitHub: every call would be a 403 until someone
			// accepts the permission, and the installation list says when they have.
			if err := g.store.RecordTrackerPoll(ctx, p.ID, db.TrackerGitHub, db.TrackerPoll{
				InstallationID: inst.ID, Err: missingIssuesPermission(inst, "read")}); err != nil {
				return err
			}
			continue
		}
		if _, err := g.importIssues(ctx, inst.ID, repo.Owner, repo.Name, !repo.Private, p, cfg); err != nil {
			errs = append(errs, fmt.Errorf("%s issues: %w", repo.FullName, err))
		}
	}
	return errors.Join(errs...)
}

// importIssues reads the issues of owner/repo updated since the project's resume point and
// applies each to the project. It is idempotent: every issue is matched to its task by its
// external_ref, so reading one again changes nothing.
func (g *GitHub) importIssues(ctx context.Context, instID int64, owner, repo string, public bool, p domain.Project, cfg db.TrackerConfig) (issueSyncReport, error) {
	rep := issueSyncReport{Repository: owner + "/" + repo}
	c := g.appClient()
	if c == nil {
		return rep, errors.New("the GitHub App is not configured")
	}
	q := githubapp.IssueQuery{ETag: cfg.ETag, MaxPages: maxIssuePages}
	if cfg.Cursor != nil {
		q.Since = *cfg.Cursor
	}
	listing, err := c.ListIssues(ctx, instID, owner, repo, q)
	if err != nil {
		msg := err.Error()
		var apiErr *githubapp.APIError
		if errors.As(err, &apiErr) && apiErr.Forbidden() {
			msg = missingIssuesPermission(githubapp.Installation{ID: instID, Account: owner}, "read")
		}
		_ = g.store.RecordTrackerPoll(ctx, p.ID, db.TrackerGitHub, db.TrackerPoll{InstallationID: instID, Err: msg})
		return rep, err
	}
	rep.NotModified, rep.More = listing.NotModified, listing.Truncated
	cursor := cfg.Cursor
	var conflicts []string
	for _, is := range listing.Issues {
		if !is.IsPullRequest() {
			rep.Seen++
			res, err := tracker.Observe(ctx, g.store, cfg, p, issueItem(owner, repo, public, is))
			switch {
			case errors.Is(err, domain.ErrConflict):
				// A decision this issue cannot take (reviving its task while another open task
				// names the issue): it is reported, and the rest of the backlog still syncs.
				conflicts = append(conflicts, fmt.Sprintf("%s#%d: %v", rep.Repository, is.Number, err))
			case err != nil:
				_ = g.store.RecordTrackerPoll(ctx, p.ID, db.TrackerGitHub, db.TrackerPoll{InstallationID: instID,
					Err: fmt.Sprintf("%s#%d: %v", rep.Repository, is.Number, err)})
				return rep, err
			default:
				rep.add(res)
			}
		}
		// Pull requests advance the resume point too: they are part of the same listing. It is
		// kept at the database's precision, so an unchanged one compares equal after a
		// round trip.
		if at := is.UpdatedAt.Truncate(time.Microsecond); cursor == nil || at.After(*cursor) {
			cursor = &at
		}
	}
	etag := listing.ETag
	if cursor != nil && (cfg.Cursor == nil || !cursor.Equal(*cfg.Cursor)) {
		// The next listing starts from the new resume point: a different request, which the
		// ETag of this one says nothing about.
		etag = ""
	}
	if err := g.store.RecordTrackerPoll(ctx, p.ID, db.TrackerGitHub, db.TrackerPoll{
		InstallationID: instID, Cursor: cursor, ETag: etag}); err != nil {
		return rep, err
	}
	if len(conflicts) > 0 {
		// Recorded after the success above, which clears the error, so it is what status shows.
		if err := g.store.RecordTrackerPoll(ctx, p.ID, db.TrackerGitHub, db.TrackerPoll{
			Err: strings.Join(conflicts, "; ")}); err != nil {
			return rep, err
		}
	}
	if rep.Created+rep.Linked+rep.Updated+rep.Cancelled+rep.Revived > 0 {
		g.logger.Info("github issues synced", "project", p.Slug, "repository", rep.Repository,
			"created", rep.Created, "linked", rep.Linked, "updated", rep.Updated,
			"cancelled", rep.Cancelled, "revived", rep.Revived)
	}
	return rep, nil
}

// writeBackExclusive runs a write-back pass unless another replica holds the poller.
func (g *GitHub) writeBackExclusive(ctx context.Context) {
	if g.store == nil || g.appClient() == nil {
		return
	}
	_, err := g.store.TryExclusive(ctx, pollerLock, func(ctx context.Context) error {
		return g.issueWriteBack(ctx, false)
	})
	if err != nil && ctx.Err() == nil {
		g.logger.Warn("github issue write-back failed", "error", err)
	}
}

// issueWriteBack tells each synced issue whose task changed what tracker.PlanWriteBack says
// it should hear. A rate limit ends the pass (the client then refuses calls until GitHub's
// wait is over); an installation without issues: write stops that project's write-back and
// says why on its sync status. Such a project is retried only when retryFailing is set — by
// the poll, every couple of minutes — not on every tick, which would ask GitHub for the same
// refusal four times a minute. Callers hold the poller lock.
func (g *GitHub) issueWriteBack(ctx context.Context, retryFailing bool) error {
	c := g.appClient()
	if c == nil || g.store == nil {
		return nil
	}
	// The app is the machine owner's, and so is every project that may sync through it
	// (githubIssuesConfigure); a project whose organization no longer owns the machine is
	// not written back for.
	org, err := g.ownerOrg(ctx)
	if err != nil || org == "" {
		return err
	}
	due, err := g.store.TrackerWriteBackDue(ctx, org, db.TrackerGitHub, writeBackBatch, retryFailing)
	if err != nil || len(due) == 0 {
		return err
	}
	installs := map[string]int64{}
	blocked := map[domain.ID]bool{}
	cleared := map[domain.ID]bool{}
	var errs []error
	for _, t := range due {
		if blocked[t.Link.ProjectID] {
			continue
		}
		_, key, _ := tracker.ParseRef(t.Link.ExternalRef)
		owner, repo, _, ok := githubapp.ParseIssueKey(key)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: not a GitHub issue reference", t.Link.ExternalRef))
			continue
		}
		inst := t.InstallationID
		if inst == 0 {
			repoKey := strings.ToLower(owner + "/" + repo)
			if inst = installs[repoKey]; inst == 0 {
				if inst, err = c.RepoInstallation(ctx, owner, repo); err != nil {
					errs = append(errs, fmt.Errorf("%s/%s: %w", owner, repo, err))
					blocked[t.Link.ProjectID] = true
					continue
				}
				installs[repoKey] = inst
			}
		}
		err := tracker.WriteBack(ctx, g.store, t, githubIssueRemote{c: c, inst: inst, owner: owner, repo: repo})
		var apiErr *githubapp.APIError
		switch {
		case err == nil:
			issueWriteBacks.Inc("ok")
			if !cleared[t.Link.ProjectID] {
				cleared[t.Link.ProjectID] = true
				_ = g.store.RecordTrackerWriteBackError(ctx, t.Link.ProjectID, db.TrackerGitHub, "")
			}
		case errors.As(err, &apiErr) && apiErr.RateLimited():
			issueWriteBacks.Inc("rate_limited")
			return errors.Join(append(errs, err)...)
		case errors.As(err, &apiErr) && apiErr.Forbidden():
			issueWriteBacks.Inc("forbidden")
			blocked[t.Link.ProjectID] = true
			_ = g.store.RecordTrackerWriteBackError(ctx, t.Link.ProjectID, db.TrackerGitHub,
				missingIssuesPermission(githubapp.Installation{ID: inst, Account: owner}, "write to"))
			errs = append(errs, fmt.Errorf("%s: %w", t.Link.ExternalRef, err))
		default:
			issueWriteBacks.Inc("error")
			errs = append(errs, fmt.Errorf("%s: %w", t.Link.ExternalRef, err))
		}
	}
	return errors.Join(errs...)
}

// githubIssueActions are the issues deliveries that can change a task.
var githubIssueActions = map[string]bool{"opened": true, "edited": true, "closed": true, "reopened": true,
	"labeled": true, "unlabeled": true}

// githubIssueEvent handles an issues delivery. Like a pull request's close it is applied
// before answering: it is a few database statements and no call to GitHub, and GitHub's
// delivery log then shows what it did.
func (s *Server) githubIssueEvent(w http.ResponseWriter, r *http.Request, body []byte) {
	var ev struct {
		Action     string          `json:"action"`
		Issue      githubapp.Issue `json:"issue"`
		Repository struct {
			Name    string `json:"name"`
			Private bool   `json:"private"`
			Owner   struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		s.ok(w, r, http.StatusBadRequest, ErrorBody{Error: "not an issues payload", Code: "invalid_argument"})
		return
	}
	owner, repo := ev.Repository.Owner.Login, ev.Repository.Name
	if !githubIssueActions[ev.Action] || ev.Issue.Number <= 0 || ev.Issue.IsPullRequest() || !githubapp.ValidRepo(owner, repo) {
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": ev.Action})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	projects, err := s.github.projectsFor(ctx, owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	results := []db.TrackerResult{}
	for _, p := range projects {
		cfg, found, err := s.store.TrackerConfigFor(ctx, p.ID, db.TrackerGitHub)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !found || !cfg.Enabled {
			continue
		}
		res, err := tracker.Observe(ctx, s.store, cfg, p, issueItem(owner, repo, !ev.Repository.Private, ev.Issue))
		if err != nil {
			s.logger.Warn("github issue sync failed", "repo", owner+"/"+repo, "issue", ev.Issue.Number, "error", err)
			s.fail(w, r, err)
			return
		}
		if res.TaskID != "" {
			results = append(results, res)
		}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"issue": ev.Issue.Number, "tasks": results})
}

// ---------------------------------------------------------------------------
// Permissions
// ---------------------------------------------------------------------------

// permissionGap is a permission the app asks for that GitHub has not granted yet, and what
// to click to grant it.
type permissionGap struct {
	Scope   string `json:"scope"` // "app", or the account an installation is on
	Missing string `json:"missing"`
	Fix     string `json:"fix"`
	URL     string `json:"url,omitempty"`
}

// permissionGaps compares the issues permission issue sync needs with what the app and each
// installation hold. An app created before Conductor asked for it lacks it at the app level:
// GitHub applies a manifest's permissions only when it creates the app, so the owner raises
// them in the app's settings. Every installation must then accept the new permission, which
// GitHub asks its owner to do on the installation's settings page. Until both happen issue
// sync cannot work, and saying so here beats a sync that fails without explanation.
func (g *GitHub) permissionGaps(ctx context.Context, c *githubapp.Client, installs []githubapp.Installation) []permissionGap {
	gaps := []permissionGap{}
	const need = "issues: write"
	if info, err := c.App(ctx); err == nil {
		perms := map[string]string{}
		if raw, ok := info["permissions"].(map[string]any); ok {
			for k, v := range raw {
				perms[k], _ = v.(string)
			}
		}
		if !githubapp.Grants(perms, "issues", "write") {
			gaps = append(gaps, permissionGap{Scope: "app", Missing: need, URL: g.appSettingsURL(c, info),
				Fix: "The app was created before Conductor asked for Issues access, which issue sync needs. " +
					"Open the app's settings, set Issues to \"Read and write\" under Repository permissions, " +
					"subscribe to the Issues event, and save; then accept the change on each installation."})
		}
	}
	for _, inst := range installs {
		if inst.Permissions == nil || githubapp.Grants(inst.Permissions, "issues", "write") {
			continue
		}
		gaps = append(gaps, permissionGap{Scope: inst.Account, Missing: need, URL: inst.HTMLURL,
			Fix: "GitHub is waiting for " + inst.Account + " to accept the app's new permission: open the installation's " +
				"settings and accept the requested permissions (GitHub shows a \"Review request\" banner)."})
	}
	return gaps
}

// web is GitHub's web address: github.com, or the Enterprise host configured.
func (g *GitHub) web() string {
	if g.opts.Web != "" {
		return strings.TrimRight(g.opts.Web, "/")
	}
	return githubapp.DefaultWeb
}

// appSettingsURL is where the app's permissions are edited, under its owner's settings.
func (g *GitHub) appSettingsURL(c *githubapp.Client, info map[string]any) string {
	web := g.web()
	slug := c.Credentials().Slug
	if slug == "" {
		return ""
	}
	if owner, ok := info["owner"].(map[string]any); ok && owner["type"] == "Organization" {
		if login, _ := owner["login"].(string); githubLogin.MatchString(login) {
			return web + "/organizations/" + login + "/settings/apps/" + slug + "/permissions"
		}
	}
	return web + "/settings/apps/" + slug + "/permissions"
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

// issueSyncView is a project's issue sync settings and state. It carries counts and settings,
// never an issue's or a task's text.
type issueSyncView struct {
	Project          string            `json:"project"`
	Repository       string            `json:"repository,omitempty"`
	Enabled          bool              `json:"enabled"`
	Label            string            `json:"label"`
	All              bool              `json:"all"`
	InProgressLabel  string            `json:"in_progress_label"`
	PublicVisibility domain.Visibility `json:"public_visibility"`
	LastSyncAt       *time.Time        `json:"last_sync_at,omitempty"`
	LastError        string            `json:"last_error,omitempty"`
	WriteBackError   string            `json:"writeback_error,omitempty"`
	Imported         int               `json:"imported"`
	Open             int               `json:"open"`
	// Web is where the repository's issues are browsed: github.com, or a GitHub Enterprise
	// host.
	Web string `json:"web"`
}

func (s *Server) issueSyncView(ctx context.Context, p domain.Project) (issueSyncView, error) {
	v := issueSyncView{Project: p.Slug, Label: "conductor", InProgressLabel: "in-progress",
		PublicVisibility: domain.VisibilityTeamArtifacts, Web: s.github.web()}
	if o, rn, ok := githubapp.ParseRemote(p.CanonicalRemote); ok {
		v.Repository = o + "/" + rn
	}
	cfg, found, err := s.store.TrackerConfigFor(ctx, p.ID, db.TrackerGitHub)
	if err != nil {
		return v, err
	}
	if found {
		v.Enabled, v.Label, v.All, v.InProgressLabel = cfg.Enabled, cfg.Label, cfg.Label == "", cfg.InProgressLabel
		v.PublicVisibility, v.LastSyncAt, v.LastError = cfg.PublicVisibility, cfg.LastSyncAt, cfg.LastError
		v.WriteBackError = cfg.WriteBackError
	}
	v.Imported, v.Open, err = s.store.TrackerCounts(ctx, p.ID, db.TrackerGitHub)
	return v, err
}

func (s *Server) githubIssuesStatus(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.issueSyncView(r.Context(), project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, v)
}

// issueLabel reports whether l can be a GitHub label as the sync uses one: GitHub allows 50
// characters, and a comma would split it in the labels filter.
func issueLabel(l string) bool {
	return len(l) <= 50 && !strings.ContainsAny(l, ",\n\r") && strings.TrimSpace(l) == l
}

// githubTaskIssue answers where a task's issue is, for `conductor task show` and the
// dashboard: the address GitHub gave for a synced issue (right for Enterprise hosts too), or
// one built from a hand-written github:owner/repo#N external_ref. A caller who may not see the
// task's external_ref (someone else's private task) gets 404, as for any field they cannot see.
func (s *Server) githubTaskIssue(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.TaskView(r.Context(), caller, task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name, key, ok := tracker.ParseRef(view.ExternalRef)
	if !ok || name != db.TrackerGitHub {
		s.fail(w, r, errors.Join(domain.ErrNotFound, errors.New("this task names no GitHub issue")))
		return
	}
	out := map[string]any{"external_ref": view.ExternalRef, "synced": false}
	link, found, err := s.store.TaskTrackerLink(r.Context(), task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if found && strings.HasPrefix(link.URL, "https://") {
		out["url"], out["state"], out["synced"] = link.URL, link.RemoteState, true
	} else if u, ok := githubapp.IssueURL(s.github.web(), key); ok {
		out["url"] = u
	}
	s.ok(w, r, http.StatusOK, out)
}

// githubIssuesConfigure enables, changes, or disables a project's issue sync. Like linking
// the repository, it is a project admin's decision and only for projects in the machine
// owner's organization: the app is theirs, and the sync acts through it.
func (s *Server) githubIssuesConfigure(w http.ResponseWriter, r *http.Request, p domain.Principal) {
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
		s.fail(w, r, errors.Join(domain.ErrNotPermitted, errors.New("this control plane's GitHub App belongs to its owner's organization; projects of other organizations cannot sync issues through it")))
		return
	}
	var body struct {
		Enabled          *bool             `json:"enabled"`
		Label            *string           `json:"label"`
		All              bool              `json:"all"`
		InProgressLabel  *string           `json:"in_progress_label"`
		PublicVisibility domain.Visibility `json:"public_visibility"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	cfg, found, err := s.store.TrackerConfigFor(r.Context(), project.ID, db.TrackerGitHub)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found {
		cfg = db.TrackerConfig{Label: "conductor", InProgressLabel: "in-progress", PublicVisibility: domain.VisibilityTeamArtifacts}
	}
	cfg.ProjectID, cfg.Tracker, cfg.EnabledBy = project.ID, db.TrackerGitHub, p.ID
	cfg.Enabled = body.Enabled == nil || *body.Enabled
	if body.Label != nil {
		cfg.Label = strings.TrimSpace(*body.Label)
		if cfg.Label == "" && !body.All {
			s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("an empty label would import every open issue; say so with all")))
			return
		}
	}
	if body.All {
		cfg.Label = ""
	}
	if body.InProgressLabel != nil {
		cfg.InProgressLabel = strings.TrimSpace(*body.InProgressLabel)
	}
	if body.PublicVisibility != "" {
		cfg.PublicVisibility = body.PublicVisibility
	}
	if !issueLabel(cfg.Label) || !issueLabel(cfg.InProgressLabel) {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("a label is at most 50 characters, without commas or line breaks")))
		return
	}
	if _, _, ok := githubapp.ParseRemote(project.CanonicalRemote); !ok && cfg.Enabled {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("link the project to its repository first: `conductor github link`")))
		return
	}
	if _, err := s.store.SaveTrackerConfig(r.Context(), cfg); err != nil {
		s.fail(w, r, err)
		return
	}
	action := "github.issues_enabled"
	if !cfg.Enabled {
		action = "github.issues_disabled"
	}
	s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, action, "project", project.ID,
		map[string]any{"label": cfg.Label, "in_progress_label": cfg.InProgressLabel, "public_visibility": cfg.PublicVisibility})
	if cfg.Enabled {
		s.github.Kick()
	}
	v, err := s.issueSyncView(r.Context(), project)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, v)
}

// githubIssuesSync runs an import pass for one project now: `conductor github issues sync`.
// The write-back follows on the poller's next tick.
func (s *Server) githubIssuesSync(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleMaintainer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := s.github.current(r.Context())
	if c == nil {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_configured", Error: "no GitHub App is configured; run `conductor github setup`"})
		return
	}
	cfg, found, err := s.store.TrackerConfigFor(r.Context(), project.ID, db.TrackerGitHub)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !found || !cfg.Enabled {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_enabled", Error: "issue sync is not enabled for this project; run `conductor github issues enable`"})
		return
	}
	owner, repo, ok := githubapp.ParseRemote(project.CanonicalRemote)
	if !ok {
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("link the project to its repository first: `conductor github link`")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	instID, err := c.RepoInstallation(ctx, owner, repo)
	if err != nil {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_installed", Error: "the GitHub App is not installed on " + owner + "/" + repo + ": " + err.Error()})
		return
	}
	installs, err := c.Installations(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, inst := range installs {
		if inst.ID == instID && !githubapp.Grants(inst.Permissions, "issues", "read") {
			msg := missingIssuesPermission(inst, "read")
			_ = s.store.RecordTrackerPoll(ctx, project.ID, db.TrackerGitHub, db.TrackerPoll{InstallationID: instID, Err: msg})
			s.ok(w, r, http.StatusConflict, ErrorBody{Code: "missing_permission", Error: msg})
			return
		}
	}
	info, err := c.RepositoryInfo(ctx, instID, owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rep, err := s.github.importIssues(ctx, instID, owner, repo, !info.Private, project, cfg)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, rep)
}
