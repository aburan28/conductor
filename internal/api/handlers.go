package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/privacy"
	"github.com/adamburan/conductor/internal/taskcard"
)

func (s *Server) routes() {
	m := s.mux
	auth := s.authenticate

	m.HandleFunc("GET /v1/health", s.health)
	m.HandleFunc("GET /v1/whoami", auth(s.whoami))

	m.HandleFunc("GET /v1/projects", auth(s.listProjects))
	m.HandleFunc("GET /v1/projects/{project}", auth(s.getProject))
	m.HandleFunc("GET /v1/projects/{project}/members", auth(s.listMembers))
	m.HandleFunc("POST /v1/projects/{project}/members", auth(s.inviteMember))
	m.HandleFunc("DELETE /v1/projects/{project}/members/{handle}", auth(s.removeMember))

	m.HandleFunc("GET /v1/tokens", auth(s.listTokens))
	m.HandleFunc("POST /v1/tokens", auth(s.createToken))
	m.HandleFunc("DELETE /v1/tokens/{name}", auth(s.revokeToken))
	m.HandleFunc("POST /v1/tokens/revoke-all", auth(s.revokeAllTokens))

	m.HandleFunc("POST /v1/projects/{project}/sessions", auth(s.registerSession))
	m.HandleFunc("POST /v1/sessions/{session}/heartbeat", auth(s.heartbeatSession))
	m.HandleFunc("POST /v1/sessions/{session}/close", auth(s.closeSession))

	m.HandleFunc("POST /v1/projects/{project}/intents/check", auth(s.checkIntent))
	m.HandleFunc("POST /v1/projects/{project}/work/start", auth(s.startWork))

	m.HandleFunc("GET /v1/projects/{project}/tasks", auth(s.listTasks))
	m.HandleFunc("POST /v1/projects/{project}/tasks", auth(s.createTask))
	m.HandleFunc("GET /v1/tasks/{task}", auth(s.getTask))
	m.HandleFunc("PATCH /v1/tasks/{task}", auth(s.patchTask))
	m.HandleFunc("GET /v1/tasks/{task}/card", auth(s.getTaskCard))
	m.HandleFunc("POST /v1/tasks/{task}/claim", auth(s.claimTask))
	m.HandleFunc("POST /v1/tasks/{task}/release", auth(s.releaseTask))
	m.HandleFunc("POST /v1/tasks/{task}/transition", auth(s.transitionTask))
	m.HandleFunc("POST /v1/tasks/{task}/handoff", auth(s.createHandoff))
	m.HandleFunc("GET /v1/tasks/{task}/handoff", auth(s.getHandoff))
	m.HandleFunc("POST /v1/tasks/{task}/scopes", auth(s.expandScope))
	m.HandleFunc("POST /v1/projects/{project}/claim-next", auth(s.claimNext))

	m.HandleFunc("GET /v1/projects/{project}/reservations", auth(s.listReservations))
	m.HandleFunc("DELETE /v1/reservations/{reservation}", auth(s.releaseReservation))

	m.HandleFunc("POST /v1/leases/heartbeat", auth(s.heartbeatLease))
	m.HandleFunc("POST /v1/attempts/{attempt}/progress", auth(s.reportProgress))
	m.HandleFunc("POST /v1/attempts/{attempt}/result", auth(s.finishWork))

	m.HandleFunc("GET /v1/projects/{project}/presence", auth(s.presence))
	m.HandleFunc("GET /v1/projects/{project}/status", auth(s.status))
	m.HandleFunc("GET /v1/projects/{project}/conflicts", auth(s.listConflicts))
	m.HandleFunc("POST /v1/conflicts/{conflict}/resolve", auth(s.resolveConflict))

	m.HandleFunc("GET /v1/projects/{project}/events", auth(s.listEvents))
	m.HandleFunc("GET /v1/projects/{project}/events/stream", auth(s.streamEvents))

	m.HandleFunc("POST /v1/runners/register", auth(s.registerRunner))
	m.HandleFunc("POST /v1/runners/{runner}/heartbeat", auth(s.heartbeatRunner))

	m.HandleFunc("GET /v1/projects/{project}/runner/snapshot", auth(s.runnerSnapshot))
	m.HandleFunc("GET /v1/attempts/{attempt}/brief", auth(s.attemptBrief))
	m.HandleFunc("POST /v1/attempts/{attempt}/route", auth(s.setAttemptRoute))
	m.HandleFunc("POST /v1/attempts/{attempt}/state", auth(s.setAttemptState))

	if len(s.dashboard) > 0 {
		m.HandleFunc("GET /{$}", s.serveDashboard)
		m.HandleFunc("GET /dashboard", s.serveDashboard)
	}
}

