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
	m.HandleFunc("PATCH /v1/projects/{project}/members/{handle}", auth(s.setMemberRole))
	m.HandleFunc("DELETE /v1/projects/{project}/members/{handle}", auth(s.removeMember))

	m.HandleFunc("GET /v1/tokens", auth(s.listTokens))
	m.HandleFunc("POST /v1/tokens", auth(s.createToken))
	m.HandleFunc("POST /v1/tokens/reset", auth(s.resetToken))
	m.HandleFunc("DELETE /v1/tokens/{name}", auth(s.revokeToken))
	m.HandleFunc("POST /v1/tokens/revoke-all", auth(s.revokeAllTokens))

	m.HandleFunc("POST /v1/projects/{project}/sessions", auth(s.registerSession))
	m.HandleFunc("GET /v1/projects/{project}/sessions", auth(s.listSessions))
	m.HandleFunc("POST /v1/sessions/{session}/heartbeat", auth(s.heartbeatSession))
	m.HandleFunc("POST /v1/sessions/{session}/close", auth(s.closeSession))
	m.HandleFunc("POST /v1/sessions/{session}/capabilities", auth(s.setSessionCapabilities))
	m.HandleFunc("GET /v1/sessions/{session}/assignments", auth(s.sessionAssignments))
	m.HandleFunc("POST /v1/assignments/{assignment}/respond", auth(s.respondToAssignment))
	m.HandleFunc("GET /v1/projects/{project}/capabilities", auth(s.projectCapabilities))
	m.HandleFunc("POST /v1/tasks/{task}/assign", auth(s.assignTask))

	m.HandleFunc("POST /v1/projects/{project}/intents/check", auth(s.checkIntent))
	m.HandleFunc("POST /v1/projects/{project}/work/start", auth(s.startWork))

	m.HandleFunc("GET /v1/projects/{project}/tasks", auth(s.listTasks))
	m.HandleFunc("POST /v1/projects/{project}/tasks", auth(s.createTask))
	m.HandleFunc("GET /v1/tasks/{task}", auth(s.getTask))
	m.HandleFunc("PATCH /v1/tasks/{task}", auth(s.patchTask))
	m.HandleFunc("GET /v1/tasks/{task}/card", auth(s.getTaskCard))
	m.HandleFunc("GET /v1/tasks/{task}/attempts", auth(s.listAttempts))
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

	m.HandleFunc("POST /v1/sessions/{session}/usage", auth(s.recordSessionUsage))
	m.HandleFunc("POST /v1/projects/{project}/usage", auth(s.recordSyncedUsage))
	m.HandleFunc("GET /v1/projects/{project}/usage", auth(s.getUsage))

	m.HandleFunc("GET /v1/projects/{project}/budget", auth(s.getBudget))
	m.HandleFunc("POST /v1/projects/{project}/budget/share", auth(s.shareBudget))
	m.HandleFunc("GET /v1/projects/{project}/budget/grants", auth(s.listBudgetGrants))

	m.HandleFunc("GET /v1/projects/{project}/events", auth(s.listEvents))
	m.HandleFunc("GET /v1/projects/{project}/events/stream", s.authenticateStream(s.streamEvents))

	m.HandleFunc("POST /v1/runners/register", auth(s.registerRunner))
	m.HandleFunc("POST /v1/runners/{runner}/heartbeat", auth(s.heartbeatRunner))

	m.HandleFunc("GET /v1/projects/{project}/runner/snapshot", auth(s.runnerSnapshot))
	m.HandleFunc("GET /v1/attempts/{attempt}/brief", auth(s.attemptBrief))
	m.HandleFunc("POST /v1/attempts/{attempt}/route", auth(s.setAttemptRoute))
	m.HandleFunc("POST /v1/attempts/{attempt}/state", auth(s.setAttemptState))

	s.inspectRoutes(m)
	s.mcpRoutes(m)
	s.queueRoutes(m)
	s.localRoutes(m)
	s.githubRoutes(m)
	s.lifecycleRoutes(m)

	// The mesh surface. /v1/peer/* is authenticated by the peer's mesh certificate (not a
	// bearer token); /v1/peers is the same link table shown to project members.
	if s.peerName != "" {
		m.HandleFunc("GET /v1/peer/info", s.peerAuth(s.peerInfo))
	}
	m.HandleFunc("GET /v1/peers", auth(s.listPeers))

	// The SPA owns every path the API does not. Go's mux prefers the more specific pattern,
	// so /v1/... always wins and everything else — "/", "/tasks/T-42", "/static/app.js" —
	// is the dashboard's to route client-side.
	if s.web != nil {
		m.Handle("GET /", s.web)
	}
}

