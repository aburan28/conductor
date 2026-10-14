package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/githubapp"
)

// The pull request lifecycle: linking a task to the pull request its work travels in, and
// finishing the task when that pull request merges.
//
// A task's last stretch happens on GitHub. Until this existed nothing ever moved a task out of
// verifying: the board filled with finished-looking work, and its territory had either been
// released too early (the next person could claim files sitting in an unmerged pull request)
// or never at all. Now the task keeps its reservations as a pending-merge hold, and the merge
// itself — delivered by webhook, or noticed by the poller when a pull request it saw open
// drops off the open list — moves the task to done and releases them.
//
// What closes how:
//
//   - merged: the task is done, wherever its work stood (db.PullRequestMerged walks the
//     ordinary edges), its lease if any is ended, and its territory released.
//   - closed without merging: a task that was waiting on the pull request goes back to ready
//     and drops its hold; a task still being worked is left alone (db.PullRequestClosed).
//
// Which task a pull request belongs to is decided exactly as the check run decides "the pull
// request's own task" (checkPull): a branch in the repository itself — never a fork's, whose
// branch name is whatever its author typed — that an attempt recorded, or Conductor's
// agent/<ref>/attempt-<n> convention; or a pull request already linked to the task.

// pullTask finds the open task in a project a pull request belongs to. ok is false when none
// does.
func (g *GitHub) pullTask(ctx context.Context, p domain.Project, pr githubapp.PullRequest) (taskID domain.ID, ref string, ok bool, err error) {
	if pr.HTMLURL != "" {
		linked, err := g.store.TasksWithOpenPullRequests(ctx, p.ID)
		if err != nil {
			return "", "", false, err
		}
		for _, t := range linked {
			if t.URL == pr.HTMLURL {
				return t.TaskID, t.Ref, true, nil
			}
		}
	}
	if pr.FromFork() || pr.Head.Ref == "" {
		return "", "", false, nil
	}
	footprints, err := g.store.OpenFootprints(ctx, p.ID)
	if err != nil {
		return "", "", false, err
	}
	m := agentBranch.FindStringSubmatch(pr.Head.Ref)
	for _, fp := range footprints {
		if (fp.Branch != "" && fp.Branch == pr.Head.Ref) || (m != nil && fp.TaskRef == m[1]) {
			return fp.TaskID, fp.TaskRef, true, nil
		}
	}
	return "", "", false, nil
}

