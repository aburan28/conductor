package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/privacy"
)

// Pull requests (migration 0020; docs/COLLABORATION.md §4.5).
//
// The pull_requests table is the source of truth for every pull request Conductor has seen
// on a linked repository. A task may have several; a pull request has at most one task.
// tasks.pull_request_url/_state mirror the task's *primary* pull request
// (tasks.primary_pull_request, "owner/repo#N"), so every reader written against migration
// 0006 — the card, the CLI, issue sync, conflict messages — keeps working unchanged.
//
// Lifecycle, in short: a merge completes the task when the merged pull request is primary,
// or when it was the task's last open one; a close without merging sends a waiting task
// back to ready only when no other pull request of its is still open. Linking has a
// precedence (link_source), and a weaker source never replaces a stronger one.

// PendingMerge reports whether a task in this status has finished its work but not landed
// it: the changes sit in a branch or pull request that has not merged. Such a task keeps its
// scope reservations as a hold, so nobody is handed files whose new contents are still in
// flight.
func PendingMerge(s domain.TaskStatus) bool {
	return s == domain.TaskVerifying || s == domain.TaskReviewRequired || s == domain.TaskMerging
}

// Pull request states, as GitHub reports them.
const (
	PullRequestOpen   = "open"
	PullRequestMerged = "merged"
	PullRequestClosed = "closed"
)

// Link sources, in rising precedence. A stronger source replaces a weaker one; a weaker
// never replaces a stronger. LinkManual may replace LinkManual (relinking by hand).
const (
	LinkBranch  = "branch"   // the head branch names the task (an attempt's branch, or agent/<ref>/attempt-<n>)
	LinkBodyRef = "body_ref" // the body mentions T-42 (first match)
	LinkTrailer = "trailer"  // a Conductor-Task: T-42 trailer on a commit
	LinkRequest = "request"  // created by a work request (phase B)
	LinkManual  = "manual"   // conductor task link-pr
)

// LinkRank orders link sources: higher wins. Zero is no source.
func LinkRank(source string) int { return linkRank(source) }

func linkRank(source string) int {
	switch source {
	case LinkBranch:
		return 1
	case LinkBodyRef:
		return 2
	case LinkTrailer:
		return 3
	case LinkRequest:
		return 4
	case LinkManual:
		return 5
	}
	return 0
}

// ValidLinkSource reports whether source is one a caller may record.
func ValidLinkSource(source string) bool { return linkRank(source) > 0 }

// PullRequestKey is how a task names its primary pull request: "owner/repo#N".
func PullRequestKey(repository string, number int) string {
	return repository + "#" + strconv.Itoa(number)
}

// ParsePullRequestKey reads "owner/repo#N".
func ParsePullRequestKey(key string) (repository string, number int, ok bool) {
	repository, n, found := strings.Cut(key, "#")
	if !found || !strings.Contains(repository, "/") {
		return "", 0, false
	}
	number, err := strconv.Atoi(n)
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return repository, number, true
}

