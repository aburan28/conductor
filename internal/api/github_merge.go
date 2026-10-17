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
// Which task a pull request belongs to is decided by link precedence, strongest first:
//
//   manual    a person said so (conductor task link-pr)
//   request   created by a work request (phase B)
//   trailer   a Conductor-Task: T-42 trailer on any commit of the pull request
//   body_ref  the first T-42 in the pull request's body
//   branch    a branch in the repository itself — never a fork's, whose branch name is
//             whatever its author typed — that an attempt recorded, or Conductor's
//             agent/<ref>/attempt-<n> convention
//
// The trailer and the body reference are text a pull request's author wrote, so they link
// only pull requests from a branch in the repository itself, like the branch rule: a stranger's
// fork cannot name a task and have its merge complete it. A person's manual link is the
// maintainers' call and has no such limit. The linking itself lives in github_pulls.go; this
// file is the lifecycle that follows it.

// branchTask finds the open task in a project that a pull request's head branch names. ok is
// false when none does, and always for a fork.
func (g *GitHub) branchTask(ctx context.Context, p domain.Project, pr githubapp.PullRequest) (taskID domain.ID, ref string, ok bool, err error) {
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
// pull requests are recorded: look up everything that closed since the last pass, then every
// pull request last seen open that is no longer on the open list, and apply how each ended.
// The open list is the evidence; the rows in the database are the memory of what was seen
// open, so a restart forgets nothing.
func (g *GitHub) syncPullLifecycle(ctx context.Context, installationID int64, owner, repo string, open []githubapp.PullRequest, projects []domain.Project) error {
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
		if _, err := g.pullClosed(ctx, installationID, pr, projects); err != nil {
			return err
		}
	}
	g.sweep.done(key, started)

	// Pull requests seen open that left the open list but were not in the sweep (older than
	// its pages reach): look each one up.
	lookups := 0
	for _, p := range projects {
		numbers, err := g.store.OpenPullRequestNumbers(ctx, p.ID, owner+"/"+repo)
		if err != nil {
			return err
		}
		for _, number := range numbers {
			if stillOpen[number] {
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
			if _, err := g.applyPullClosed(ctx, installationID, p, pr); err != nil {
				return err
			}
		}
	}
	return nil
}

// pullClosed applies a closed pull request (from a webhook or the sweep) in each project the
// repository maps to.
func (g *GitHub) pullClosed(ctx context.Context, installationID int64, pr githubapp.PullRequest, projects []domain.Project) ([]db.PullRequestOutcome, error) {
	var out []db.PullRequestOutcome
	for _, p := range projects {
		res, err := g.applyPullClosed(ctx, installationID, p, pr)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// applyPullClosed records a closed pull request in a project — the row, its task link — then
// its end, which completes or releases the task by db.EndPullRequest's rule: a merge
// completes the task when it is the primary pull request or the last open one, and a close
// without merging returns a waiting task to ready only when none of its pull requests is
// still open.
func (g *GitHub) applyPullClosed(ctx context.Context, installationID int64, p domain.Project, pr githubapp.PullRequest) (db.PullRequestOutcome, error) {
	var res db.PullRequestOutcome
	repo := pr.Repository()
	if repo == "" {
		return res, nil
	}
	rec, err := g.recordPullIn(ctx, installationID, p, repo, pr, rollupNever, false)
	if err != nil {
		return res, err
	}
	merged := pr.WasMerged()
	res, err = g.store.EndPullRequest(ctx, p.ID, repo, pr.Number, merged)
	if err != nil || !res.Recorded {
		// Already on record (a redelivery, or the sweep seeing it again): nothing to say.
		return res, err
	}
	if res.TaskRef == "" {
		return res, nil
	}
	event := "github.pr_closed"
	if merged {
		event = "github.pr_merged"
	}
	payload := map[string]any{
		"task_ref": res.TaskRef, "pull_request": pr.HTMLURL, "branch": pr.Head.Ref,
		"from": string(res.From), "status": string(res.Status),
		"repository": repo, "number": pr.Number,
	}
	if pr.MergeCommitSHA != "" && merged {
		payload["commit_sha"] = pr.MergeCommitSHA
	}
	_ = g.store.AppendEvent(ctx, p.OrganizationID, p.ID, "", "task", rec.row.TaskID, event,
		eventVisibility(rec.row), payload)
	g.logger.Info("pull request closed", "project", p.Slug, "task", res.TaskRef,
		"merged", merged, "from", res.From, "status", res.Status, "others_open", res.OthersOpen)
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
func (s *Server) githubPullClosed(w http.ResponseWriter, r *http.Request, installationID int64, owner, repo string, pr githubapp.PullRequest) {
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
	outcomes, err := s.github.pullClosed(ctx, installationID, pr, projects)
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
