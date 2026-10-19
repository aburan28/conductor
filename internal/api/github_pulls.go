package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/githubapp"
)

// Pull requests as first-class objects (docs/COLLABORATION.md §4.5).
//
// This file is ingestion and the HTTP surface. Every pull request GitHub shows Conductor —
// by webhook, by the poller, or because a person linked one — becomes a row in pull_requests
// (db/pullrequest.go), with the task it belongs to and the rollup a reviewer wants: checks,
// review decision, unresolved threads, mergeability, size. The lifecycle that follows a merge
// or a close is github_merge.go.
//
// What is kept is deliberately thin. The title is stored (bounded): it is what the repository
// shows to everyone who can read it. The body is read once, for a task reference, and
// dropped. Review and comment text, commit messages, CI logs, and the diff are never stored:
// they are data from strangers, and the usual carriers of prompt injection.

// rollupMode says whether recording a pull request also reads its checks and review state.
type rollupMode int

const (
	rollupNever rollupMode = iota
	// rollupIfStale reads the rollup when the pull request is new, its head moved, it still
	// has checks pending, or the last read is older than rollupStale. It is what the poller
	// uses: GitHub does not bump a pull request's updated_at when CI finishes, so a settled
	// rollup is read again now and then, and a pending one every pass.
	rollupIfStale
	rollupAlways
)

// rollupStale bounds how long a settled rollup is trusted without a webhook.
const rollupStale = 15 * time.Minute

// rollupDebounce is how soon after a read a webhook-triggered read is skipped: a burst of
// check runs finishing together is one read. The poller reads pending pull requests every
// pass, which heals the tail.
//
// A var so a test can step past it without sleeping.
var rollupDebounce = 2 * time.Second

// openedRecently bounds which first sightings announce github.pr_opened: a pull request the
// poller finds on its first pass, long after it was opened, is history rather than news.
const openedRecently = 2 * time.Hour

// maxTrailerMemo bounds the memory of which heads were scanned for a trailer.
const maxTrailerMemo = 4096

// pullMemory is what the integration remembers in memory about pull requests, so a poll
// costs a conditional request and a restart costs one full pass, never a wrong transition.
type pullMemory struct {
	mu       sync.Mutex
	lists    map[string]cachedPullList // per repository: the last open list and its ETag
	trailers map[string]string         // repo#number@sha -> the task ref its commits name ("" for none)
}

type cachedPullList struct {
	etag  string
	pulls []githubapp.PullRequest
}

// openPulls lists a repository's open pull requests as a conditional request. changed is
// false when GitHub answered 304: the list is the one remembered, and nothing about any open
// pull request has changed since.
func (g *GitHub) openPulls(ctx context.Context, c *githubapp.Client, installationID int64, owner, repo string) (pulls []githubapp.PullRequest, changed bool, err error) {
	key := strings.ToLower(owner + "/" + repo)
	g.memory.mu.Lock()
	cached, have := g.memory.lists[key]
	g.memory.mu.Unlock()
	etag := ""
	if have {
		etag = cached.etag
	}
	listing, err := c.OpenPullRequestsIfChanged(ctx, installationID, owner, repo, etag)
	if err != nil {
		return nil, true, err
	}
	if listing.NotModified && have {
		return cached.pulls, false, nil
	}
	g.memory.mu.Lock()
	if g.memory.lists == nil {
		g.memory.lists = map[string]cachedPullList{}
	}
	g.memory.lists[key] = cachedPullList{etag: listing.ETag, pulls: listing.Pulls}
	g.memory.mu.Unlock()
	return listing.Pulls, true, nil
}

// pullRecord is a pull request as recorded in one project.
type pullRecord struct {
	project domain.Project
	row     db.PullRequest
	change  db.PullRequestChange
}

// recordPull records a pull request in every project its repository maps to.
func (g *GitHub) recordPull(ctx context.Context, installationID int64, pr githubapp.PullRequest, projects []domain.Project, mode rollupMode, unchanged bool) error {
	repo := pr.Repository()
	if repo == "" || pr.Number <= 0 {
		return nil
	}
	var errs []error
	for _, p := range projects {
		if _, err := g.recordPullIn(ctx, installationID, p, repo, pr, mode, unchanged); err != nil {
			errs = append(errs, fmt.Errorf("%s#%d: %w", repo, pr.Number, err))
		}
	}
	return errors.Join(errs...)
}

func pullState(pr githubapp.PullRequest) string {
	switch {
	case pr.State != "closed":
		return db.PullRequestOpen
	case pr.WasMerged():
		return db.PullRequestMerged
	}
	return db.PullRequestClosed
}