// ParsePullRequestURL reads owner/repo and number from a pull request's html_url
// (https://<host>/<owner>/<repo>/pull/<n>). The host is not checked, so GitHub Enterprise
// URLs parse the same way.
func ParsePullRequestURL(u string) (repository string, number int, ok bool) {
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 5 || parts[len(parts)-2] != "pull" {
		return "", 0, false
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return parts[len(parts)-4] + "/" + parts[len(parts)-3], n, true
}

// maxPullRequestTitle bounds a stored title. GitHub allows 256 characters; the bound keeps a
// pathological title from becoming a transcript column by another name.
const maxPullRequestTitle = privacy.MaxPullRequestTitle

func clampTitle(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxPullRequestTitle {
		return string(r)
	}
	return string(r[:maxPullRequestTitle]) + "…"
}

// PullRequest is one row, with the task and author joined in.
type PullRequest struct {
	ProjectID  domain.ID
	Repository string
	Number     int
	URL        string

	TaskID         domain.ID
	TaskRef        string
	TaskVisibility domain.Visibility
	TaskOwner      domain.ID
	TaskStatus     domain.TaskStatus
	LinkSource     string
	Primary        bool

	State       string
	Draft       bool
	Title       string
	AuthorLogin string
	AuthorID    int64
	// AuthorPrincipal and AuthorHandle name the member whose GitHub identity matches the
	// author, when one is linked through SSO.
	AuthorPrincipal domain.ID
	AuthorHandle    string

	HeadRef  string
	HeadSHA  string
	HeadRepo string
	BaseRef  string
	FromFork bool

	PullRequestRollup

	RemoteUpdatedAt *time.Time
	RollupAt        *time.Time
	LastSeenAt      time.Time
	CreatedAt       time.Time
}

// Key is "owner/repo#N".
func (p PullRequest) Key() string { return PullRequestKey(p.Repository, p.Number) }

// PullRequestRollup is what the checks and review APIs say about a pull request right now.
type PullRequestRollup struct {
	ChecksTotal       int
	ChecksPassed      int
	ChecksFailed      int
	ChecksPending     int
	ReviewDecision    string // '' | approved | changes_requested | review_required
	UnresolvedThreads int
	Mergeable         string // '' | clean | dirty | blocked | behind | unknown
	// The size of the change. Zero means unknown: the REST list does not carry it, so a
	// zero never overwrites a size already read.
	Additions    int
	Deletions    int
	ChangedFiles int
}

const pullRequestColumns = `
	p.project_id::text, p.repository, p.number, p.url,
	COALESCE(p.task_id::text, ''), COALESCE(t.ref, ''), COALESCE(t.visibility, ''), COALESCE(t.created_by::text, ''),
	COALESCE(t.status, ''), p.link_source,
	COALESCE(t.primary_pull_request = p.repository || '#' || p.number::text, false),
	p.state, p.draft, p.title, p.author_login, p.author_id,
	COALESCE(p.author_principal::text, ''), COALESCE(a.handle, ''),
	p.head_ref, p.head_sha, p.head_repo, p.base_ref, p.from_fork,
	p.checks_total, p.checks_passed, p.checks_failed, p.checks_pending,
	p.review_decision, p.unresolved_threads, p.mergeable,
	p.additions, p.deletions, p.changed_files,
	p.remote_updated_at, p.rollup_at, p.last_seen_at, p.created_at`

const pullRequestFrom = ` FROM pull_requests p
	LEFT JOIN tasks t ON t.id = p.task_id
	LEFT JOIN principals a ON a.id = p.author_principal`

func scanPullRequest(scan func(dest ...any) error) (PullRequest, error) {
	var p PullRequest
	err := scan(&p.ProjectID, &p.Repository, &p.Number, &p.URL,
		&p.TaskID, &p.TaskRef, &p.TaskVisibility, &p.TaskOwner, &p.TaskStatus, &p.LinkSource, &p.Primary,
		&p.State, &p.Draft, &p.Title, &p.AuthorLogin, &p.AuthorID, &p.AuthorPrincipal, &p.AuthorHandle,
		&p.HeadRef, &p.HeadSHA, &p.HeadRepo, &p.BaseRef, &p.FromFork,
		&p.ChecksTotal, &p.ChecksPassed, &p.ChecksFailed, &p.ChecksPending,
		&p.ReviewDecision, &p.UnresolvedThreads, &p.Mergeable,
		&p.Additions, &p.Deletions, &p.ChangedFiles,
		&p.RemoteUpdatedAt, &p.RollupAt, &p.LastSeenAt, &p.CreatedAt)
	return p, err
}

// querier is what the read helpers need from a pool or a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getPullRequest(ctx context.Context, q querier, projectID domain.ID, repository string, number int, lock bool) (PullRequest, error) {
	sql := `SELECT ` + pullRequestColumns + pullRequestFrom + `
		 WHERE p.project_id = $1::uuid AND lower(p.repository) = lower($2) AND p.number = $3`
	if lock {
		sql += " FOR UPDATE OF p"
	}
	p, err := scanPullRequest(q.QueryRow(ctx, sql, projectID, repository, number).Scan)
	return p, noRows(err)
}

// GetPullRequest reads one pull request of a project.
func (s *Store) GetPullRequest(ctx context.Context, projectID domain.ID, repository string, number int) (PullRequest, error) {
	return getPullRequest(ctx, s.pool, projectID, repository, number, false)
}

// PullRequestFilter narrows ListPullRequests. Zero values do not filter.
type PullRequestFilter struct {
	State       string    // open | merged | closed
	Repository  string    // owner/repo
	Number      int       // one pull request by number, in any linked repository
	TaskID      domain.ID // linked to this task
	Unlinked    bool      // no task
	NeedsReview bool      // open, not a draft, and not yet approved or sent back
	Failing     bool      // at least one failed check
	// Mine is a member: pull requests they opened (by linked GitHub identity) or whose
	// task they own.
	Mine domain.ID
	// Viewer limits filters on hidden metadata to tasks this caller may see. Empty
	// means a trusted internal query, which does not project results to a member.
	Viewer  domain.ID
	HeadSHA string
	HeadRef string
	Limit   int
}