// ---------------------------------------------------------------------------
// Health and identity
// ---------------------------------------------------------------------------

// health is the unauthenticated liveness probe. It names a failure without quoting it: the
// driver's error text can carry the database host, user and more. /v1/ready has the detail.
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Pool().Ping(r.Context()); err != nil {
		s.logger.Error("health: database unreachable", "request_id", requestID(r), "error", err)
		s.ok(w, r, http.StatusServiceUnavailable,
			map[string]any{"status": "degraded", "database": "unreachable", "request_id": requestID(r)})
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
	projects = inTokenScope(r, projects)
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
	projects = inTokenScope(r, projects)
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
	Harness        string                     `json:"harness"`
	HarnessVersion string                     `json:"harness_version"`
	MachineID      string                     `json:"machine_id"`
	BaseSHA        string                     `json:"base_sha"`
	Branch         string                     `json:"branch"`
	WorktreePath   string                     `json:"worktree_path"`
	Visibility     domain.Visibility          `json:"visibility"`
	RunnerID       domain.ID                  `json:"runner_id"`
	Capabilities   domain.SessionCapabilities `json:"capabilities"`
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
	// What the session declared is resolved against the org catalog before it is stored, so
	// the tier it will be selected on is the catalog's opinion of that model, not its own.
	caps, err := s.svc.ResolveCapabilities(r.Context(), project.OrganizationID, body.Harness, body.Capabilities)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	session, err := s.store.RegisterSession(r.Context(), db.RegisterSessionParams{
		ProjectID: project.ID, PrincipalID: p.ID, RunnerID: body.RunnerID,
		Harness: body.Harness, HarnessVersion: body.HarnessVersion,
		MachineID: body.MachineID, BaseSHA: body.BaseSHA, Branch: body.Branch,
		WorktreePath: body.WorktreePath, Visibility: body.Visibility,
		Capabilities: caps,
		TTL:          project.Config.LeaseTTL.OrDefault(90 * time.Second),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, session)
}

// setSessionCapabilities updates what a live session advertises, for when someone switches
// model or raises effort without restarting the session.
func (s *Server) setSessionCapabilities(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.ownSession(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body domain.SessionCapabilities
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	project, err := s.store.GetProject(r.Context(), session.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caps, err := s.svc.ResolveCapabilities(r.Context(), project.OrganizationID, session.Harness, body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.store.SetSessionCapabilities(r.Context(), session.ID, caps)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, updated)
}

func (s *Server) sessionAssignments(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.store.GetSession(r.Context(), r.PathValue("session"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, session.ProjectID, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	assignments, err := s.svc.Inbox(r.Context(), caller, session.ID, r.URL.Query().Get("all") == "true")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"assignments": assignments})
}

func (s *Server) respondToAssignment(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	assignment, err := s.store.GetAssignment(r.Context(), r.PathValue("assignment"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	caller, err := s.svc.Authorize(r.Context(), p, assignment.ProjectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body struct {
		Accept bool   `json:"accept"`
		Note   string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	updated, err := s.svc.RespondToOffer(r.Context(), caller, assignment.ID, body.Accept, body.Note)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, updated)
}

// ---------------------------------------------------------------------------
// Capabilities and assignment
// ---------------------------------------------------------------------------

// requirementFromQuery reads a capability floor off the query string, so the read path stays
// a GET: `?tier=T4&effort=xhigh&capability=architecture&capability=long_context`.
func requirementFromQuery(r *http.Request) domain.CapabilityRequirement {
	q := r.URL.Query()
	return domain.CapabilityRequirement{
		Tier:         domain.Tier(q.Get("tier")),
		Effort:       domain.Effort(q.Get("effort")),
		Capabilities: q["capability"],
		Harness:      q.Get("harness"),
		Model:        q.Get("model"),
		Role:         domain.AgentRole(q.Get("role")),
	}
}

func (s *Server) projectCapabilities(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	req := requirementFromQuery(r)
	if err := validateRequirement(req); err != nil {
		s.fail(w, r, err)
		return
	}
	inv, err := s.svc.Capabilities(r.Context(), caller, project.ID, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, inv)
}

type assignTaskBody struct {
	SessionID   domain.ID                    `json:"session_id"`
	Requirement domain.CapabilityRequirement `json:"require"`
	TTLSeconds  int                          `json:"ttl_seconds"`
}

func (s *Server) assignTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body assignTaskBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validateRequirement(body.Requirement); err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.svc.Assign(r.Context(), caller, coord.AssignParams{
		TaskID:      task.ID,
		Requirement: body.Requirement,
		SessionID:   body.SessionID,
		TTL:         time.Duration(body.TTLSeconds) * time.Second,
	})
	if err != nil {
		// A capacity failure carries the rejection reasons, which are the actionable part:
		// returning a bare 503 would hide "three sessions are up, none can do xhigh".
		if errors.Is(err, domain.ErrCapacity) {
			s.ok(w, r, http.StatusConflict, map[string]any{
				"error": err.Error(), "code": "no_capable_session",
				"rejected": result.Choice.Rejected, "inventory": result.Inventory,
			})
			return
		}
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, result)
}

// validateRequirement rejects unknown tiers and efforts up front. A typo like `effort=xhig`
// would otherwise silently match nothing and read as "nobody can do this".
func validateRequirement(req domain.CapabilityRequirement) error {
	if req.Tier != "" && !validTier(req.Tier) {
		return fmt.Errorf("%w: unknown tier %q", domain.ErrInvalidEnum, req.Tier)
	}
	if req.Effort != "" && !validEffort(req.Effort) {
		return fmt.Errorf("%w: unknown reasoning effort %q", domain.ErrInvalidEnum, req.Effort)
	}
	return nil
}

func validTier(t domain.Tier) bool {
	for _, known := range []domain.Tier{domain.TierT0, domain.TierT1, domain.TierT2, domain.TierT3, domain.TierT4} {
		if t == known {
			return true
		}
	}
	return false
}

func validEffort(e domain.Effort) bool {
	for _, known := range domain.AllEfforts {
		if e == known {
			return true
		}
	}
	return false
}

type heartbeatSessionBody struct {
	State   domain.SessionState `json:"state"`
	Branch  string              `json:"branch"`
	BaseSHA string              `json:"base_sha"`
	// ChangedPaths is what the session's working tree differs in, paths only. It feeds the
	// merge-risk graph for interactive sessions the way a runner's harvested diff does.
	ChangedPaths []string `json:"changed_paths"`
}

func (s *Server) heartbeatSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.ownSession(r, p)
	if err != nil {
		s.fail(w, r, err)
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
		TTL:          project.Config.LeaseTTL.OrDefault(90 * time.Second),
		ChangedPaths: body.ChangedPaths,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, updated)
}

// listSessions is the read behind `conductor sessions save all`: every session the project
// has had, including closed and stale ones, projected for the caller.
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sessions, err := s.svc.Sessions(r.Context(), caller, project.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	session, err := s.ownSession(r, p)
	if err != nil {
		s.fail(w, r, err)
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
		Labels:   r.URL.Query()["label"],
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
	Labels             []string                     `json:"labels"`
	DependsOn          []string                     `json:"depends_on"`
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

	// Dependencies may be given as task refs (T-12) or ids; resolve refs to ids so the UI
	// can link by the ref a person actually types.
	dependsOn := make([]domain.ID, 0, len(body.DependsOn))
	for _, dep := range body.DependsOn {
		if dep == "" {
			continue
		}
		if dt, err := s.store.GetTask(r.Context(), dep); err == nil {
			dependsOn = append(dependsOn, dt.ID)
		} else if dt, err := s.store.GetTaskByRef(r.Context(), project.ID, dep); err == nil {
			dependsOn = append(dependsOn, dt.ID)
		} else {
			s.fail(w, r, fmt.Errorf("%w: unknown dependency %q", domain.ErrInvalidArgument, dep))
			return
		}
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
		Labels:             body.Labels,
		MaxAttempts:        project.Config.MaxAttempts,
		DependsOn:          dependsOn,
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
	redacted := task.Visibility == domain.VisibilityPrivate && owner.ID != caller.Principal.ID
	if redacted {
		task.Title, task.Objective, task.ExternalRef = "(private)", "", ""
		task.AcceptanceCriteria = nil
		task.PullRequestURL = ""
	}

	reservations, err := s.store.ReservationsForTask(r.Context(), task.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var attempt *domain.Attempt
	if a, err := s.store.ActiveAttempt(r.Context(), task.ID); err == nil {
		attempt = &a
	} else if a, err := s.store.LatestAttempt(r.Context(), task.ID); err == nil {
		// A finished task has no active attempt, and its branch and commit are the point of
		// looking at it: without this the card said "not yet published" after a publish.
		attempt = &a
	}
	var lease *domain.Lease
	if l, err := s.store.ActiveLeaseForTask(r.Context(), task.ID); err == nil {
		lease = &l
	}

	card := taskcard.FromTask(task, project.Slug, owner.Handle, attempt, lease,
		reservations, task.DependsOn, project.Config.RequiredChecks)
	if redacted && (attempt == nil || attempt.SponsorPrincipal != caller.Principal.ID) {
		// Where the work runs is task detail, as in privacy.ProjectAttempt.
		card.Worktree = ""
	}
	if lease != nil && card.Lease != nil {
		// Everyone may see that the task is held, by whom, and until when — that is what
		// stops a collision. Only the holder sees the lease id and fencing epoch, which are
		// the identifiers that act on the lease.
		if holder, err := s.store.GetPrincipal(r.Context(), lease.HolderPrincipal); err == nil {
			card.Lease.Holder = holder.Handle
		}
		if !caller.HoldsLease(*lease) {
			card.Lease.ID, card.Lease.FencingEpoch = "", 0
		}
	}
	rendered, err := card.Render()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(rendered))
}

type patchTaskBody struct {
	Title              *string                       `json:"title"`
	Objective          *string                       `json:"objective"`
	AcceptanceCriteria *[]domain.AcceptanceCriterion `json:"acceptance_criteria"`
	Priority           *int                          `json:"priority"`
	RiskLevel          *domain.RiskLevel             `json:"risk_level"`
	Visibility         *domain.Visibility            `json:"visibility"`
	ModelAlias         *string                       `json:"model_alias"`
	Labels             *[]string                     `json:"labels"`
}

// patchTask edits a task. Who may (coord.AuthorizeTaskEdit): the creator, the holder of its
// live lease, or a maintainer; changing visibility needs the creator or a maintainer. Before,
// any contributor could rewrite a teammate's title and objective, or publish their private
// task.
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
	others, err := s.svc.AuthorizeTaskEdit(r.Context(), caller, task, body.Visibility != nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.store.PatchTask(r.Context(), task.ID, db.TaskPatch{
		Title: body.Title, Objective: body.Objective, AcceptanceCriteria: body.AcceptanceCriteria,
		Priority: body.Priority, RiskLevel: body.RiskLevel, Visibility: body.Visibility,
		ModelAlias: body.ModelAlias, Labels: body.Labels,
	}); err != nil {
		s.fail(w, r, err)
		return
	}
	if others {
		detail := map[string]any{}
		if body.Visibility != nil {
			detail["from"], detail["to"] = string(task.Visibility), string(*body.Visibility)
		}
		s.auditOthersWork(r, caller, task, "task.edited_by_other", detail)
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
		LeaseTTL: coord.ClaimLeaseTTL(project.Config, body.SessionID, body.RunnerID),
		Scopes:   body.Scopes, ScopePolicy: config.ScopePolicyFrom(project.Config),
		AllowWarnings:     body.AllowWarnings,
		MemberTokenBudget: project.Config.Budget.MemberTokens,
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
	project, caller, err := s.execProject(r, p)
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
			LeaseTTL:    coord.ClaimLeaseTTL(project.Config, body.SessionID, body.RunnerID),
			ScopePolicy: config.ScopePolicyFrom(project.Config), AllowWarnings: true,
			MemberTokenBudget: project.Config.Budget.MemberTokens,
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
	task, caller, err := s.execTaskFor(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body releaseBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence, override, err := s.svc.ResolveFence(r.Context(), caller, task.ID, body.fence())
	if err != nil {
		s.fail(w, r, err)
		return
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
	if override {
		s.auditOthersWork(r, caller, task, "lease.released_by_other", map[string]any{
			"lease_id": fence.LeaseID, "status": string(updated.Status)})
	}
	s.ok(w, r, http.StatusOK, updated)
}

type transitionBody struct {
	Status domain.TaskStatus `json:"status"`
	// To is accepted as a synonym for Status; it is what the dashboard sends.
	To domain.TaskStatus `json:"to"`
}

func (s *Server) transitionTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	// Reviewer is the floor here, below contributor: a reviewer may make review decisions
	// and nothing else (authorizeTransition).
	task, caller, err := s.taskFor(r, p, domain.RoleReviewer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body transitionBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	if body.Status == "" {
		body.Status = body.To
	}
	if body.Status == "" {
		s.fail(w, r, fmt.Errorf("%w: status is required", domain.ErrInvalidArgument))
		return
	}
	others, err := s.svc.AuthorizeTransition(r.Context(), caller, task, body.Status)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if _, err := s.store.UpdateTaskStatus(r.Context(), task.ID, body.Status); err != nil {
		s.fail(w, r, err)
		return
	}
	if others {
		s.auditOthersWork(r, caller, task, "task.transitioned_by_other", map[string]any{
			"from": string(task.Status), "to": string(body.Status)})
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
	task, caller, err := s.execTaskFor(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var body expandScopeBody
	if err := decode(r, &body); err != nil {
		s.fail(w, r, err)
		return
	}
	fence, override, err := s.svc.ResolveFence(r.Context(), caller, task.ID, body.fence())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result, err := s.svc.ExpandScope(r.Context(), caller, fence, task.ProjectID,
		body.Scopes, body.Source)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if override {
		s.auditOthersWork(r, caller, task, "lease.scope_expanded_by_other", map[string]any{
			"lease_id": fence.LeaseID, "outcome": string(result.Outcome)})
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
	caller, err := s.svc.AuthorizeExecution(r.Context(), p, lease.ProjectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Renewing someone else's lease would keep their abandoned work from being reclaimed.
	if !caller.HoldsLease(lease) && !caller.Role.Can(domain.RoleMaintainer) {
		s.fail(w, r, fmt.Errorf("%w: this lease is held by another principal", domain.ErrNotPermitted))
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
	caller, err := s.svc.AuthorizeExecution(r.Context(), p, attempt.ProjectID)
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
	caller, err := s.svc.AuthorizeExecution(r.Context(), p, attempt.ProjectID)
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
	ToHarness   string                       `json:"to_harness"`
	ToRole      string                       `json:"to_role"`
	Bundle      domain.HandoffBundle         `json:"bundle"`
	Requirement domain.CapabilityRequirement `json:"require"`
	SessionID   domain.ID                    `json:"session_id"`
	TTLSeconds  int                          `json:"ttl_seconds"`
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
	fence, override, err := s.svc.ResolveFence(r.Context(), caller, task.ID, body.fence())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if override {
		s.auditOthersWork(r, caller, task, "lease.handed_off_by_other", map[string]any{
			"lease_id": fence.LeaseID})
	}
	// A handoff that names a capability floor is a delegation: package the work *and* offer
	// it to a session that can meet the floor, in one call.
	if !body.Requirement.Empty() {
		if err := validateRequirement(body.Requirement); err != nil {
			s.fail(w, r, err)
			return
		}
		caller.SessionID = firstNonEmpty(body.SessionID, caller.SessionID)
		result, err := s.svc.Delegate(r.Context(), caller, coord.DelegateParams{
			Fence: fence, ToHarness: body.ToHarness, ToRole: body.ToRole,
			Bundle: body.Bundle, Requirement: body.Requirement,
			TTL: time.Duration(body.TTLSeconds) * time.Second,
		})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.ok(w, r, http.StatusCreated, result)
		return
	}

	handoff, err := s.svc.Handoff(r.Context(), caller, fence, body.ToHarness, body.ToRole, body.Bundle)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusCreated, handoff)
}

func (s *Server) getHandoff(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	task, caller, err := s.taskFor(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	handoff, err := s.svc.HandoffView(r.Context(), caller, task)
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
	caller, err := s.svc.Authorize(r.Context(), p, projectID, domain.RoleContributor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	edge, err := s.store.ResolveConflict(r.Context(), id, body.State, body.Note, p.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Ignoring a conflict silences a warning for both parties, so it is on the record.
	if edge.State != "open" {
		s.store.Audit(r.Context(), caller.Principal.OrganizationID, projectID, p.ID,
			"conflict."+edge.State, "conflict", edge.ID, map[string]any{"state": edge.State})
	}
	s.ok(w, r, http.StatusOK, edge)
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	events, err := s.store.ListEvents(r.Context(), project.ID, intParam(r, "limit", 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Every event leaves through the caller's visibility (coord.ProjectEvents), exactly like
	// the task it describes.
	events, err = s.svc.ProjectEvents(r.Context(), caller, project.ID, events)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"events": events})
}

// streamEvents is the SSE feed behind the dashboard (DESIGN.md §7.1: SSE first, because it
// passes through load balancers without special handling). Live events come from the shared
// per-project feed (hub.go) rather than a poll per connection.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, caller, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, r, errors.New("streaming unsupported"))
		return
	}
	// Subscribe before reading the backlog, so nothing committed in between is missed.
	sub, err := s.ops.hub.subscribe(project.ID, p.ID)
	if err != nil {
		w.Header().Set("Retry-After", "30")
		s.ok(w, r, http.StatusServiceUnavailable, ErrorBody{Code: "no_capacity",
			Error: "too many open event streams; close a dashboard tab or retry later"})
		return
	}
	defer sub.close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	cursor := time.Now().Add(-streamBacklog)
	var lastID domain.ID
	// send writes events the viewer has not seen yet, each projected for this viewer first:
	// the shared feed carries every event in the project, and a private task's details are
	// not for everyone watching it. A projection failure ends the stream rather than risk
	// sending an unfiltered event.
	send := func(raw ...domain.Event) bool {
		fresh := raw[:0:0]
		for _, e := range raw {
			if after(e, cursor, lastID) {
				fresh = append(fresh, e)
				cursor, lastID = e.OccurredAt, e.ID
			}
		}
		if len(fresh) == 0 {
			return true
		}
		events, err := s.svc.ProjectEvents(r.Context(), caller, project.ID, fresh)
		if err != nil {
			return false
		}
		for _, e := range events {
			body, err := json.Marshal(e)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, body)
		}
		return true
	}

	backlog, err := s.store.EventsSince(r.Context(), project.ID, cursor, lastID, 200)
	if err != nil {
		return
	}
	if !send(backlog...) {
		return
	}
	flusher.Flush()

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

		case e, open := <-sub.events:
			if !open {
				return // too slow to keep up, or the server is shutting down
			}
			// Gather whatever else is ready, so a burst is projected and flushed as one write.
			burst := []domain.Event{e}
			closed := false
			for drained := false; !drained; {
				select {
				case e, open := <-sub.events:
					if !open {
						closed, drained = true, true
						break
					}
					burst = append(burst, e)
				default:
					drained = true
				}
			}
			if !send(burst...) {
				return
			}
			flusher.Flush()
			if closed {
				return
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
		if _, err := s.svc.AuthorizeExecution(r.Context(), p, project.ID); err != nil {
			s.fail(w, r, err)
			return
		}
		projectID = project.ID
	} else if tokenProjectScope(r) != "" {
		// An org-wide runner would be reachable from projects the token is not scoped to.
		s.fail(w, r, fmt.Errorf("%w: a project-scoped token registers only a runner for that project",
			domain.ErrNotPermitted))
		return
	}
	runner, err := s.store.RegisterRunner(r.Context(), domain.Runner{
		OrganizationID: p.OrganizationID, ProjectID: projectID, PrincipalID: p.ID,
		Name: body.Name, Capabilities: body.Capabilities, MaxConcurrency: body.MaxConcurrency,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.Audit(r.Context(), p.OrganizationID, projectID, p.ID,
		"runner.registered", "runner", runner.ID, map[string]any{"count": runner.MaxConcurrency})
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
	// Only the principal that registered a runner may report its liveness and load; the
	// store matches on both, so another principal — in this organization or any other —
	// gets the same 404 as for a runner that does not exist.
	if tokenProjectScope(r) != "" {
		if runner, err := s.store.GetRunner(r.Context(), r.PathValue("runner")); err != nil ||
			runner.ProjectID != tokenProjectScope(r) {
			s.fail(w, r, domain.ErrNotFound)
			return
		}
	}
	if err := s.store.HeartbeatRunner(r.Context(), r.PathValue("runner"), p.ID, body.InFlight); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