// recordPullIn records a pull request in one project: its row, its link to a task by the
// precedence in github_merge.go, the events that follow, and (by mode) its rollup. unchanged
// says the open list answered 304, so the row is not rewritten.
func (g *GitHub) recordPullIn(ctx context.Context, installationID int64, p domain.Project, repo string, pr githubapp.PullRequest, mode rollupMode, unchanged bool) (pullRecord, error) {
	rec := pullRecord{project: p}
	var row db.PullRequest
	var ch db.PullRequestChange
	have := false
	if unchanged {
		r, err := g.store.GetPullRequest(ctx, p.ID, repo, pr.Number)
		switch {
		case err == nil:
			row, have = r, true
		case !errors.Is(err, domain.ErrNotFound):
			return rec, err
		}
	}
	if pr.HTMLURL == "" {
		// Every pull request has an address; derive it where a payload (or a test double)
		// left it out, so the row always links somewhere.
		pr.HTMLURL = g.web() + "/" + repo + "/pull/" + strconv.Itoa(pr.Number)
	}
	if !have {
		author, err := g.store.PrincipalByExternalSubject(ctx, p.OrganizationID, g.web(), strconv.FormatInt(pr.User.ID, 10))
		if pr.User.ID == 0 || err != nil {
			author = ""
		}
		row, ch, err = g.store.UpsertPullRequest(ctx, db.PullRequestUpsert{
			ProjectID: p.ID, Repository: repo, Number: pr.Number, URL: pr.HTMLURL, State: pullState(pr),
			Draft: pr.Draft, Title: pr.Title, AuthorLogin: pr.User.Login, AuthorID: pr.User.ID, AuthorPrincipal: author,
			HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA, HeadRepo: pr.Head.Repo.FullName, BaseRef: pr.Base.Ref,
			FromFork: pr.FromFork(), Additions: pr.Additions, Deletions: pr.Deletions, ChangedFiles: pr.ChangedFiles,
			RemoteUpdatedAt: pr.UpdatedAt,
		})
		if err != nil {
			return rec, err
		}
	}
	row, err := g.linkPull(ctx, installationID, p, row, pr)
	if err != nil {
		return rec, err
	}
	g.pullEvents(ctx, p, row, ch, pr)
	if row.State == db.PullRequestOpen && installationID != 0 && mode != rollupNever &&
		(mode == rollupAlways || ch.Created || ch.HeadMoved || ch.LeftDraft || rollupDue(row)) {
		if err := g.applyRollup(ctx, installationID, p, &row); err != nil {
			// The rollup is an enrichment: a GitHub without GraphQL, or an installation
			// without the permission, still lists its pull requests.
			g.logger.Debug("github rollup unavailable", "repo", repo, "pr", pr.Number, "error", err)
		}
	}
	rec.row, rec.change = row, ch
	return rec, nil
}

// rollupDue reports whether a row's rollup should be read again on a poll.
func rollupDue(row db.PullRequest) bool {
	return row.RollupAt == nil || row.ChecksPending > 0 || time.Since(*row.RollupAt) > rollupStale
}

// taskRefInBody finds the first task reference in a pull request's body.
var taskRefInBody = regexp.MustCompile(`\bT-[0-9]{1,9}\b`)

// maxBodyScan bounds how much of a body is searched for a task reference.
const maxBodyScan = 64 << 10

// linkPull links a pull request to a task by the precedence in github_merge.go, and returns
// the row as it stands afterwards. A manual link (or a manual unlink) is never overridden.
func (g *GitHub) linkPull(ctx context.Context, installationID int64, p domain.Project, row db.PullRequest, pr githubapp.PullRequest) (db.PullRequest, error) {
	if row.LinkSource == db.LinkManual {
		return row, nil
	}
	rank := db.LinkRank(row.LinkSource)
	if row.TaskID == "" {
		rank = 0
	}
	trusted := !row.FromFork
	var taskID domain.ID
	var source string
	lookup := func(ref, via string) error {
		t, err := g.store.GetTaskByRef(ctx, p.ID, ref)
		switch {
		case err == nil:
			taskID, source = t.ID, via
		case !errors.Is(err, domain.ErrNotFound):
			return err
		}
		return nil
	}
	if trusted && installationID != 0 && rank < db.LinkRank(db.LinkTrailer) {
		if ref := g.trailer(ctx, installationID, row); ref != "" {
			if err := lookup(ref, db.LinkTrailer); err != nil {
				return row, err
			}
		}
	}
	if taskID == "" && trusted && rank < db.LinkRank(db.LinkBodyRef) {
		body := pr.Body
		if len(body) > maxBodyScan {
			body = body[:maxBodyScan]
		}
		if ref := taskRefInBody.FindString(body); ref != "" {
			if err := lookup(ref, db.LinkBodyRef); err != nil {
				return row, err
			}
		}
	}
	if taskID == "" && rank < db.LinkRank(db.LinkBranch) {
		id, _, ok, err := g.branchTask(ctx, p, pr)
		if err != nil {
			return row, err
		}
		if ok {
			taskID, source = id, db.LinkBranch
		}
	}
	if taskID == "" {
		return row, nil
	}
	res, err := g.store.LinkPullRequestTask(ctx, p.ID, row.Repository, row.Number, taskID, source)
	if err != nil {
		return row, err
	}
	if res.Linked || res.Source != row.LinkSource {
		return g.store.GetPullRequest(ctx, p.ID, row.Repository, row.Number)
	}
	return row, nil
}