// ListPullRequests lists a project's pull requests, open ones first, most recently changed
// first within a state.
func (s *Store) ListPullRequests(ctx context.Context, projectID domain.ID, f PullRequestFilter) ([]PullRequest, error) {
	q := strings.Builder{}
	q.WriteString(`SELECT ` + pullRequestColumns + pullRequestFrom + ` WHERE p.project_id = $1::uuid`)
	args := []any{projectID}
	add := func(clause string, v any) {
		args = append(args, v)
		q.WriteString(" AND " + strings.ReplaceAll(clause, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.State != "" {
		add("p.state = ?", f.State)
	}
	if f.Repository != "" {
		add("lower(p.repository) = lower(?)", f.Repository)
	}
	if f.Number > 0 {
		add("p.number = ?", f.Number)
	}
	if f.TaskID != "" {
		add("p.task_id = ?::uuid", f.TaskID)
	}
	if f.Unlinked {
		q.WriteString(" AND p.task_id IS NULL")
	}
	if f.Viewer != "" && (f.NeedsReview || f.Failing || f.Mine != "") {
		// Redacting the response after filtering would still reveal a private task's
		// checks, review, or author through membership in the result. Apply this before
		// ordering and LIMIT so pagination cannot reveal those hidden matches either.
		add("(t.id IS NULL OR t.visibility <> 'private' OR t.created_by = ?::uuid)", f.Viewer)
	}
	if f.NeedsReview {
		q.WriteString(" AND p.state = 'open' AND NOT p.draft AND p.review_decision IN ('', 'review_required')")
	}
	if f.Failing {
		q.WriteString(" AND p.checks_failed > 0")
	}
	if f.Mine != "" {
		args = append(args, f.Mine)
		n := strconv.Itoa(len(args))
		q.WriteString(" AND (p.author_principal = $" + n + "::uuid OR t.created_by = $" + n + "::uuid)")
	}
	if f.HeadSHA != "" {
		add("p.head_sha = ?", f.HeadSHA)
	}
	if f.HeadRef != "" {
		add("p.head_ref = ?", f.HeadRef)
	}
	q.WriteString(` ORDER BY (p.state = 'open') DESC, COALESCE(p.remote_updated_at, p.last_seen_at) DESC, p.number DESC`)
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args = append(args, limit)
	q.WriteString(" LIMIT $" + strconv.Itoa(len(args)))
	rows, err := s.pool.Query(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PullRequest{}
	for rows.Next() {
		p, err := scanPullRequest(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PullRequestUpsert is what GitHub's pull request object says, as the webhook, the list, or
// the single-PR endpoint delivered it. Nothing here is the body.
type PullRequestUpsert struct {
	ProjectID  domain.ID
	Repository string
	Number     int
	URL        string
	// State is open, merged, or closed. An existing row only moves to open here (a reopen)
	// or stays where it is: merges and closes are recorded by EndPullRequest, which also
	// applies them to the task, so a listing that races a webhook cannot skip that step.
	State string
	Draft bool
	Title string

	AuthorLogin     string
	AuthorID        int64
	AuthorPrincipal domain.ID

	HeadRef  string
	HeadSHA  string
	HeadRepo string
	BaseRef  string
	FromFork bool

	// Sizes, when the source carried them (a webhook or the single-PR endpoint); zero keeps
	// what is stored.
	Additions    int
	Deletions    int
	ChangedFiles int

	RemoteUpdatedAt time.Time
}

// PullRequestChange is what an upsert changed, for the events that follow it.
type PullRequestChange struct {
	Created   bool // first time this pull request was seen
	Reopened  bool // was closed, is open again
	HeadMoved bool // the head commit changed
	LeftDraft bool // was a draft, is ready for review
	// Updated is true when GitHub's updated_at moved past the one stored (or none was).
	Updated bool
}

// UpsertPullRequest records a pull request as GitHub currently describes it.
func (s *Store) UpsertPullRequest(ctx context.Context, u PullRequestUpsert) (PullRequest, PullRequestChange, error) {
	var out PullRequest
	var ch PullRequestChange
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, ch, err = upsertPullRequestTx(ctx, tx, u)
		return err
	})
	return out, ch, err
}

func upsertPullRequestTx(ctx context.Context, tx pgx.Tx, u PullRequestUpsert) (PullRequest, PullRequestChange, error) {
	var ch PullRequestChange
	if u.Repository == "" || u.Number <= 0 {
		return PullRequest{}, ch, fmt.Errorf("%w: a pull request needs a repository and a number", domain.ErrInvalidArgument)
	}
	state := u.State
	if state == "" {
		state = PullRequestOpen
	}
	if state != PullRequestOpen && state != PullRequestMerged && state != PullRequestClosed {
		return PullRequest{}, ch, fmt.Errorf("%w: pull request state %q", domain.ErrInvalidArgument, state)
	}
	// GitHub repository names are case-insensitive. Serialize initial observations too:
	// a row lock alone cannot protect a pull request that has not been inserted yet.
	key := "conductor/pull/" + u.ProjectID + "/" + PullRequestKey(strings.ToLower(u.Repository), u.Number)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return PullRequest{}, ch, err
	}
	prev, err := getPullRequest(ctx, tx, u.ProjectID, u.Repository, u.Number, true)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		ch.Created = true
		// A row is born open, even for a pull request that has already ended: the end is
		// recorded next, by EndPullRequest, which is what completes a task.
		state = PullRequestOpen
	case err != nil:
		return PullRequest{}, ch, err
	default:
		// Keep the existing primary-key spelling when a manual reference and GitHub's
		// response use different case, preserving the task link and its privacy policy.
		u.Repository = prev.Repository
		ch.Reopened = prev.State == PullRequestClosed && state == PullRequestOpen
		ch.HeadMoved = u.HeadSHA != "" && prev.HeadSHA != "" && u.HeadSHA != prev.HeadSHA
		ch.LeftDraft = prev.Draft && !u.Draft
		ch.Updated = !u.RemoteUpdatedAt.IsZero() && (prev.RemoteUpdatedAt == nil || u.RemoteUpdatedAt.After(*prev.RemoteUpdatedAt))
		if prev.State == PullRequestMerged || state != PullRequestOpen {
			state = prev.State
		}
	}
	if ch.Created {
		ch.Updated = true
	}
	var remote *time.Time
	if !u.RemoteUpdatedAt.IsZero() {
		t := u.RemoteUpdatedAt.UTC()
		remote = &t
	}
	var author any
	if u.AuthorPrincipal != "" {
		author = string(u.AuthorPrincipal)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pull_requests (project_id, repository, number, url, state, draft, title,
		        author_login, author_id, author_principal, head_ref, head_sha, head_repo, base_ref, from_fork,
		        additions, deletions, changed_files, remote_updated_at, last_seen_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10::uuid, $11, $12, $13, $14, $15, $16, $17, $18, $19, now())
		ON CONFLICT (project_id, repository, number) DO UPDATE
		   SET url = CASE WHEN EXCLUDED.url = '' THEN pull_requests.url ELSE EXCLUDED.url END,
		       state = EXCLUDED.state, draft = EXCLUDED.draft,
		       title = CASE WHEN EXCLUDED.title = '' THEN pull_requests.title ELSE EXCLUDED.title END,
		       author_login = CASE WHEN EXCLUDED.author_login = '' THEN pull_requests.author_login ELSE EXCLUDED.author_login END,
		       author_id = CASE WHEN EXCLUDED.author_id = 0 THEN pull_requests.author_id ELSE EXCLUDED.author_id END,
		       author_principal = COALESCE(EXCLUDED.author_principal, pull_requests.author_principal),
		       head_ref = CASE WHEN EXCLUDED.head_ref = '' THEN pull_requests.head_ref ELSE EXCLUDED.head_ref END,
		       head_sha = CASE WHEN EXCLUDED.head_sha = '' THEN pull_requests.head_sha ELSE EXCLUDED.head_sha END,
		       head_repo = CASE WHEN EXCLUDED.head_repo = '' THEN pull_requests.head_repo ELSE EXCLUDED.head_repo END,
		       base_ref = CASE WHEN EXCLUDED.base_ref = '' THEN pull_requests.base_ref ELSE EXCLUDED.base_ref END,
		       from_fork = EXCLUDED.from_fork,
		       additions = CASE WHEN EXCLUDED.changed_files = 0 THEN pull_requests.additions ELSE EXCLUDED.additions END,
		       deletions = CASE WHEN EXCLUDED.changed_files = 0 THEN pull_requests.deletions ELSE EXCLUDED.deletions END,
		       changed_files = CASE WHEN EXCLUDED.changed_files = 0 THEN pull_requests.changed_files ELSE EXCLUDED.changed_files END,
		       remote_updated_at = COALESCE(EXCLUDED.remote_updated_at, pull_requests.remote_updated_at),
		       last_seen_at = now()`,
		u.ProjectID, u.Repository, u.Number, u.URL, state, u.Draft, clampTitle(u.Title),
		u.AuthorLogin, u.AuthorID, author, u.HeadRef, u.HeadSHA, u.HeadRepo, u.BaseRef, u.FromFork,
		u.Additions, u.Deletions, u.ChangedFiles, remote); err != nil {
		return PullRequest{}, ch, err
	}
	// A reopened pull request shows on its task again; a head that moved on the primary
	// changes nothing the mirror carries, but keep the mirror honest either way.
	if ch.Reopened && prev.TaskID != "" {
		if err := mirrorPrimaryTx(ctx, tx, prev.TaskID); err != nil {
			return PullRequest{}, ch, err
		}
	}
	out, err := getPullRequest(ctx, tx, u.ProjectID, u.Repository, u.Number, false)
	return out, ch, err
}

// SetPullRequestRollup records what the checks and review APIs said, and returns the row as
// it was before, so the caller can tell what changed.
func (s *Store) SetPullRequestRollup(ctx context.Context, projectID domain.ID, repository string, number int, r PullRequestRollup) (before PullRequest, after PullRequest, err error) {
	err = s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		before, err = getPullRequest(ctx, tx, projectID, repository, number, true)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE pull_requests
			   SET checks_total = $4, checks_passed = $5, checks_failed = $6, checks_pending = $7,
			       review_decision = $8, unresolved_threads = $9, mergeable = $10,
			       additions = CASE WHEN $13 = 0 THEN additions ELSE $11 END,
			       deletions = CASE WHEN $13 = 0 THEN deletions ELSE $12 END,
			       changed_files = CASE WHEN $13 = 0 THEN changed_files ELSE $13 END,
			       rollup_at = now(), last_seen_at = now()
			 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
			projectID, repository, number, r.ChecksTotal, r.ChecksPassed, r.ChecksFailed, r.ChecksPending,
			r.ReviewDecision, r.UnresolvedThreads, r.Mergeable, r.Additions, r.Deletions, r.ChangedFiles); err != nil {
			return err
		}
		after, err = getPullRequest(ctx, tx, projectID, repository, number, false)
		return err
	})
	return before, after, err
}

// PullRequestLink is the outcome of linking a pull request to a task.
type PullRequestLink struct {
	// Linked is true when the link changed: the pull request now belongs to the task (or
	// its source was upgraded). False when it already did, or when a stronger link stood.
	Linked bool
	// Source is the link's source after the call.
	Source  string
	TaskRef string
	// Previous is the task the pull request was linked to before, when it moved.
	Previous domain.ID
}

// LinkPullRequestTask links a pull request to a task under a source. A weaker source never
// replaces a stronger one; manual may replace manual. Automatic sources (branch, body_ref,
// trailer) do not link a finished task — a pull request against a task that was never
// worked under Conductor, or already closed, is not that task's work. The first pull request
// linked to a task becomes its primary, as does any linked by hand.
func (s *Store) LinkPullRequestTask(ctx context.Context, projectID domain.ID, repository string, number int, taskID domain.ID, source string) (PullRequestLink, error) {
	var out PullRequestLink
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = linkPullRequestTx(ctx, tx, projectID, repository, number, taskID, source)
		return err
	})
	return out, err
}

// LinkPullRequestTaskForMember makes a manual link after checking both tasks in the
// transaction. Moving a pull request also unlinks its previous task; authorization on
// just the destination would let a member expose someone else's private pull request.
func (s *Store) LinkPullRequestTaskForMember(ctx context.Context, projectID domain.ID, repository string, number int, taskID, principalID domain.ID, role domain.Role) (PullRequestLink, error) {
	var out PullRequestLink
	if !role.Can(domain.RoleContributor) {
		return out, domain.ErrNotPermitted
	}
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		pr, err := getPullRequest(ctx, tx, projectID, repository, number, true)
		if err != nil {
			return err
		}
		ids := []string{taskID}
		if pr.TaskID != "" && pr.TaskID != taskID {
			ids = append(ids, pr.TaskID)
		}
		rows, err := tx.Query(ctx, `SELECT id::text, COALESCE(created_by::text, ''), visibility
			FROM tasks WHERE project_id = $1::uuid AND id = ANY($2::uuid[])
			ORDER BY id FOR UPDATE`, projectID, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			var id, owner domain.ID
			var visibility domain.Visibility
			if err := rows.Scan(&id, &owner, &visibility); err != nil {
				return err
			}
			found = found || id == taskID
			if visibility == domain.VisibilityPrivate && owner != principalID && !role.Can(domain.RoleMaintainer) {
				return fmt.Errorf("%w: only the owner of a private task, or a maintainer, can change its pull request links", domain.ErrNotPermitted)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if !found {
			return domain.ErrNotFound
		}
		out, err = linkPullRequestTx(ctx, tx, projectID, repository, number, taskID, LinkManual)
		return err
	})
	return out, err
}

func linkPullRequestTx(ctx context.Context, tx pgx.Tx, projectID domain.ID, repository string, number int, taskID domain.ID, source string) (PullRequestLink, error) {
	var out PullRequestLink
	if !ValidLinkSource(source) {
		return out, fmt.Errorf("%w: link source %q", domain.ErrInvalidArgument, source)
	}
	pr, err := getPullRequest(ctx, tx, projectID, repository, number, true)
	if err != nil {
		return out, err
	}
	var orgID domain.ID
	var ref string
	var status domain.TaskStatus
	var primary string
	if err := tx.QueryRow(ctx, `
		SELECT organization_id::text, ref, status, primary_pull_request FROM tasks
		 WHERE id = $1::uuid AND project_id = $2::uuid FOR UPDATE`, taskID, projectID,
	).Scan(&orgID, &ref, &status, &primary); err != nil {
		return out, noRows(err)
	}
	out.TaskRef, out.Source = ref, pr.LinkSource
	if pr.TaskID == taskID {
		if linkRank(source) > linkRank(pr.LinkSource) {
			if _, err := tx.Exec(ctx, `UPDATE pull_requests SET link_source = $4
				 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
				projectID, repository, number, source); err != nil {
				return out, err
			}
			out.Source = source
		}
		return out, nil
	}
	if pr.TaskID == "" && pr.LinkSource == LinkManual && source != LinkManual {
		return out, nil
	}
	if pr.TaskID != "" {
		if linkRank(source) < linkRank(pr.LinkSource) || (linkRank(source) == linkRank(pr.LinkSource) && source != LinkManual) {
			return out, nil
		}
	}
	if source != LinkManual && source != LinkRequest && status.IsTerminal() {
		return out, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET task_id = $4::uuid, link_source = $5
		 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
		projectID, repository, number, taskID, source); err != nil {
		return out, err
	}
	out.Linked, out.Source, out.Previous = true, source, pr.TaskID
	key := PullRequestKey(pr.Repository, pr.Number)
	if primary == "" || source == LinkManual {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET primary_pull_request = $2 WHERE id = $1::uuid`, taskID, key); err != nil {
			return out, err
		}
	}
	if pr.TaskID != "" {
		if err := mirrorPrimaryTx(ctx, tx, pr.TaskID); err != nil {
			return out, err
		}
	}
	if err := mirrorPrimaryTx(ctx, tx, taskID); err != nil {
		return out, err
	}
	return out, appendEvents(ctx, tx, orgID, projectID, "",
		eventSpec{"task", taskID, "task.pull_request_linked", domain.VisibilityTeamSummary,
			map[string]any{"task_ref": ref, "pull_request": pr.URL, "state": pr.State,
				"repository": pr.Repository, "number": pr.Number, "link_source": source}})
}

// UnlinkPullRequestTask removes a pull request's link to a task. It reports false when the
// pull request was not linked to that task.
func (s *Store) UnlinkPullRequestTask(ctx context.Context, projectID domain.ID, repository string, number int, taskID domain.ID) (bool, error) {
	var done bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		pr, err := getPullRequest(ctx, tx, projectID, repository, number, true)
		if err != nil {
			return err
		}
		if pr.TaskID != taskID {
			return nil
		}
		// 'manual' with no task is a person's decision that this pull request has no task:
		// automatic sources do not link it again (linkPullRequestTx).
		if _, err := tx.Exec(ctx, `UPDATE pull_requests SET task_id = NULL, link_source = 'manual'
			 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
			projectID, repository, number); err != nil {
			return err
		}
		done = true
		return mirrorPrimaryTx(ctx, tx, taskID)
	})
	return done, err
}

