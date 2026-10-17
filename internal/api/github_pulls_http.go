package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/githubapp"
	"github.com/aburan28/conductor/internal/privacy"
)

// The pull request list: GET and refresh under a project, and the manual link under a task
// (docs/COLLABORATION.md §4.5, §7). Reads come from the database, so they work whether or not
// a GitHub App is configured; a refresh reads GitHub.
func (s *Server) pullRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("GET /v1/projects/{project}/pulls", auth(s.listPulls))
	m.HandleFunc("GET /v1/projects/{project}/pulls/{owner}/{repo}/{number}", auth(s.getPull))
	m.HandleFunc("POST /v1/projects/{project}/pulls/{owner}/{repo}/{number}/refresh", auth(s.refreshPull))
	m.HandleFunc("POST /v1/tasks/{task}/pull-requests", auth(s.linkPull))
	m.HandleFunc("DELETE /v1/tasks/{task}/pull-requests/{owner}/{repo}/{number}", auth(s.unlinkPull))
}

func pullFacts(row db.PullRequest) privacy.PullRequestFacts {
	updated := row.LastSeenAt
	if row.RemoteUpdatedAt != nil {
		updated = *row.RemoteUpdatedAt
	}
	return privacy.PullRequestFacts{
		Repository: row.Repository, URL: row.URL, State: row.State, Title: row.Title,
		AuthorLogin: row.AuthorLogin, AuthorHandle: row.AuthorHandle, Number: row.Number,
		Draft: row.Draft, Primary: row.Primary, FromFork: row.FromFork,
		TaskRef: row.TaskRef, LinkSource: row.LinkSource, TaskStatus: row.TaskStatus,
		TaskOwner: row.TaskOwner, TaskVisibility: row.TaskVisibility,
		HeadRef: row.HeadRef, HeadSHA: row.HeadSHA, BaseRef: row.BaseRef,
		ChecksTotal: row.ChecksTotal, ChecksPassed: row.ChecksPassed, ChecksFailed: row.ChecksFailed, ChecksPending: row.ChecksPending,
		ReviewDecision: row.ReviewDecision, Mergeable: row.Mergeable, UnresolvedThreads: row.UnresolvedThreads,
		Additions: row.Additions, Deletions: row.Deletions, ChangedFiles: row.ChangedFiles,
		UpdatedAt: updated, CreatedAt: row.CreatedAt,
	}
}

func queryFlag(r *http.Request, key string) bool {
	switch strings.ToLower(r.URL.Query().Get(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// pullTarget reads {owner}/{repo}/{number} from the path.
func pullTarget(r *http.Request) (repository string, number int, err error) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	if !githubapp.ValidRepo(owner, repo) {
		return "", 0, errors.Join(domain.ErrInvalidArgument, errors.New("repository must be owner/name"))
	}
	number, err = strconv.Atoi(r.PathValue("number"))
	if err != nil || number <= 0 {
		return "", 0, errors.Join(domain.ErrInvalidArgument, errors.New("a pull request number is a positive integer"))
	}
	return owner + "/" + repo, number, nil
}

// taskInProject resolves a task named by ref or id in a project.
func (s *Server) taskInProject(ctx context.Context, project domain.Project, ref string) (domain.Task, error) {
	t, err := s.store.GetTaskByRef(ctx, project.ID, ref)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return domain.Task{}, err
	}
	t, err = s.store.GetTask(ctx, ref)
	if err != nil {
		if isBadUUID(err) {
			return domain.Task{}, domain.ErrNotFound
		}
		return domain.Task{}, err
	}
	if t.ProjectID != project.ID {
		return domain.Task{}, domain.ErrNotFound
	}
	return t, nil
}

// listPulls: GET /v1/projects/{project}/pulls?state=open|merged|closed|all &task=T-42
// &unlinked=1 &needs_review=1 &failing=1 &mine=1 &repository= &number= &limit=
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	f := db.PullRequestFilter{Unlinked: queryFlag(r, "unlinked"), NeedsReview: queryFlag(r, "needs_review"),
		Failing: queryFlag(r, "failing"), Repository: q.Get("repository")}
	switch st := q.Get("state"); st {
	case "", "all":
	case db.PullRequestOpen, db.PullRequestMerged, db.PullRequestClosed:
		f.State = st
	default:
		s.fail(w, r, errors.Join(domain.ErrInvalidArgument, fmt.Errorf("state must be open, merged, closed, or all, not %q", st)))
		return
	}
	if queryFlag(r, "mine") {
		f.Mine = caller.Principal.ID
	}
	if n := q.Get("number"); n != "" {
		if f.Number, err = strconv.Atoi(n); err != nil || f.Number <= 0 {
			s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("number must be a positive integer")))
			return
		}
	}
	if l := q.Get("limit"); l != "" {
		if f.Limit, err = strconv.Atoi(l); err != nil || f.Limit < 0 {
			s.fail(w, r, errors.Join(domain.ErrInvalidArgument, errors.New("limit must be a number")))
			return
		}
	}
	if t := q.Get("task"); t != "" {
		task, err := s.taskInProject(r.Context(), project, t)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		f.TaskID = task.ID
	}
	rows, err := s.store.ListPullRequests(r.Context(), project.ID, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	views := make([]privacy.PullRequestView, 0, len(rows))
	viewer := caller.Viewer()
	for _, row := range rows {
		views = append(views, privacy.ProjectPullRequest(viewer, pullFacts(row)))
	}
	s.ok(w, r, http.StatusOK, map[string]any{"pulls": views})
}