// trailer returns the task a pull request's commits name with a Conductor-Task trailer,
// scanning a given head once.
func (g *GitHub) trailer(ctx context.Context, installationID int64, row db.PullRequest) string {
	c := g.appClient()
	owner, name, ok := strings.Cut(row.Repository, "/")
	if c == nil || !ok || row.HeadSHA == "" {
		return ""
	}
	key := strings.ToLower(row.Repository) + "#" + strconv.Itoa(row.Number) + "@" + row.HeadSHA
	g.memory.mu.Lock()
	ref, seen := g.memory.trailers[key]
	g.memory.mu.Unlock()
	if seen {
		return ref
	}
	ref, err := c.ConductorTaskTrailer(ctx, installationID, owner, name, row.Number)
	if err != nil {
		g.logger.Debug("github commits unavailable", "repo", row.Repository, "pr", row.Number, "error", err)
		return ""
	}
	g.memory.mu.Lock()
	if g.memory.trailers == nil || len(g.memory.trailers) >= maxTrailerMemo {
		g.memory.trailers = map[string]string{}
	}
	g.memory.trailers[key] = ref
	g.memory.mu.Unlock()
	return ref
}

// eventVisibility is the visibility of an event about a pull request: its task's, so an
// event about a private task's pull request is private.
func eventVisibility(row db.PullRequest) domain.Visibility {
	if row.TaskID != "" && row.TaskVisibility == domain.VisibilityPrivate {
		return domain.VisibilityPrivate
	}
	return domain.VisibilityTeamSummary
}

// emitPull appends a pull request event. Payloads carry identifiers, states, and counts, and
// pass the allowlist in privacy.SanitizeEventPayload: never the title, a body, or any text a
// stranger wrote.
func (g *GitHub) emitPull(ctx context.Context, p domain.Project, row db.PullRequest, typ string, extra map[string]any) {
	payload := map[string]any{
		"repository": row.Repository, "number": row.Number, "pull_request": row.URL,
		"branch": row.HeadRef, "commit_sha": row.HeadSHA,
	}
	if row.TaskRef != "" {
		payload["task_ref"] = row.TaskRef
	}
	for k, v := range extra {
		payload[k] = v
	}
	if err := g.store.AppendEvent(ctx, p.OrganizationID, p.ID, "", "project", p.ID, typ, eventVisibility(row), payload); err != nil {
		g.logger.Warn("record pull request event failed", "type", typ, "repo", row.Repository, "pr", row.Number, "error", err)
	}
}

// pullEvents announces what recording a pull request changed. The changes come from the row
// lock in the upsert, so a webhook redelivery or the webhook and the poller seeing the same
// change announce it once.
func (g *GitHub) pullEvents(ctx context.Context, p domain.Project, row db.PullRequest, ch db.PullRequestChange, pr githubapp.PullRequest) {
	if row.State != db.PullRequestOpen {
		return
	}
	switch {
	case ch.Created && (pr.CreatedAt.IsZero() || time.Since(pr.CreatedAt) < openedRecently):
		extra := map[string]any{"draft": row.Draft, "base": row.BaseRef}
		if row.AuthorLogin != "" {
			extra["author"] = row.AuthorLogin
		}
		g.emitPull(ctx, p, row, "github.pr_opened", extra)
	case ch.HeadMoved:
		g.emitPull(ctx, p, row, "github.pr_updated", nil)
	}
	if ch.LeftDraft && !ch.Created {
		g.emitPull(ctx, p, row, "github.pr_ready", nil)
	}
}