// mirrorPrimaryTx keeps tasks.primary_pull_request pointing at a pull request the task still
// has (the open ones first, then the oldest) and copies that pull request's URL and state
// into the 0006 mirror columns. A task with no pull request left has both cleared.
func mirrorPrimaryTx(ctx context.Context, tx pgx.Tx, taskID domain.ID) error {
	_, err := tx.Exec(ctx, `
		WITH chosen AS (
		    SELECT p.repository || '#' || p.number::text AS key, p.url, p.state
		      FROM pull_requests p, tasks t
		     WHERE t.id = $1::uuid AND p.task_id = t.id
		     ORDER BY (p.repository || '#' || p.number::text = t.primary_pull_request) DESC,
		              (p.state = 'open') DESC, p.created_at ASC
		     LIMIT 1)
		UPDATE tasks t
		   SET primary_pull_request = COALESCE((SELECT key FROM chosen), ''),
		       pull_request_url = COALESCE((SELECT url FROM chosen), ''),
		       pull_request_state = COALESCE((SELECT state FROM chosen), ''),
		       updated_at = CASE WHEN t.primary_pull_request IS DISTINCT FROM COALESCE((SELECT key FROM chosen), '')
		                           OR t.pull_request_state IS DISTINCT FROM COALESCE((SELECT state FROM chosen), '')
		                         THEN now() ELSE t.updated_at END
		 WHERE t.id = $1::uuid`, taskID)
	return err
}