// linkPulls records, for each open pull request, the task it belongs to. It is what lets the
// poller later notice that pull request's merge without ever receiving a webhook.
func (g *GitHub) linkPulls(ctx context.Context, pulls []githubapp.PullRequest, projects []domain.Project) error {
	for _, pr := range pulls {
		if pr.HTMLURL == "" {
			continue
		}
		for _, p := range projects {
			taskID, _, ok, err := g.pullTask(ctx, p, pr)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if _, err := g.store.LinkPullRequest(ctx, taskID, pr.HTMLURL); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxClosedLookups bounds how many vanished pull requests one poll looks up, so a burst of
// merges costs a few API calls per pass rather than one burst of many.
const maxClosedLookups = 20

// The closed-list sweep. A pull request opened and merged between two polls is never on the
// open list, so it is never linked, and the vanished-PR lookup cannot find it; listing what
// closed since the last sweep does.
const (
	// maxClosedPages bounds one sweep to a few hundred pull requests per repository.
	maxClosedPages = 3
	// closedSweepMargin re-reads a little before the last sweep, so a pull request updated
	// while that sweep was running is not lost to clock skew between here and GitHub.
	closedSweepMargin = 10 * time.Minute
	// closedFirstLookback is how far back the first sweep after a start reads. A merge
	// older than that while the daemon was down is still found if its pull request was
	// linked (the vanished-PR lookup), but not otherwise.
	closedFirstLookback = 24 * time.Hour
)

// pullSweep is the per-repository time of the last successful closed-list sweep. It lives in
// memory: losing it on restart costs one wider sweep, never a missed or doubled transition,
// because recording a pull request's end is idempotent (db.PullRequestOutcome.Recorded).
type pullSweep struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (p *pullSweep) since(repo string, now time.Time) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.last[repo]; ok {
		return t.Add(-closedSweepMargin)
	}
	return now.Add(-closedFirstLookback)
}

func (p *pullSweep) done(repo string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		p.last = map[string]time.Time{}
	}
	p.last[repo] = at
}

// syncPullLifecycle is the poller's half of the lifecycle, run after each repository's open
// pull requests are checked: link the open ones to their tasks, then look up every linked
// pull request that is no longer open and apply how it ended. The open list is the evidence;
// the linked set in the database is the memory of what was seen open, so a restart forgets
// nothing.
func (g *GitHub) syncPullLifecycle(ctx context.Context, installationID int64, owner, repo string, open []githubapp.PullRequest, projects []domain.Project) error {
	if err := g.linkPulls(ctx, open, projects); err != nil {
		return err
	}
	stillOpen := map[int]bool{}
	for _, pr := range open {
		stillOpen[pr.Number] = true
	}
	c := g.appClient()
	if c == nil {
		return nil
	}

	// Everything that closed since the last sweep, linked or not.
	started := time.Now()
	key := strings.ToLower(owner + "/" + repo)
	closed, err := c.ClosedPullRequests(ctx, installationID, owner, repo, g.sweep.since(key, started), maxClosedPages)
	if err != nil {
		return fmt.Errorf("closed pull requests: %w", err)
	}
	for _, pr := range closed {
		if pr.State != "closed" {
			continue
		}
		if _, err := g.pullClosed(ctx, pr, projects); err != nil {
			return err
		}
	}
	g.sweep.done(key, started)

	// Linked pull requests that left the open list but were not in the sweep (older than
	// its pages reach): look each one up.
	lookups := 0
	for _, p := range projects {
		linked, err := g.store.TasksWithOpenPullRequests(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, t := range linked {
			o, r, number, ok := parsePullURL(t.URL)
			if !ok || !strings.EqualFold(o, owner) || !strings.EqualFold(r, repo) || stillOpen[number] {
				continue
			}
			// The open list is capped (100, newest first). A pull request missing from it may
			// simply be old and still open, which the lookup below settles.
			if lookups >= maxClosedLookups {
				return nil
			}
			lookups++
			pr, err := c.GetPullRequest(ctx, installationID, owner, repo, number)
			if err != nil {
				return fmt.Errorf("%s#%d: %w", owner+"/"+repo, number, err)
			}
			if pr.State != "closed" {
				continue
			}
			if _, err := g.applyPullClosed(ctx, p, t.TaskID, pr); err != nil {
				return err
			}
		}
	}
	return nil
}

// pullClosed applies a closed pull request (from a webhook) to whichever task it belongs to,
// in each project the repository maps to.
func (g *GitHub) pullClosed(ctx context.Context, pr githubapp.PullRequest, projects []domain.Project) ([]db.PullRequestOutcome, error) {
	var out []db.PullRequestOutcome
	for _, p := range projects {
		taskID, _, ok, err := g.pullTask(ctx, p, pr)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		res, err := g.applyPullClosed(ctx, p, taskID, pr)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func (g *GitHub) applyPullClosed(ctx context.Context, p domain.Project, taskID domain.ID, pr githubapp.PullRequest) (db.PullRequestOutcome, error) {
	var res db.PullRequestOutcome
	var err error
	merged := pr.WasMerged()
	event := "github.pr_closed"
	if merged {
		res, err = g.store.PullRequestMerged(ctx, taskID, pr.HTMLURL)
		event = "github.pr_merged"
	} else {
		res, err = g.store.PullRequestClosed(ctx, taskID, pr.HTMLURL)
	}
	if err != nil || !res.Recorded {
		// Already on record (a redelivery, or the sweep seeing it again): nothing to say.
		return res, err
	}
	payload := map[string]any{
		"task_ref": res.TaskRef, "pull_request": pr.HTMLURL, "branch": pr.Head.Ref,
		"from": string(res.From), "status": string(res.Status),
	}
	if pr.MergeCommitSHA != "" && merged {
		payload["commit_sha"] = pr.MergeCommitSHA
	}
	_ = g.store.AppendEvent(ctx, p.OrganizationID, p.ID, "", "task", taskID, event,
		domain.VisibilityTeamSummary, payload)
	g.logger.Info("pull request closed", "project", p.Slug, "task", res.TaskRef,
		"merged", merged, "from", res.From, "status", res.Status)
	return res, nil
}

// parsePullURL reads owner, repository, and number from a pull request's html_url
// (https://<host>/<owner>/<repo>/pull/<n>). The host is not checked, so GitHub Enterprise
// URLs parse the same way.
func parsePullURL(u string) (owner, repo string, number int, ok bool) {
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 5 || parts[len(parts)-2] != "pull" {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	return parts[len(parts)-4], parts[len(parts)-3], n, true
}

// githubPullClosed handles a pull_request "closed" delivery. Unlike a check it is applied
// before answering: it is a few database statements, and doing it inline means GitHub's
// delivery log shows whether a merge actually completed a task.
func (s *Server) githubPullClosed(w http.ResponseWriter, r *http.Request, owner, repo string, pr githubapp.PullRequest) {
	if !githubapp.ValidRepo(owner, repo) {
		s.ok(w, r, http.StatusAccepted, map[string]any{"ignored": "not a repository"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	projects, err := s.github.projectsFor(ctx, owner, repo)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	outcomes, err := s.github.pullClosed(ctx, pr, projects)
	if err != nil {
		s.logger.Warn("github pull request close failed", "repo", owner+"/"+repo, "pr", pr.Number, "error", err)
		s.fail(w, r, err)
		return
	}
	if outcomes == nil {
		outcomes = []db.PullRequestOutcome{}
	}
	s.ok(w, r, http.StatusOK, map[string]any{"merged": pr.WasMerged(), "tasks": outcomes})
}