// checksOutcome summarizes a rollup for the pr_checks event: failed as soon as one check
// fails, pending while any runs, passed when all succeeded, none when there are none.
func checksOutcome(r db.PullRequestRollup) string {
	switch {
	case r.ChecksFailed > 0:
		return "failed"
	case r.ChecksPending > 0:
		return "pending"
	case r.ChecksTotal > 0:
		return "passed"
	}
	return "none"
}

// applyRollup reads a pull request's rollup, stores it, and announces a review decision or a
// checks result that changed. The first read of a pull request announces nothing: there is
// no "before" to have changed from.
func (g *GitHub) applyRollup(ctx context.Context, installationID int64, p domain.Project, row *db.PullRequest) error {
	c := g.appClient()
	owner, name, ok := strings.Cut(row.Repository, "/")
	if c == nil || !ok {
		return errors.New("the GitHub App is not configured")
	}
	r, err := c.PullRequestRollup(ctx, installationID, owner, name, row.Number)
	if err != nil {
		return err
	}
	before, after, err := g.store.SetPullRequestRollup(ctx, p.ID, row.Repository, row.Number, db.PullRequestRollup{
		ChecksTotal: r.ChecksTotal, ChecksPassed: r.ChecksPassed, ChecksFailed: r.ChecksFailed, ChecksPending: r.ChecksPending,
		ReviewDecision: r.ReviewDecision, UnresolvedThreads: r.UnresolvedThreads, Mergeable: r.Mergeable,
		Additions: r.Additions, Deletions: r.Deletions, ChangedFiles: r.ChangedFiles,
	})
	if err != nil {
		return err
	}
	*row = after
	if before.RollupAt == nil {
		return nil
	}
	if before.ReviewDecision != after.ReviewDecision && after.ReviewDecision != "" {
		g.emitPull(ctx, p, after, "github.pr_review", map[string]any{"decision": after.ReviewDecision})
	}
	if o := checksOutcome(after.PullRequestRollup); o != checksOutcome(before.PullRequestRollup) && (o == "passed" || o == "failed") {
		g.emitPull(ctx, p, after, "github.pr_checks", map[string]any{
			"outcome": o, "passed": after.ChecksPassed, "failed": after.ChecksFailed,
			"pending": after.ChecksPending, "count": after.ChecksTotal,
		})
	}
	return nil
}