// SetPrimaryPullRequest makes one of a task's pull requests the one its mirror follows.
func (s *Store) SetPrimaryPullRequest(ctx context.Context, taskID domain.ID, repository string, number int) error {
	return s.Tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE tasks t SET primary_pull_request = $2
			 WHERE t.id = $1::uuid AND EXISTS (
			    SELECT 1 FROM pull_requests p WHERE p.task_id = t.id
			       AND lower(p.repository) = lower($3) AND p.number = $4)`,
			taskID, PullRequestKey(repository, number), repository, number)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return mirrorPrimaryTx(ctx, tx, taskID)
	})
}

// ensurePullRequestTx makes sure a row exists for a pull request known only by URL (the
// 0006-era callers), as an open stub the next ingestion fills in. It reports the row.
func ensurePullRequestTx(ctx context.Context, tx pgx.Tx, projectID domain.ID, url string) (PullRequest, error) {
	repository, number, ok := ParsePullRequestURL(url)
	if !ok {
		return PullRequest{}, fmt.Errorf("%w: %q is not a pull request URL", domain.ErrInvalidArgument, url)
	}
	pr, err := getPullRequest(ctx, tx, projectID, repository, number, true)
	if err == nil {
		if pr.URL == "" {
			// Seen before without an address (a listing that carried none): the caller has one.
			if _, err := tx.Exec(ctx, `UPDATE pull_requests SET url = $4
				 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
				projectID, pr.Repository, pr.Number, url); err != nil {
				return PullRequest{}, err
			}
			pr.URL = url
		}
		return pr, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return PullRequest{}, err
	}
	pr, _, err = upsertPullRequestTx(ctx, tx, PullRequestUpsert{ProjectID: projectID, Repository: repository, Number: number, URL: url, State: PullRequestOpen})
	return pr, err
}