// pullDetail is one pull request with the task it belongs to and that task's attempts, as
// the viewer may see them.
type pullDetail struct {
	Pull     privacy.PullRequestView `json:"pull"`
	Task     *privacy.TaskView       `json:"task,omitempty"`
	Attempts []privacy.AttemptView   `json:"attempts,omitempty"`
}

func (s *Server) pullDetail(ctx context.Context, caller coord.Caller, row db.PullRequest) (pullDetail, error) {
	out := pullDetail{Pull: privacy.ProjectPullRequest(caller.Viewer(), pullFacts(row))}
	if row.TaskID == "" || out.Pull.Redacted {
		return out, nil
	}
	tv, err := s.svc.TaskView(ctx, caller, row.TaskID)
	if err != nil {
		return out, err
	}
	out.Task = &tv
	if tv.Redacted {
		return out, nil
	}
	attempts, err := s.svc.AttemptViews(ctx, caller, row.TaskID)
	if err != nil {
		return out, err
	}
	out.Attempts = attempts
	return out, nil
}

// getPull: GET /v1/projects/{project}/pulls/{owner}/{repo}/{number}
func (s *Server) getPull(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	repository, number, err := pullTarget(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row, err := s.store.GetPullRequest(r.Context(), project.ID, repository, number)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	detail, err := s.pullDetail(r.Context(), caller, row)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, detail)
}

// pullRefreshLimiter bounds POST .../refresh, each of which costs GitHub API calls: a pull
// request is read at most once every pullRefreshGap, and one caller at most
// pullRefreshPerMinute times a minute. In-process, like the other limiters here.
type pullRefreshLimiter struct {
	mu     sync.Mutex
	last   map[string]time.Time
	caller map[string][]time.Time
}

const (
	pullRefreshGap       = 10 * time.Second
	pullRefreshPerMinute = 20
	pullRefreshMaxKeys   = 4096
)