// refreshRollup re-reads one pull request's rollup (or the whole pull request, if it has not
// been seen) in each project, for a webhook that says its checks or reviews changed.
func (g *GitHub) refreshRollup(ctx context.Context, installationID int64, owner, repo string, number int, projects []domain.Project, force bool) {
	repository := owner + "/" + repo
	for _, p := range projects {
		row, err := g.store.GetPullRequest(ctx, p.ID, repository, number)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			c := g.appClient()
			if c == nil {
				continue
			}
			pr, err := c.GetPullRequest(ctx, installationID, owner, repo, number)
			if err != nil {
				g.logger.Warn("github pull request lookup failed", "repo", repository, "pr", number, "error", err)
				continue
			}
			if pr.State == "closed" {
				_, err = g.applyPullClosed(ctx, installationID, p, pr)
			} else {
				_, err = g.recordPullIn(ctx, installationID, p, repository, pr, rollupAlways, false)
			}
			if err != nil {
				g.logger.Warn("github pull request record failed", "repo", repository, "pr", number, "error", err)
			}
			continue
		case err != nil:
			g.logger.Warn("github pull request read failed", "repo", repository, "pr", number, "error", err)
			continue
		}
		if row.State != db.PullRequestOpen {
			continue
		}
		if !force && row.RollupAt != nil && time.Since(*row.RollupAt) < rollupDebounce {
			continue
		}
		if err := g.applyRollup(ctx, installationID, p, &row); err != nil {
			g.logger.Debug("github rollup unavailable", "repo", repository, "pr", number, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Webhooks beyond pull_request
// ---------------------------------------------------------------------------

// githubPullSignal handles the deliveries that change a pull request without being a
// pull_request event: reviews and their comments, comments on the conversation, check suites,
// check runs, commit statuses, and pushes. None of them carries anything Conductor stores:
// each only says the pull request's rollup or head may have moved, so the work is to read
// them again (github_pulls.go applyRollup), in the background so GitHub is answered at once.
func (s *Server) githubPullSignal(w http.ResponseWriter, r *http.Request, event string, body []byte) {
	var ev struct {
		Action      string `json:"action"`
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		Issue struct {
			Number      int             `json:"number"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"issue"`
		CheckSuite struct {
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_suite"`
		CheckRun struct {
			HeadSHA      string `json:"head_sha"`
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_run"`
		SHA        string `json:"sha"` // status
		Ref        string `json:"ref"` // push
		Deleted    bool   `json:"deleted"`
		Repository struct {
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
		s.ok(w, r, http.StatusBadRequest, ErrorBody{Error: "not a " + event + " payload", Code: "invalid_argument"})
		return
	}
	owner, repo := ev.Repository.Owner.Login, ev.Repository.Name
	if ev.Installation.ID == 0 || !githubapp.ValidRepo(owner, repo) {
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": "not installed, or not a repository"})
		return
	}
	var numbers []int
	sha, branch := "", ""
	switch event {
	case "pull_request_review", "pull_request_review_comment":
		numbers = []int{ev.PullRequest.Number}
	case "issue_comment":
		if len(ev.Issue.PullRequest) == 0 || string(ev.Issue.PullRequest) == "null" {
			s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": "a comment on an issue"})
			return
		}
		numbers = []int{ev.Issue.Number}
	case "check_suite":
		if ev.Action != "completed" {
			s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": ev.Action})
			return
		}
		sha = ev.CheckSuite.HeadSHA
		for _, pr := range ev.CheckSuite.PullRequests {
			numbers = append(numbers, pr.Number)
		}
	case "check_run":
		if ev.Action != "completed" {
			s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": ev.Action})
			return
		}
		sha = ev.CheckRun.HeadSHA
		for _, pr := range ev.CheckRun.PullRequests {
			numbers = append(numbers, pr.Number)
		}
	case "status":
		sha = ev.SHA
	case "push":
		if ev.Deleted || !strings.HasPrefix(ev.Ref, "refs/heads/") {
			s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": "not a branch update"})
			return
		}
		branch = strings.TrimPrefix(ev.Ref, "refs/heads/")
	}
	started := s.goBackground(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		projects, err := s.github.projectsFor(ctx, owner, repo)
		if err != nil || len(projects) == 0 {
			return
		}
		inst := ev.Installation.ID
		repository := owner + "/" + repo
		for _, p := range projects {
			nums := append([]int(nil), numbers...)
			if sha != "" {
				// Forks and pull requests GitHub did not list on the event: by head commit.
				rows, err := s.store.ListPullRequests(ctx, p.ID, db.PullRequestFilter{State: db.PullRequestOpen, Repository: repository, HeadSHA: sha, Limit: 50})
				if err == nil {
					for _, row := range rows {
						nums = append(nums, row.Number)
					}
				}
			}
			if branch != "" {
				s.githubPush(ctx, inst, p, owner, repo, branch)
			}
			seen := map[int]bool{}
			for _, n := range nums {
				if n <= 0 || seen[n] {
					continue
				}
				seen[n] = true
				s.github.refreshRollup(ctx, inst, owner, repo, n, []domain.Project{p}, false)
			}
		}
	})
	if !started {
		webhookDeferred.Inc()
		s.github.Kick()
		s.ok(w, r, http.StatusAccepted, map[string]any{"deferred": event})
		return
	}
	s.ok(w, r, http.StatusAccepted, map[string]any{"refreshing": event})
}

// githubPush handles a push to a branch: pull requests whose head it is move to the new head
// (read from GitHub, which also announces github.pr_updated), and pull requests that merge
// into it are marked for a fresh read of their mergeability.
func (s *Server) githubPush(ctx context.Context, installationID int64, p domain.Project, owner, repo, branch string) {
	repository := owner + "/" + repo
	if err := s.store.InvalidatePullRequestRollups(ctx, p.ID, repository, branch); err != nil {
		s.logger.Warn("github push: invalidate rollups failed", "repo", repository, "branch", branch, "error", err)
	}
	rows, err := s.store.ListPullRequests(ctx, p.ID, db.PullRequestFilter{State: db.PullRequestOpen, Repository: repository, HeadRef: branch, Limit: 20})
	if err != nil {
		return
	}
	c := s.github.appClient()
	if c == nil {
		return
	}
	for _, row := range rows {
		if row.FromFork {
			continue
		}
		pr, err := c.GetPullRequest(ctx, installationID, owner, repo, row.Number)
		if err != nil {
			s.logger.Warn("github push: pull request lookup failed", "repo", repository, "pr", row.Number, "error", err)
			continue
		}
		if pr.State == "closed" {
			continue
		}
		if _, err := s.github.recordPullIn(ctx, installationID, p, repository, pr, rollupAlways, false); err != nil {
			s.logger.Warn("github push: record failed", "repo", repository, "pr", row.Number, "error", err)
		}
	}
}