// LinkPullRequest records the pull request a task's work travels in, by URL, as the head
// branch rule found it. It is idempotent, and it never moves a merge backwards: a pull
// request already known to have merged stays merged even if a stale poll still lists it as
// open. A closed one seen open again was reopened. It reports whether the link changed.
func (s *Store) LinkPullRequest(ctx context.Context, taskID domain.ID, url string) (bool, error) {
	if url == "" {
		return false, nil
	}
	var linked bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var projectID domain.ID
		if err := tx.QueryRow(ctx, `SELECT project_id::text FROM tasks WHERE id = $1::uuid`, taskID).Scan(&projectID); err != nil {
			return noRows(err)
		}
		pr, err := ensurePullRequestTx(ctx, tx, projectID, url)
		if err != nil {
			return err
		}
		if pr.State == PullRequestClosed {
			if _, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'open', last_seen_at = now()
				 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
				projectID, pr.Repository, pr.Number); err != nil {
				return err
			}
			if pr.TaskID != "" {
				if err := mirrorPrimaryTx(ctx, tx, pr.TaskID); err != nil {
					return err
				}
			}
		}
		out, err := linkPullRequestTx(ctx, tx, projectID, pr.Repository, pr.Number, taskID, LinkBranch)
		if err != nil {
			return err
		}
		// A closed pull request seen open again was reopened, which is news for its task too.
		linked = out.Linked || pr.State == PullRequestClosed
		return nil
	})
	return linked, err
}

// OpenPullRequestTask is a task whose pull request was last seen open.
type OpenPullRequestTask struct {
	TaskID domain.ID
	Ref    string
	URL    string
	// Repository and Number identify the pull request without parsing its URL.
	Repository string
	Number     int
}