func (l *pullRefreshLimiter) allow(who, key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil || len(l.last) > pullRefreshMaxKeys {
		l.last, l.caller = map[string]time.Time{}, map[string][]time.Time{}
	}
	if at, ok := l.last[key]; ok && now.Sub(at) < pullRefreshGap {
		return false, pullRefreshGap - now.Sub(at)
	}
	recent := l.caller[who][:0:0]
	for _, at := range l.caller[who] {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	if len(recent) >= pullRefreshPerMinute {
		return false, time.Minute - now.Sub(recent[0])
	}
	l.last[key] = now
	l.caller[who] = append(recent, now)
	return true, 0
}

// refreshPull: POST /v1/projects/{project}/pulls/{owner}/{repo}/{number}/refresh reads the
// pull request and its rollup from GitHub now.
func (s *Server) refreshPull(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	repository, number, err := pullTarget(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var c *githubapp.Client
	if s.github != nil {
		c = s.github.current(r.Context())
	}
	if c == nil {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_configured", Error: "no GitHub App is configured; run `conductor github setup`"})
		return
	}
	owner, name, _ := strings.Cut(repository, "/")
	linked, err := s.github.projectsFor(r.Context(), owner, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	inProject := false
	for _, lp := range linked {
		inProject = inProject || lp.ID == project.ID
	}
	if !inProject {
		s.fail(w, r, errors.Join(domain.ErrNotFound, fmt.Errorf("project %s is not linked to %s; run `conductor github link` in its checkout", project.Slug, repository)))
		return
	}
	if ok, wait := s.github.refresh.allow(p.ID, project.ID+"/"+strings.ToLower(repository)+"#"+strconv.Itoa(number), time.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.ok(w, r, http.StatusTooManyRequests, ErrorBody{Code: "rate_limited", Error: "pull requests can be refreshed from GitHub every few seconds; try again shortly"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	inst, err := c.RepoInstallation(ctx, owner, name)
	if err != nil {
		s.ok(w, r, http.StatusConflict, ErrorBody{Code: "not_installed", Error: "the GitHub App is not installed on " + repository + ": " + err.Error()})
		return
	}
	pr, err := c.GetPullRequest(ctx, inst, owner, name, number)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if pr.State == "closed" {
		_, err = s.github.applyPullClosed(ctx, inst, project, pr)
	} else {
		_, err = s.github.recordPullIn(ctx, inst, project, repository, pr, rollupAlways, false)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	row, err := s.store.GetPullRequest(ctx, project.ID, repository, number)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	detail, err := s.pullDetail(ctx, caller, row)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, detail)
}

var (
	pullURLRef = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/([0-9]+)/?$`)
	pullKeyRef = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)?#?([0-9]+)$`)
)

// resolvePullRef reads a pull request named as a URL, owner/repo#N, #N, or N, with a bare
// number meaning the project's own repository. A project linked to a repository only holds
// that repository's pull requests.
func resolvePullRef(project domain.Project, repository string, number int, ref string) (string, int, error) {
	own := ""
	if o, n, ok := githubapp.ParseRemote(project.CanonicalRemote); ok {
		own = o + "/" + n
	}
	ref = strings.TrimSpace(ref)
	switch {
	case ref != "":
		if m := pullURLRef.FindStringSubmatch(ref); m != nil {
			repository, number = m[1], atoiOr(m[2])
		} else if m := pullKeyRef.FindStringSubmatch(ref); m != nil {
			repository, number = m[1], atoiOr(m[2])
		} else {
			return "", 0, errors.Join(domain.ErrInvalidArgument, fmt.Errorf("%q is not a pull request: use a URL, owner/repo#N, or a number", ref))
		}
	}
	if repository == "" {
		repository = own
	}
	owner, name, found := strings.Cut(repository, "/")
	if !found || !githubapp.ValidRepo(owner, name) {
		return "", 0, errors.Join(domain.ErrInvalidArgument, errors.New("name the repository as owner/name: this project is not linked to one"))
	}
	if number <= 0 {
		return "", 0, errors.Join(domain.ErrInvalidArgument, errors.New("a pull request number is a positive integer"))
	}
	if own != "" && !strings.EqualFold(own, repository) {
		return "", 0, errors.Join(domain.ErrInvalidArgument, fmt.Errorf("this project's repository is %s, not %s", own, repository))
	}
	return repository, number, nil
}

func atoiOr(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// ensurePull makes sure a row exists for a pull request a person named. With a GitHub App it
// is read from GitHub (a pull request that does not exist is an error); without one, or on
// a repository the app is not installed on, a bare row stands for it until a poll or a
// webhook fills it in.
func (s *Server) ensurePull(ctx context.Context, project domain.Project, repository string, number int) (db.PullRequest, error) {
	row, err := s.store.GetPullRequest(ctx, project.ID, repository, number)
	if err == nil || !errors.Is(err, domain.ErrNotFound) {
		return row, err
	}
	owner, name, _ := strings.Cut(repository, "/")
	if s.github != nil {
		if c := s.github.current(ctx); c != nil {
			if inst, ierr := c.RepoInstallation(ctx, owner, name); ierr == nil {
				pr, gerr := c.GetPullRequest(ctx, inst, owner, name, number)
				var apiErr *githubapp.APIError
				if errors.As(gerr, &apiErr) && apiErr.NotFound() {
					return db.PullRequest{}, errors.Join(domain.ErrNotFound, fmt.Errorf("%s#%d is not a pull request", repository, number))
				}
				if gerr == nil {
					if pr.State == "closed" {
						_, gerr = s.github.applyPullClosed(ctx, inst, project, pr)
					} else {
						_, gerr = s.github.recordPullIn(ctx, inst, project, repository, pr, rollupAlways, false)
					}
					if gerr == nil {
						return s.store.GetPullRequest(ctx, project.ID, repository, number)
					}
				}
			}
		}
	}
	web := githubapp.DefaultWeb
	if s.github != nil {
		web = s.github.web()
	}
	row, _, err = s.store.UpsertPullRequest(ctx, db.PullRequestUpsert{ProjectID: project.ID, Repository: repository, Number: number,
		URL: web + "/" + repository + "/pull/" + strconv.Itoa(number), State: db.PullRequestOpen})
	return row, err
}

// authorizeLink checks that the caller may change a task's pull request links: a contributor
// may, except on someone else's private task, which only its owner or a maintainer may.
func (s *Server) authorizeLink(task domain.Task, caller coord.Caller) error {
	if task.Visibility == domain.VisibilityPrivate && task.CreatedBy != caller.Principal.ID && !caller.Role.Can(domain.RoleMaintainer) {
		return errors.Join(domain.ErrNotPermitted, errors.New("only the owner of a private task, or a maintainer, can link a pull request to it"))
	}
	return nil
}

// linkPull: POST /v1/tasks/{task}/pull-requests {pull_request: "owner/repo#118" | URL | "118"}
// (or {repository, number}). A manual link outranks every automatic one.
func (s *Server) linkPull(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.authorizeLink(task, caller); err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		PullRequest string `json:"pull_request"`
		Repository  string `json:"repository"`
		Number      int    `json:"number"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), task.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	repository, number, err := resolvePullRef(project, body.Repository, body.Number, body.PullRequest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := s.ensurePull(ctx, project, repository, number); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.store.LinkPullRequestTask(ctx, project.ID, repository, number, task.ID, db.LinkManual)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(ctx, project.OrganizationID, project.ID, p.ID, "pull_request.linked", "task", task.ID,
		map[string]any{"task_ref": task.Ref, "repository": repository, "number": number, "link_source": db.LinkManual})
	row, err := s.store.GetPullRequest(ctx, project.ID, repository, number)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Linked {
		status = http.StatusCreated
	}
	s.ok(w, r, status, map[string]any{"linked": res.Linked, "task_ref": task.Ref,
		"pull": privacy.ProjectPullRequest(caller.Viewer(), pullFacts(row))})
}

// unlinkPull: DELETE /v1/tasks/{task}/pull-requests/{owner}/{repo}/{number}. The unlink
// sticks: automatic sources do not link the pull request again.
func (s *Server) unlinkPull(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.authorizeLink(task, caller); err != nil {
		s.fail(w, r, err)
		return
	}
	repository, number, err := pullTarget(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done, err := s.store.UnlinkPullRequestTask(r.Context(), task.ProjectID, repository, number, task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !done {
		s.fail(w, r, errors.Join(domain.ErrNotFound, fmt.Errorf("%s#%d is not linked to %s", repository, number, task.Ref)))
		return
	}
	project, err := s.store.GetProject(r.Context(), task.ProjectID)
	if err == nil {
		s.store.Audit(r.Context(), project.OrganizationID, project.ID, p.ID, "pull_request.unlinked", "task", task.ID,
			map[string]any{"task_ref": task.Ref, "repository": repository, "number": number})
	}
	s.ok(w, r, http.StatusOK, map[string]any{"unlinked": true, "task_ref": task.Ref})
}