// ---------------------------------------------------------------------------
// Health and identity
// ---------------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		s.ok(w, r, http.StatusServiceUnavailable,
			map[string]any{"status": "degraded", "database": err.Error()})
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC()})
}

func (s *Server) whoami(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	projects, err := s.store.ListProjectsFor(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type projectRef struct {
		ID   domain.ID   `json:"id"`
		Slug string      `json:"slug"`
		Role domain.Role `json:"role"`
	}
	refs := make([]projectRef, 0, len(projects))
	for _, pr := range projects {
		role, _ := s.store.RoleIn(r.Context(), pr.ID, p.ID)
		refs = append(refs, projectRef{ID: pr.ID, Slug: pr.Slug, Role: role})
	}
	s.ok(w, r, http.StatusOK, map[string]any{"principal": p, "projects": refs})
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	projects, err := s.store.ListProjectsFor(r.Context(), p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, project)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	members, err := s.store.ListMembers(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ids := make([]domain.ID, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.PrincipalID)
	}
	principals, err := s.store.PrincipalsByID(r.Context(), ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type memberView struct {
		Handle string               `json:"handle"`
		Kind   domain.PrincipalKind `json:"kind"`
		Role   domain.Role          `json:"role"`
	}
	out := make([]memberView, 0, len(members))
	for _, m := range members {
		pr := principals[m.PrincipalID]
		out = append(out, memberView{Handle: pr.Handle, Kind: pr.Kind, Role: m.Role})
	}
	s.ok(w, r, http.StatusOK, map[string]any{"members": out})
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

type registerSessionBody struct {
	Harness        string            `json:"harness"`
	HarnessVersion string            `json:"harness_version"`
	MachineID      string            `json:"machine_id"`
	BaseSHA        string            `json:"base_sha"`
	Branch         string            `json:"branch"`
	WorktreePath   string            `json:"worktree_path"`
	Visibility     domain.Visibility `json:"visibility"`
	RunnerID       domain.ID         `json:"runner_id"`
}

func (s *Server) registerSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body registerSessionBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Harness == "" {
		body.Harness = "cli"
	}
	session, err := s.store.RegisterSession(r.Context(), db.RegisterSessionParams{
		ProjectID: project.ID, PrincipalID: p.ID, RunnerID: body.RunnerID,
		Harness: body.Harness, HarnessVersion: body.HarnessVersion,
		MachineID: body.MachineID, BaseSHA: body.BaseSHA, Branch: body.Branch,
		WorktreePath: body.WorktreePath, Visibility: body.Visibility,
		TTL: project.Config.LeaseTTL.OrDefault(90 * time.Second),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, session)
}

type heartbeatSessionBody struct {
	State   domain.SessionState `json:"state"`
	Branch  string              `json:"branch"`
	BaseSHA string              `json:"base_sha"`
}

func (s *Server) heartbeatSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("session"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if session.PrincipalID != p.ID {
		s.fail(w, r, domain.ErrNotPermitted)
		return
	}
	var body heartbeatSessionBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), session.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.store.HeartbeatSession(r.Context(), db.HeartbeatSessionParams{
		SessionID: session.ID, State: body.State, Branch: body.Branch, BaseSHA: body.BaseSHA,
		TTL: project.Config.LeaseTTL.OrDefault(90 * time.Second),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, updated)
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("session"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if session.PrincipalID != p.ID {
		s.fail(w, r, domain.ErrNotPermitted)
		return
	}
	if err := s.store.CloseSession(r.Context(), session.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusNoContent, nil)
}

// ---------------------------------------------------------------------------
// Intent and work
// ---------------------------------------------------------------------------

type intentBody struct {
	Summary     string                `json:"summary"`
	ExternalRef string                `json:"external_ref"`
	Visibility  domain.Visibility     `json:"visibility"`
	Scopes      []domain.ScopeRequest `json:"scopes"`
	Fingerprint string                `json:"intent_fingerprint"`
	Verb        string                `json:"verb"`
	Resource    string                `json:"resource"`
	SessionID   domain.ID             `json:"session_id"`
	ExcludeTask domain.ID             `json:"exclude_task"`
}

func (b intentBody) toRequest(projectID domain.ID) coord.IntentRequest {
	return coord.IntentRequest{
		ProjectID: projectID, SessionID: b.SessionID, ExternalRef: b.ExternalRef,
		Summary: b.Summary, Visibility: b.Visibility, Scopes: b.Scopes,
		Fingerprint: b.Fingerprint, Verb: b.Verb, Resource: b.Resource,
		ExcludeTask: b.ExcludeTask,
	}
}

func (s *Server) checkIntent(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body intentBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	decision, err := s.svc.CheckIntent(r.Context(), caller, body.toRequest(project.ID))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, decision)
}

type startWorkBody struct {
	intentBody
	Title              string                       `json:"title"`
	AttachTo           domain.ID                    `json:"attach_to"`
	Force              bool                         `json:"force"`
	Harness            string                       `json:"harness"`
	ModelAlias         string                       `json:"model_alias"`
	Role               domain.AgentRole             `json:"role"`
	Branch             string                       `json:"branch"`
	BaseSHA            string                       `json:"base_sha"`
	WorktreePath       string                       `json:"worktree_path"`
	Priority           int                          `json:"priority"`
	AcceptanceCriteria []domain.AcceptanceCriterion `json:"acceptance_criteria"`
}

func (s *Server) startWork(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body startWorkBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.idempotent(w, r, p) {
		return
	}

	result, err := s.svc.StartWork(r.Context(), caller, coord.StartWorkRequest{
		IntentRequest:      body.toRequest(project.ID),
		Title:              body.Title,
		AttachTo:           body.AttachTo,
		Force:              body.Force,
		Harness:            body.Harness,
		ModelAlias:         body.ModelAlias,
		Role:               body.Role,
		Branch:             body.Branch,
		BaseSHA:            body.BaseSHA,
		WorktreePath:       body.WorktreePath,
		Priority:           body.Priority,
		AcceptanceCriteria: body.AcceptanceCriteria,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// A blocked outcome is a successful answer to "may I?", not a server error. The caller
	// gets 409 so clients that only check status still stop, plus the full explanation.
	status := http.StatusOK
	if result.Outcome.Blocks() {
		status = http.StatusConflict
	}
	s.remember(r, p, status, result)
	s.ok(w, r, status, result)
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	filter := db.ListTasksFilter{
		OpenOnly: r.URL.Query().Get("open") == "true",
		Limit:    intParam(r, "limit", 200),
	}
	for _, st := range r.URL.Query()["status"] {
		filter.Statuses = append(filter.Statuses, domain.TaskStatus(st))
	}
	views, err := s.svc.ListTaskViews(r.Context(), caller, project.ID, filter)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"tasks": views})
}

type createTaskBody struct {
	Title              string                       `json:"title"`
	Objective          string                       `json:"objective"`
	ExternalRef        string                       `json:"external_ref"`
	Status             domain.TaskStatus            `json:"status"`
	Visibility         domain.Visibility            `json:"visibility"`
	Priority           int                          `json:"priority"`
	RiskLevel          domain.RiskLevel             `json:"risk_level"`
	ModelAlias         string                       `json:"model_alias"`
	HarnessPref        string                       `json:"harness"`
	DependsOn          []domain.ID                  `json:"depends_on"`
	AcceptanceCriteria []domain.AcceptanceCriterion `json:"acceptance_criteria"`
	Scopes             []domain.ScopeRequest        `json:"scopes"`
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body createTaskBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.idempotent(w, r, p) {
		return
	}
	if body.Status == "" {
		body.Status = domain.TaskProposed
	}

	// Compute the coordination fingerprint so the task participates in duplicate detection
	// even when it was filed directly rather than through start-work.
	key, err := s.store.DedupeKeyForProject(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	scopeStrings := make([]string, 0, len(body.Scopes))
	for _, sc := range body.Scopes {
		scopeStrings = append(scopeStrings, sc.Resource)
	}
	env := privacy.Envelope{
		Summary:     body.Title + " " + body.Objective,
		Scopes:      scopeStrings,
		ExternalRef: body.ExternalRef,
	}

	task, err := s.store.CreateTask(r.Context(), db.CreateTaskParams{
		ProjectID: project.ID, CreatedBy: p.ID,
		ExternalRef:        body.ExternalRef,
		Title:              privacy.ClampSummary(body.Title),
		Objective:          privacy.ClampSummary(body.Objective),
		AcceptanceCriteria: body.AcceptanceCriteria,
		Status:             body.Status,
		Visibility:         body.Visibility,
		Priority:           body.Priority,
		RiskLevel:          body.RiskLevel,
		ModelAlias:         body.ModelAlias,
		HarnessPref:        body.HarnessPref,
		MaxAttempts:        project.Config.MaxAttempts,
		DependsOn:          body.DependsOn,
		Fingerprint:        env.Fingerprint(key),
		MinHash:            env.Signature(key),
		WorkflowSHA:        project.WorkflowSHA,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// Declared scopes are recorded as planned reservations so the conflict radar sees the
	// task's intended territory before anyone claims it.
	if len(body.Scopes) > 0 {
		if _, _, err := s.store.AcquireScopes(r.Context(), db.AcquireScopesParams{
			ProjectID: project.ID, TaskID: task.ID, PrincipalID: p.ID,
			Requests: body.Scopes, Source: domain.SourcePlanned,
			Policy: config.ScopePolicyFrom(project.Config), AllowWarnings: true,
		}); err != nil && !errors.Is(err, domain.ErrConflict) {
			s.fail(w, r, err)
			return
		}
	}

	view, err := s.svc.TaskView(r.Context(), caller, task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.remember(r, p, http.StatusCreated, view)
	s.ok(w, r, http.StatusCreated, view)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
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
	s.ok(w, r, http.StatusOK, view)
}

func (s *Server) getTaskCard(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), task.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	owner, err := s.store.GetPrincipal(r.Context(), task.CreatedBy)
	if err != nil {
		s.fail(w, r, err)
		return
	}

	// A card for someone else's private task exposes territory only.
	if task.Visibility == domain.VisibilityPrivate && owner.ID != caller.Principal.ID {
		task.Title, task.Objective, task.ExternalRef = "(private)", "", ""
		task.AcceptanceCriteria = nil
	}

	reservations, err := s.store.ReservationsForTask(r.Context(), task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var attempt *domain.Attempt
	if a, err := s.store.ActiveAttempt(r.Context(), task.ID); err == nil {
		attempt = &a
	}
	var lease *domain.Lease
	if l, err := s.store.ActiveLeaseForTask(r.Context(), task.ID); err == nil {
		lease = &l
	}

	card := taskcard.FromTask(task, project.Slug, owner.Handle, attempt, lease,
		reservations, task.DependsOn, project.Config.RequiredChecks)
	rendered, err := card.Render()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(rendered))
}

type patchTaskBody struct {
	Title      *string            `json:"title"`
	Objective  *string            `json:"objective"`
	Priority   *int               `json:"priority"`
	RiskLevel  *domain.RiskLevel  `json:"risk_level"`
	Visibility *domain.Visibility `json:"visibility"`
	ModelAlias *string            `json:"model_alias"`
}

func (s *Server) patchTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body patchTaskBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	// Only the owner or a maintainer may change a task's visibility; otherwise anyone could
	// downgrade someone else's private work to team-visible.
	if body.Visibility != nil && task.CreatedBy != p.ID && !caller.Role.Can(domain.RoleMaintainer) {
		s.fail(w, r, fmt.Errorf("%w: only the owner or a maintainer may change visibility",
			domain.ErrNotPermitted))
		return
	}
	if _, err := s.store.PatchTask(r.Context(), task.ID, db.TaskPatch{
		Title: body.Title, Objective: body.Objective, Priority: body.Priority,
		RiskLevel: body.RiskLevel, Visibility: body.Visibility, ModelAlias: body.ModelAlias,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.TaskView(r.Context(), caller, task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, view)
}

type claimBody struct {
	SessionID       domain.ID             `json:"session_id"`
	RunnerID        domain.ID             `json:"runner_id"`
	Harness         string                `json:"harness"`
	ModelAlias      string                `json:"model_alias"`
	ResolvedModel   string                `json:"resolved_model"`
	ReasoningEffort domain.Effort         `json:"reasoning_effort"`
	Role            domain.AgentRole      `json:"role"`
	Branch          string                `json:"branch"`
	BaseSHA         string                `json:"base_sha"`
	WorktreePath    string                `json:"worktree_path"`
	Scopes          []domain.ScopeRequest `json:"scopes"`
	AllowWarnings   bool                  `json:"allow_warnings"`
}

func (s *Server) claimTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body claimBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), task.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.store.Claim(r.Context(), db.ClaimParams{
		TaskID: task.ID, ProjectID: project.ID,
		HolderPrincipal: p.ID, SponsorPrincipal: p.ID,
		SessionID: body.SessionID, RunnerID: body.RunnerID,
		Role: body.Role, Harness: firstNonEmpty(body.Harness, "cli"),
		ModelAlias: body.ModelAlias, ResolvedModel: body.ResolvedModel,
		ReasoningEffort: body.ReasoningEffort,
		Branch:          body.Branch, WorktreePath: body.WorktreePath, BaseCommitSHA: body.BaseSHA,
		WorkflowSHA: project.WorkflowSHA, ProjectConfigSHA: project.ConfigSHA,
		LeaseTTL: project.Config.LeaseTTL.OrDefault(90 * time.Second),
		Scopes:   body.Scopes, ScopePolicy: config.ScopePolicyFrom(project.Config),
		AllowWarnings: body.AllowWarnings,
	})
	if err != nil {
		if errors.Is(err, domain.ErrConflict) {
			s.ok(w, r, http.StatusConflict, map[string]any{
				"error": err.Error(), "code": "scope_conflict",
				"conflicts": result.Warnings,
			})
			return
		}
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.TaskView(r.Context(), caller, result.Task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{
		"task": view, "attempt": result.Attempt, "lease": result.Lease,
		"fence": result.Fence, "reservations": result.Reservations,
		"warnings": result.Warnings,
	})
}

func (s *Server) claimNext(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body claimBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.store.ClaimNext(r.Context(), db.ClaimNextParams{
		ClaimParams: db.ClaimParams{
			ProjectID: project.ID, HolderPrincipal: p.ID, SponsorPrincipal: p.ID,
			SessionID: body.SessionID, RunnerID: body.RunnerID,
			Role: body.Role, Harness: firstNonEmpty(body.Harness, "cli"),
			ModelAlias: body.ModelAlias, ReasoningEffort: body.ReasoningEffort,
			WorkflowSHA: project.WorkflowSHA, ProjectConfigSHA: project.ConfigSHA,
			LeaseTTL:    project.Config.LeaseTTL.OrDefault(90 * time.Second),
			ScopePolicy: config.ScopePolicyFrom(project.Config), AllowWarnings: true,
		},
		MaxCandidates: 10,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.TaskView(r.Context(), caller, result.Task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{
		"task": view, "attempt": result.Attempt, "lease": result.Lease,
		"fence": result.Fence, "reservations": result.Reservations,
	})
}

type fenceBody struct {
	AttemptID    domain.ID `json:"attempt_id"`
	LeaseID      domain.ID `json:"lease_id"`
	FencingEpoch int64     `json:"fencing_epoch"`
	TaskID       domain.ID `json:"task_id"`
}

func (b fenceBody) fence() domain.Fence {
	return domain.Fence{
		TaskID: b.TaskID, AttemptID: b.AttemptID,
		LeaseID: b.LeaseID, FencingEpoch: b.FencingEpoch,
	}
}

type releaseBody struct {
	fenceBody
	Reason         string              `json:"reason"`
	NextStatus     domain.TaskStatus   `json:"next_status"`
	AttemptState   domain.AttemptState `json:"attempt_state"`
	FailureClass   string              `json:"failure_class"`
	FailureSummary string              `json:"failure_summary"`
	Note           string              `json:"note"`
}

func (s *Server) releaseTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, _, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body releaseBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := body.fence()
	fence.TaskID = task.ID
	if fence.LeaseID == "" {
		lease, err := s.store.ActiveLeaseForTask(r.Context(), task.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		fence = domain.Fence{
			TaskID: task.ID, AttemptID: lease.AttemptID,
			LeaseID: lease.ID, FencingEpoch: lease.FencingEpoch,
		}
	}
	updated, err := s.store.Release(r.Context(), db.ReleaseParams{
		Fence: fence, Reason: firstNonEmpty(body.Reason, body.Note),
		NextTaskStatus: body.NextStatus, AttemptState: body.AttemptState,
		FailureClass:   body.FailureClass,
		FailureSummary: privacy.ClampSummary(body.FailureSummary),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, updated)
}

type transitionBody struct {
	Status domain.TaskStatus `json:"status"`
}

func (s *Server) transitionTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body transitionBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.store.UpdateTaskStatus(r.Context(), task.ID, body.Status); err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.svc.TaskView(r.Context(), caller, task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, view)
}

// ---------------------------------------------------------------------------
// Scopes
// ---------------------------------------------------------------------------

type expandScopeBody struct {
	fenceBody
	Scopes []domain.ScopeRequest    `json:"scopes"`
	Source domain.ReservationSource `json:"source"`
}

func (s *Server) expandScope(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body expandScopeBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := body.fence()
	fence.TaskID = task.ID
	if fence.LeaseID == "" {
		lease, err := s.store.ActiveLeaseForTask(r.Context(), task.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		fence = domain.Fence{TaskID: task.ID, AttemptID: lease.AttemptID,
			LeaseID: lease.ID, FencingEpoch: lease.FencingEpoch}
	}
	result, err := s.svc.ExpandScope(r.Context(), caller, fence, task.ProjectID,
		body.Scopes, body.Source)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if result.Outcome.Blocks() {
		status = http.StatusConflict
	}
	s.ok(w, r, status, result)
}

func (s *Server) listReservations(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reservations, err := s.store.ListReservations(r.Context(), project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"reservations": reservations})
}

func (s *Server) releaseReservation(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	id := r.PathValue("reservation")
	// Releasing someone else's reservation would let one principal strip another's
	// protection, so ownership is checked rather than assumed.
	var ownerID, projectID domain.ID
	err := s.store.Pool().QueryRow(r.Context(),
		`SELECT principal_id::text, project_id::text FROM scope_reservations WHERE id = $1::uuid`,
		id).Scan(&ownerID, &projectID)
	if err != nil {
		s.fail(w, r, domain.ErrNotFound)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, projectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ownerID != p.ID && !caller.Role.Can(domain.RoleMaintainer) {
		s.fail(w, r, fmt.Errorf("%w: reservation belongs to another principal", domain.ErrNotPermitted))
		return
	}
	if err := s.store.ReleaseReservation(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusNoContent, nil)
}

// ---------------------------------------------------------------------------
// Leases, progress, results
// ---------------------------------------------------------------------------

type heartbeatLeaseBody struct {
	fenceBody
}

func (s *Server) heartbeatLease(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body heartbeatLeaseBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := body.fence()
	lease, err := s.store.GetLease(r.Context(), fence.LeaseID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.svc.Authorize(r.Context(), p, lease.ProjectID, domain.RoleContributor); err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), lease.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.store.HeartbeatLease(r.Context(), fence,
		project.Config.LeaseTTL.OrDefault(90*time.Second))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, updated)
}

type progressBody struct {
	fenceBody
	Phase        string   `json:"phase"`
	Summary      string   `json:"summary"`
	PercentHint  int      `json:"percent_hint"`
	Blocker      string   `json:"blocker"`
	ChangedPaths []string `json:"changed_paths"`
	TokensIn     int64    `json:"tokens_in"`
	TokensOut    int64    `json:"tokens_out"`
	CostUSD      float64  `json:"cost_usd"`
	Turns        int      `json:"turns"`
}

func (s *Server) reportProgress(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body progressBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := fenceFrom(r, body)
	attempt, err := s.store.GetAttempt(r.Context(), fence.AttemptID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fence.TaskID = attempt.TaskID
	caller, err := s.svc.Authorize(r.Context(), p, attempt.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.ReportProgress(r.Context(), caller, fence, attempt.ProjectID,
		coord.ProgressReport{
			Phase: body.Phase, Summary: body.Summary, PercentHint: body.PercentHint,
			Blocker: body.Blocker, ChangedPaths: body.ChangedPaths,
			TokensIn: body.TokensIn, TokensOut: body.TokensOut,
			CostUSD: body.CostUSD, Turns: body.Turns,
		}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusAccepted, map[string]any{"status": "recorded"})
}

type finishBody struct {
	fenceBody
	Outcome        string                   `json:"outcome"`
	FailureClass   string                   `json:"failure_class"`
	FailureSummary string                   `json:"failure_summary"`
	Evidence       *domain.EvidenceManifest `json:"evidence"`
	RunnerID       domain.ID                `json:"runner_id"`
}

func (s *Server) finishWork(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body finishBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := fenceFrom(r, body)
	attempt, err := s.store.GetAttempt(r.Context(), fence.AttemptID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	fence.TaskID = attempt.TaskID
	caller, err := s.svc.Authorize(r.Context(), p, attempt.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.svc.FinishWork(r.Context(), caller, coord.FinishRequest{
		Fence: fence, ProjectID: attempt.ProjectID, Outcome: body.Outcome,
		FailureClass: body.FailureClass, FailureSummary: body.FailureSummary,
		Evidence: body.Evidence, RunnerID: body.RunnerID,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Handoff
// ---------------------------------------------------------------------------

type handoffBody struct {
	fenceBody
	ToHarness string               `json:"to_harness"`
	ToRole    string               `json:"to_role"`
	Bundle    domain.HandoffBundle `json:"bundle"`
}

func (s *Server) createHandoff(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body handoffBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence := body.fence()
	fence.TaskID = task.ID
	if fence.LeaseID == "" {
		lease, err := s.store.ActiveLeaseForTask(r.Context(), task.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		fence = domain.Fence{TaskID: task.ID, AttemptID: lease.AttemptID,
			LeaseID: lease.ID, FencingEpoch: lease.FencingEpoch}
	}
	handoff, err := s.svc.Handoff(r.Context(), caller, fence, body.ToHarness, body.ToRole, body.Bundle)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, handoff)
}

func (s *Server) getHandoff(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, _, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	handoff, err := s.store.LatestHandoff(r.Context(), task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, handoff)
}

// ---------------------------------------------------------------------------
// Presence, status, conflicts
// ---------------------------------------------------------------------------

func (s *Server) presence(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entries, err := s.svc.Presence(r.Context(), caller, project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"presence": entries})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	summary, err := s.svc.ProjectStatus(r.Context(), caller, project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, summary)
}

func (s *Server) listConflicts(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	openOnly := r.URL.Query().Get("all") != "true"
	views, err := s.svc.ConflictViews(r.Context(), caller, project.ID, openOnly)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"conflicts": views})
}

type resolveConflictBody struct {
	State string `json:"state"`
	Note  string `json:"note"`
}

func (s *Server) resolveConflict(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body resolveConflictBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	id := r.PathValue("conflict")
	var projectID domain.ID
	if err := s.store.Pool().QueryRow(r.Context(),
		`SELECT project_id::text FROM conflict_edges WHERE id = $1::uuid`, id).Scan(&projectID); err != nil {
		s.fail(w, r, domain.ErrNotFound)
		return
	}
	if _, err := s.svc.Authorize(r.Context(), p, projectID, domain.RoleContributor); err != nil {
		s.fail(w, r, err)
		return
	}
	edge, err := s.store.ResolveConflict(r.Context(), id, body.State, body.Note, p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, edge)
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	events, err := s.store.ListEvents(r.Context(), project.ID, intParam(r, "limit", 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"events": events})
}

// streamEvents is the SSE feed behind the dashboard (DESIGN.md §7.1: SSE first, because it
// passes through load balancers without special handling).
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, r, errors.New("streaming unsupported"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	cursor := time.Now().Add(-2 * time.Minute)
	var lastID domain.ID

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case <-keepalive.C:
			// Comment frames keep intermediaries from closing an idle connection.
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()

		case <-ticker.C:
			events, err := s.store.EventsSince(r.Context(), project.ID, cursor, lastID, 200)
			if err != nil {
				return
			}
			for _, e := range events {
				body, err := json.Marshal(e)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, body)
				cursor, lastID = e.OccurredAt, e.ID
			}
			if len(events) > 0 {
				flusher.Flush()
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Runners
// ---------------------------------------------------------------------------

type registerRunnerBody struct {
	Name string `json:"name"`
	// ProjectID accepts either a project id or an org-scoped slug, like every other
	// project reference in this API.
	ProjectID      string                    `json:"project_id"`
	Capabilities   domain.RunnerCapabilities `json:"capabilities"`
	MaxConcurrency int                       `json:"max_concurrency"`
}

func (s *Server) registerRunner(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body registerRunnerBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	var projectID domain.ID
	if body.ProjectID != "" {
		project, err := s.resolveProject(r.Context(), p, body.ProjectID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if _, err := s.svc.Authorize(r.Context(), p, project.ID, domain.RoleContributor); err != nil {
			s.fail(w, r, err)
			return
		}
		projectID = project.ID
	}
	runner, err := s.store.RegisterRunner(r.Context(), domain.Runner{
		OrganizationID: p.OrganizationID, ProjectID: projectID, PrincipalID: p.ID,
		Name: body.Name, Capabilities: body.Capabilities, MaxConcurrency: body.MaxConcurrency,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, runner)
}

type runnerHeartbeatBody struct {
	InFlight int `json:"in_flight"`
}

func (s *Server) heartbeatRunner(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body runnerHeartbeatBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.HeartbeatRunner(r.Context(), r.PathValue("runner"), body.InFlight); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

func (s *Server) serveDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.dashboard)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