// TasksWithOpenPullRequests lists a project's open pull requests that belong to a task that
// is not finished, one entry per pull request. The GitHub poller uses it to notice a merge
// it never received a webhook for: a pull request that drops off the open list was either
// merged or closed.
func (s *Store) TasksWithOpenPullRequests(ctx context.Context, projectID domain.ID) ([]OpenPullRequestTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.ref, p.url, p.repository, p.number
		  FROM pull_requests p JOIN tasks t ON t.id = p.task_id
		 WHERE p.project_id = $1::uuid AND p.state = 'open'
		   AND t.status NOT IN ('done','cancelled','superseded')
		 ORDER BY p.created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenPullRequestTask
	for rows.Next() {
		var t OpenPullRequestTask
		if err := rows.Scan(&t.TaskID, &t.Ref, &t.URL, &t.Repository, &t.Number); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// mergePath is the chain of legal transitions a task walks when its pull request merges.
//
// A merge is the strongest evidence of completion there is — the work is on the default
// branch — so a task completes from wherever its work stood, through the ordinary edges
// rather than around them: a person who claimed, worked, and merged without ever calling a
// finish step still ends at done. A task that was never claimed (proposed, ready) has no
// path: a pull request that merged against an unclaimed task did not do that task's work
// under Conductor, and silently completing it would be a guess.
func mergePath(from domain.TaskStatus) []domain.TaskStatus {
	switch from {
	case domain.TaskVerifying, domain.TaskReviewRequired, domain.TaskMerging:
		return []domain.TaskStatus{domain.TaskDone}
	case domain.TaskRunning:
		return []domain.TaskStatus{domain.TaskVerifying, domain.TaskDone}
	case domain.TaskClaimed, domain.TaskBlockedConflict, domain.TaskBlockedDependency, domain.TaskBlockedInput:
		return []domain.TaskStatus{domain.TaskRunning, domain.TaskVerifying, domain.TaskDone}
	}
	return nil
}

// PullRequestOutcome is what recording a pull request's end did to its task.
type PullRequestOutcome struct {
	TaskRef string            `json:"task_ref,omitempty"`
	From    domain.TaskStatus `json:"from,omitempty"`
	Status  domain.TaskStatus `json:"status,omitempty"`
	// Changed is false when the task was left where it was (already finished, never claimed,
	// a closed pull request against work still in progress, or another of the task's pull
	// requests still open).
	Changed bool `json:"changed"`
	// Recorded is false when this pull request's end was already on record — a redelivered
	// webhook, or the poller seeing the same closed pull request again. Nothing was written
	// and no event should follow.
	Recorded bool `json:"recorded"`
	// OthersOpen counts the task's other pull requests still open after this one ended.
	OthersOpen int `json:"others_open,omitempty"`
}

// EndPullRequest records that a pull request merged (merged true) or closed without
// merging, and applies the task rule:
//
//   - merged: the task completes (done, lease ended, territory released) when the merged
//     pull request is its primary or its last open one; otherwise the merge is recorded and
//     the task waits for the rest.
//   - closed: a task waiting on its pull requests (verifying, review_required, merging)
//     goes back to ready and drops its hold once none of them is open; a task still being
//     worked is left alone. The primary moves to another open pull request if one exists.
//
// Idempotent — an end already on record changes nothing (Recorded is false).
func (s *Store) EndPullRequest(ctx context.Context, projectID domain.ID, repository string, number int, merged bool) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = endPullRequestTx(ctx, tx, projectID, repository, number, merged)
		return err
	})
	return out, err
}

func endPullRequestTx(ctx context.Context, tx pgx.Tx, projectID domain.ID, repository string, number int, merged bool) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	pr, err := getPullRequest(ctx, tx, projectID, repository, number, true)
	if err != nil {
		return out, err
	}
	state := PullRequestClosed
	if merged {
		state = PullRequestMerged
	}
	out.TaskRef, out.From, out.Status = pr.TaskRef, pr.TaskStatus, pr.TaskStatus
	// A merge on record stays merged; a close on record stays closed. A pull request that
	// closed and then turns out merged (GitHub lists merged ones as closed) moves on.
	if pr.State == state || pr.State == PullRequestMerged {
		return out, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET state = $4, last_seen_at = now()
		 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND number = $3`,
		projectID, repository, number, state); err != nil {
		return out, err
	}
	out.Recorded = true
	if pr.TaskID == "" {
		return out, nil
	}
	var from domain.TaskStatus
	var primary string
	if err := tx.QueryRow(ctx, `SELECT status, primary_pull_request FROM tasks WHERE id = $1::uuid FOR UPDATE`, pr.TaskID).Scan(&from, &primary); err != nil {
		return out, noRows(err)
	}
	out.From, out.Status = from, from
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pull_requests
		 WHERE task_id = $1::uuid AND state = 'open'
		   AND NOT (lower(repository) = lower($2) AND number = $3)`,
		pr.TaskID, repository, number).Scan(&out.OthersOpen); err != nil {
		return out, err
	}
	isPrimary := primary == PullRequestKey(pr.Repository, pr.Number)
	switch {
	case merged && (isPrimary || out.OthersOpen == 0):
		if err := completeTx(ctx, tx, pr.TaskID, from, "pull request merged", &out); err != nil {
			return out, err
		}
	case !merged && out.OthersOpen == 0 && PendingMerge(from):
		task, err := updateTaskStatusTx(ctx, tx, pr.TaskID, domain.TaskReady, "pull request closed without merging")
		if err != nil {
			return out, err
		}
		if err := releaseTaskReservationsTx(ctx, tx, pr.TaskID); err != nil {
			return out, err
		}
		out.Status, out.Changed = task.Status, true
	case !merged && isPrimary && out.OthersOpen > 0:
		// The mirror follows a pull request that can still merge.
		if _, err := tx.Exec(ctx, `UPDATE tasks SET primary_pull_request = '' WHERE id = $1::uuid`, pr.TaskID); err != nil {
			return out, err
		}
	}
	return out, mirrorPrimaryTx(ctx, tx, pr.TaskID)
}

// endByURLTx records a pull request's end for a task known only by URL: the row is created
// if it was never seen, and linked to the task if it is unlinked.
func endByURLTx(ctx context.Context, tx pgx.Tx, taskID domain.ID, url string, merged bool) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	var projectID domain.ID
	if err := tx.QueryRow(ctx, `SELECT project_id::text FROM tasks WHERE id = $1::uuid`, taskID).Scan(&projectID); err != nil {
		return out, noRows(err)
	}
	pr, err := ensurePullRequestTx(ctx, tx, projectID, url)
	if err != nil {
		return out, err
	}
	if pr.TaskID == "" {
		if _, err := linkPullRequestTx(ctx, tx, projectID, pr.Repository, pr.Number, taskID, LinkBranch); err != nil {
			return out, err
		}
	}
	return endPullRequestTx(ctx, tx, projectID, pr.Repository, pr.Number, merged)
}

// PullRequestMerged records that a task's pull request merged and completes the task when
// it was the primary or the last open one: done, its lease (if one is still live) ended,
// its territory released. Idempotent — a merge already on record changes nothing (Recorded
// is false).
func (s *Store) PullRequestMerged(ctx context.Context, taskID domain.ID, url string) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = endByURLTx(ctx, tx, taskID, url, true)
		return err
	})
	return out, err
}

// PullRequestClosed records that a task's pull request closed without merging.
//
// The policy, deliberately: a task whose work was waiting on that pull request (verifying,
// review_required, merging) goes back to ready and its pending-merge hold is released once
// none of its pull requests is open — the changes were rejected or abandoned, so nothing is
// in flight any more and the next claimant reserves afresh. A task still being worked (a
// live lease) is left alone: the holder may open another pull request, and the closed one
// is only recorded. A pull request that is reopened is linked again by the next webhook or
// poll.
func (s *Store) PullRequestClosed(ctx context.Context, taskID domain.ID, url string) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = endByURLTx(ctx, tx, taskID, url, false)
		return err
	})
	return out, err
}

// CompleteWork marks a task's work as landed by hand — `conductor task done` on a task that
// is still claimed or running, when it merged without the GitHub App to say so. It is the
// same walk a merge takes (mergePath), so it ends the lease and releases the territory. A
// task already done is left alone; one that was never claimed is refused.
func (s *Store) CompleteWork(ctx context.Context, taskID domain.ID, reason string) (PullRequestOutcome, error) {
	var out PullRequestOutcome
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		var from domain.TaskStatus
		if err := tx.QueryRow(ctx,
			`SELECT ref, status FROM tasks WHERE id = $1::uuid FOR UPDATE`, taskID,
		).Scan(&out.TaskRef, &from); err != nil {
			return noRows(err)
		}
		out.From, out.Status = from, from
		if from == domain.TaskDone {
			return nil
		}
		if len(mergePath(from)) == 0 {
			return fmt.Errorf("%w: %s is %s; only claimed or finished work can be completed",
				domain.ErrIllegalTransition, out.TaskRef, from)
		}
		return completeTx(ctx, tx, taskID, from, reason, &out)
	})
	return out, err
}

// completeTx walks a task from `from` to done along mergePath.
func completeTx(ctx context.Context, tx pgx.Tx, taskID domain.ID, from domain.TaskStatus, reason string, out *PullRequestOutcome) error {
	path := mergePath(from)
	if len(path) == 0 {
		return nil
	}
	task, err := updateTaskStatusTx(ctx, tx, taskID, domain.TaskDone, reason, path[:len(path)-1]...)
	if err != nil {
		return fmt.Errorf("complete %s: %w", out.TaskRef, err)
	}
	out.Status, out.Changed = task.Status, true
	return nil
}

// PrincipalByExternalSubject finds the member of an organization whose linked external
// identity (issuer, subject) is given: for GitHub, the issuer is the GitHub web address and
// the subject the numeric user id. It returns "" when no member is linked to the account.
func (s *Store) PrincipalByExternalSubject(ctx context.Context, orgID domain.ID, issuer, subject string) (domain.ID, error) {
	var id domain.ID
	err := s.pool.QueryRow(ctx, `
		SELECT e.principal_id::text FROM external_identities e
		  JOIN principals p ON p.id = e.principal_id
		 WHERE p.organization_id = $1::uuid AND e.issuer = $2 AND e.subject = $3`,
		orgID, issuer, subject).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// OpenPullRequestNumbers lists the numbers of a repository's pull requests last seen open,
// linked or not. The poller looks up each one that left GitHub's open list to learn how it
// ended.
func (s *Store) OpenPullRequestNumbers(ctx context.Context, projectID domain.ID, repository string) ([]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT number FROM pull_requests
		 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND state = 'open'
		 ORDER BY last_seen_at`, projectID, repository)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// InvalidatePullRequestRollups marks the open pull requests that merge into a branch as
// needing their rollup read again: a push to the base changes whether they are behind or
// conflicting, and nothing on the pull request itself says so.
func (s *Store) InvalidatePullRequestRollups(ctx context.Context, projectID domain.ID, repository, baseRef string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE pull_requests SET rollup_at = NULL
		 WHERE project_id = $1::uuid AND lower(repository) = lower($2) AND state = 'open' AND base_ref = $3`,
		projectID, repository, baseRef)
	return err
}
