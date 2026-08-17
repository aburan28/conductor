package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
)

// The tool set is intentionally short (DESIGN.md §18). Nine tools, each mapping to one API
// call. `conductor_check_conflicts` is separated out from `coord_start_work` because it is
// the one an agent should reach for reflexively before editing — the cheapest possible call
// that prevents the most expensive possible mistake.
func toolDefinitions() []map[string]any {
	scopeSchema := map[string]any{
		"type":        "array",
		"description": "Resources you intend to touch, as `type:key` strings such as `path:internal/api/handlers.go`, `dir:internal/router`, or `migration:primary`.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource": map[string]any{"type": "string"},
				"mode": map[string]any{
					"type": "string",
					"enum": []string{"read_shared", "write_exclusive", "review_shared",
						"speculative_write", "protected_exclusive"},
					"description": "write_exclusive to modify, read_shared to only read.",
				},
			},
			"required": []string{"resource"},
		},
	}

	return []map[string]any{
		{
			"name": "conductor_check_conflicts",
			"description": "Check whether anyone else is already doing this work or holds the files you are about to edit. " +
				"Call this BEFORE editing anything. It changes nothing and returns an action: allow, allow_with_warning, " +
				"suggest_join, block_duplicate, or block_conflict.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary": map[string]any{"type": "string",
						"description": "One sentence describing the work. Used for duplicate detection; keep it factual and free of private context."},
					"scopes":       scopeSchema,
					"external_ref": map[string]any{"type": "string", "description": "Issue key, if any."},
				},
				"required": []string{"summary"},
			},
		},
		{
			"name": "coord_start_work",
			"description": "Declare intent, check for duplicates and conflicts, then create or attach to a task and acquire a lease. " +
				"Returns a fencing epoch that must accompany later mutations. If blocked, returns who holds what and what to do about it.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"summary":      map[string]any{"type": "string"},
					"title":        map[string]any{"type": "string"},
					"scopes":       scopeSchema,
					"external_ref": map[string]any{"type": "string"},
					"visibility": map[string]any{
						"type":        "string",
						"enum":        []string{"private", "team_summary", "team_artifacts"},
						"description": "private shares only your reserved scopes with the team, never the title or objective.",
					},
					"attach_to": map[string]any{"type": "string", "description": "Existing task id or ref to join instead of creating new work."},
					"force":     map[string]any{"type": "boolean", "description": "Proceed despite a duplicate suggestion, as a deliberate alternative."},
				},
				"required": []string{"summary"},
			},
		},
		{
			"name":        "coord_get_work",
			"description": "Fetch the active task card, workflow rules, dependencies, blockers, and any handoff bundle.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{"type": "string", "description": "Task id or ref. Defaults to the current attempt's task."},
				},
			},
		},
		{
			"name": "coord_expand_scope",
			"description": "Request additional files or resources when the work turns out to be wider than declared. " +
				"Returns granted, warning, or blocked with the conflicting owner.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"scopes": scopeSchema},
				"required":   []string{"scopes"},
			},
		},
		{
			"name": "coord_report_progress",
			"description": "Publish structured progress. Keep the summary short and structural — it is visible to the team " +
				"according to task visibility, and must not contain secrets or conversation excerpts.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"phase":         map[string]any{"type": "string", "enum": []string{"planning", "implementing", "testing", "reviewing", "blocked"}},
					"summary":       map[string]any{"type": "string"},
					"percent_hint":  map[string]any{"type": "integer"},
					"blocker":       map[string]any{"type": "string"},
					"changed_paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"phase"},
			},
		},
		{
			"name":        "coord_publish_result",
			"description": "Publish artifact metadata and validation evidence for the current attempt.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"commit_sha":    map[string]any{"type": "string"},
					"changed_paths": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"commands": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"command":   map[string]any{"type": "string"},
								"exit_code": map[string]any{"type": "integer"},
							},
							"required": []string{"command", "exit_code"},
						},
					},
				},
			},
		},
		{
			"name": "coord_finish_work",
			"description": "Finish the current attempt. Completion requires a commit and passing required checks; " +
				"reporting success without evidence will be refused and the task handed back.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"outcome":         map[string]any{"type": "string", "enum": []string{"succeeded", "failed", "cancelled", "blocked"}},
					"failure_summary": map[string]any{"type": "string"},
					"commit_sha":      map[string]any{"type": "string"},
				},
				"required": []string{"outcome"},
			},
		},
		{
			"name": "coord_handoff",
			"description": "Hand the task to another harness or person with a structured continuation bundle. " +
				"The next session receives decisions, open questions, and validation state — never your conversation.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to_harness":     map[string]any{"type": "string"},
					"next_action":    map[string]any{"type": "string"},
					"completed":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"open_questions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"assumptions":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
		},
		{
			"name": "coord_capabilities",
			"description": "What the sessions live on this project can actually do right now: which models are " +
				"connected, the highest tier and reasoning effort available, and who is free. Pass a requirement " +
				"to ask whether anything here can serve it before you plan work that depends on the answer.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tier":   map[string]any{"type": "string", "enum": []string{"T0", "T1", "T2", "T3", "T4"}},
					"effort": map[string]any{"type": "string", "enum": []string{"none", "low", "medium", "high", "xhigh", "max"}},
					"capabilities": map[string]any{
						"type": "array", "items": map[string]any{"type": "string"},
						"description": "Capability tags the session must have, such as architecture or long_context.",
					},
					"harness": map[string]any{"type": "string"},
					"role":    map[string]any{"type": "string", "enum": []string{"planner", "implementer", "verifier", "reviewer", "researcher"}},
				},
			},
		},
		{
			"name": "coord_delegate",
			"description": "Hand the current task to a live session that meets a capability floor — use this when the " +
				"work needs a more capable model or more reasoning effort than you are running at. Packages the same " +
				"continuation bundle as coord_handoff, releases your lease, and offers the task to the cheapest " +
				"session that clears the floor. If nothing live qualifies the bundle is still written, and you are " +
				"told what the ceiling actually is.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tier": map[string]any{"type": "string", "enum": []string{"T0", "T1", "T2", "T3", "T4"},
						"description": "Minimum model tier the receiving session must be running."},
					"effort": map[string]any{"type": "string", "enum": []string{"none", "low", "medium", "high", "xhigh", "max"},
						"description": "Minimum reasoning effort the receiving session must be able to run at."},
					"capabilities": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"harness":      map[string]any{"type": "string"},
					"role": map[string]any{"type": "string",
						"enum": []string{"planner", "implementer", "verifier", "reviewer", "researcher"}},
					"next_action":    map[string]any{"type": "string", "description": "What the receiving session should do first."},
					"completed":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"open_questions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"assumptions":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
		},
		{
			"name": "coord_project_status",
			"description": "Who is working on what right now, plus open conflicts. Shows tasks, owners, scopes, and " +
				"branches. Never shows anyone's prompts or model output.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func (s *Server) callTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if s.project == "" {
		return nil, errors.New("no project configured: set CONDUCTOR_PROJECT or pass --project")
	}

	switch name {
	case "conductor_check_conflicts":
		return s.checkConflicts(ctx, raw)
	case "coord_start_work":
		return s.startWork(ctx, raw)
	case "coord_get_work":
		return s.getWork(ctx, raw)
	case "coord_expand_scope":
		return s.expandScope(ctx, raw)
	case "coord_report_progress":
		return s.reportProgress(ctx, raw)
	case "coord_publish_result":
		return s.publishResult(ctx, raw)
	case "coord_finish_work":
		return s.finishWork(ctx, raw)
	case "coord_handoff":
		return s.handoff(ctx, raw)
	case "coord_capabilities":
		return s.capabilities(ctx, raw)
	case "coord_delegate":
		return s.delegate(ctx, raw)
	case "coord_project_status":
		return s.projectStatus(ctx)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

type scopeArg struct {
	Resource string                 `json:"resource"`
	Mode     domain.ReservationMode `json:"mode"`
}

func toScopes(in []scopeArg) []domain.ScopeRequest {
	out := make([]domain.ScopeRequest, 0, len(in))
	for _, s := range in {
		mode := s.Mode
		if mode == "" {
			mode = domain.ModeWriteExclusive
		}
		out = append(out, domain.ScopeRequest{Resource: s.Resource, Mode: mode})
	}
	return out
}

func (s *Server) checkConflicts(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Summary     string     `json:"summary"`
		Scopes      []scopeArg `json:"scopes"`
		ExternalRef string     `json:"external_ref"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}

	var decision coord.IntentDecision
	err := s.api.Post(ctx, "/v1/projects/"+s.project+"/intents/check", map[string]any{
		"summary":      args.Summary,
		"scopes":       toScopes(args.Scopes),
		"external_ref": args.ExternalRef,
		"exclude_task": s.fence.TaskID,
	}, &decision)
	if err != nil {
		return nil, err
	}
	return jsonResult(decision), nil
}

func (s *Server) startWork(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Summary     string            `json:"summary"`
		Title       string            `json:"title"`
		Scopes      []scopeArg        `json:"scopes"`
		ExternalRef string            `json:"external_ref"`
		Visibility  domain.Visibility `json:"visibility"`
		AttachTo    string            `json:"attach_to"`
		Force       bool              `json:"force"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}

	var result coord.StartWorkResult
	err := s.api.Post(ctx, "/v1/projects/"+s.project+"/work/start", map[string]any{
		"summary":      args.Summary,
		"title":        args.Title,
		"scopes":       toScopes(args.Scopes),
		"external_ref": args.ExternalRef,
		"visibility":   args.Visibility,
		"attach_to":    args.AttachTo,
		"force":        args.Force,
		"harness":      "mcp",
	}, &result)

	// A 409 here is a coordination answer, not a failure: the body explains who holds the
	// work and what to do. Returning it as a normal result lets the agent act on it.
	var apiErr *client.APIError
	if err != nil && errors.As(err, &apiErr) && apiErr.Blocked() {
		return jsonResult(result), nil
	}
	if err != nil {
		return nil, err
	}
	if result.Outcome != domain.OutcomeAllow {
		return jsonResult(result), nil
	}

	// Adopt the fence so subsequent tool calls in this session are automatically fenced.
	s.fence = domain.Fence{
		TaskID: result.TaskID, AttemptID: result.AttemptID,
		LeaseID: result.LeaseID, FencingEpoch: result.FencingEpoch,
	}
	return jsonResult(result), nil
}

func (s *Server) getWork(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Task string `json:"task"`
	}
	_ = json.Unmarshal(raw, &args)

	task := firstNonEmpty(args.Task, s.fence.TaskID)
	if task == "" {
		// No active task is not necessarily an error: work may have been offered to this
		// session by a coordinator that decided it needed this session's model.
		if offers := s.offers(ctx); offers != "" {
			return toolResult(offers, false), nil
		}
		return nil, errors.New("no active task; call coord_start_work first or pass a task id")
	}

	card, err := s.api.Raw(ctx, "/v1/tasks/"+task+"/card"+client.Query("project", s.project))
	if err != nil {
		return nil, err
	}

	out := string(card)
	var handoff map[string]any
	if err := s.api.Get(ctx, "/v1/tasks/"+task+"/handoff", &handoff); err == nil && handoff != nil {
		if body, err := json.MarshalIndent(handoff["bundle"], "", "  "); err == nil {
			out += "\n\n## Handoff bundle\n\n```json\n" + string(body) + "\n```\n"
		}
	}
	if offers := s.offers(ctx); offers != "" {
		out += "\n\n" + offers
	}
	return toolResult(out, false), nil
}

// offers renders the work waiting for this session, if any.
//
// It is folded into coord_get_work rather than given a tool of its own: an agent already
// calls get_work to find out what it should be doing, and a tenth tool that answers a
// question the ninth already implies costs context in every window for nothing (§4.9).
func (s *Server) offers(ctx context.Context) string {
	if s.session == "" {
		return ""
	}
	var body struct {
		Assignments []domain.Assignment `json:"assignments"`
	}
	if err := s.api.Get(ctx, "/v1/sessions/"+string(s.session)+"/assignments", &body); err != nil {
		return ""
	}
	if len(body.Assignments) == 0 {
		return ""
	}
	out := "## Work offered to this session\n\n" +
		"You were chosen for this because your model and reasoning effort meet a floor the " +
		"work requires. Call coord_start_work with attach_to set to the task to take it.\n\n"
	for _, a := range body.Assignments {
		out += fmt.Sprintf("- %s (%s) — requires %s\n", a.TaskRef, a.State, a.Requirement.Describe())
	}
	return out
}

func (s *Server) expandScope(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Scopes []scopeArg `json:"scopes"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if s.fence.TaskID == "" {
		return nil, errors.New("no active task; call coord_start_work first")
	}

	var result coord.ExpandScopeResult
	err := s.api.Post(ctx, "/v1/tasks/"+s.fence.TaskID+"/scopes", map[string]any{
		"attempt_id": s.fence.AttemptID, "lease_id": s.fence.LeaseID,
		"fencing_epoch": s.fence.FencingEpoch,
		"scopes":        toScopes(args.Scopes), "source": "declared",
	}, &result)

	var apiErr *client.APIError
	if err != nil && errors.As(err, &apiErr) && apiErr.Blocked() {
		return jsonResult(result), nil
	}
	if err != nil {
		return nil, err
	}
	return jsonResult(result), nil
}

func (s *Server) reportProgress(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Phase        string   `json:"phase"`
		Summary      string   `json:"summary"`
		PercentHint  int      `json:"percent_hint"`
		Blocker      string   `json:"blocker"`
		ChangedPaths []string `json:"changed_paths"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if err := s.requireFence(); err != nil {
		return nil, err
	}

	err := s.api.Post(ctx, "/v1/attempts/"+s.fence.AttemptID+"/progress", map[string]any{
		"task_id": s.fence.TaskID, "lease_id": s.fence.LeaseID,
		"fencing_epoch": s.fence.FencingEpoch,
		"phase":         args.Phase, "summary": args.Summary,
		"percent_hint": args.PercentHint, "blocker": args.Blocker,
		"changed_paths": args.ChangedPaths,
	}, nil)
	if err != nil {
		return nil, err
	}
	return toolResult("progress recorded", false), nil
}

func (s *Server) publishResult(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		CommitSHA    string   `json:"commit_sha"`
		ChangedPaths []string `json:"changed_paths"`
		Commands     []struct {
			Command  string `json:"command"`
			ExitCode int    `json:"exit_code"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if err := s.requireFence(); err != nil {
		return nil, err
	}

	commands := make([]domain.ValidationResult, 0, len(args.Commands))
	for i, c := range args.Commands {
		commands = append(commands, domain.ValidationResult{
			CommandID: fmt.Sprintf("agent-%d", i), Command: c.Command, ExitCode: c.ExitCode,
		})
	}

	// Reported as evidence with outcome "blocked", which records the artifact metadata
	// without transitioning the task. Completion still goes through coord_finish_work, and
	// the runner's own observations are what the verification pipeline trusts.
	err := s.api.Post(ctx, "/v1/attempts/"+s.fence.AttemptID+"/progress", map[string]any{
		"task_id": s.fence.TaskID, "lease_id": s.fence.LeaseID,
		"fencing_epoch": s.fence.FencingEpoch,
		"phase":         "testing", "changed_paths": args.ChangedPaths,
		"summary": fmt.Sprintf("published %d validation result(s)", len(commands)),
	}, nil)
	if err != nil {
		return nil, err
	}
	return jsonResult(map[string]any{
		"status": "recorded", "commands": len(commands), "commit_sha": args.CommitSHA,
		"note": "Validation is attested by the runner. Agent-reported exit codes are advisory.",
	}), nil
}

func (s *Server) finishWork(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Outcome        string `json:"outcome"`
		FailureSummary string `json:"failure_summary"`
		CommitSHA      string `json:"commit_sha"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if err := s.requireFence(); err != nil {
		return nil, err
	}

	body := map[string]any{
		"task_id": s.fence.TaskID, "lease_id": s.fence.LeaseID,
		"fencing_epoch": s.fence.FencingEpoch,
		"outcome":       args.Outcome, "failure_summary": args.FailureSummary,
	}
	if args.CommitSHA != "" {
		body["evidence"] = map[string]any{"commit_sha": args.CommitSHA}
	}

	var result coord.FinishResult
	if err := s.api.Post(ctx, "/v1/attempts/"+s.fence.AttemptID+"/result", body, &result); err != nil {
		return nil, err
	}
	if len(result.MissingChecks) > 0 {
		return jsonResult(result), nil
	}
	s.fence = domain.Fence{}
	return jsonResult(result), nil
}

func (s *Server) handoff(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ToHarness     string   `json:"to_harness"`
		NextAction    string   `json:"next_action"`
		Completed     []string `json:"completed"`
		OpenQuestions []string `json:"open_questions"`
		Assumptions   []string `json:"assumptions"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if err := s.requireFence(); err != nil {
		return nil, err
	}

	var out map[string]any
	err := s.api.Post(ctx, "/v1/tasks/"+s.fence.TaskID+"/handoff", map[string]any{
		"attempt_id": s.fence.AttemptID, "lease_id": s.fence.LeaseID,
		"fencing_epoch": s.fence.FencingEpoch,
		"to_harness":    args.ToHarness,
		"bundle": map[string]any{
			"recommended_next_action": args.NextAction,
			"completed_work":          args.Completed,
			"open_questions":          args.OpenQuestions,
			"assumptions":             args.Assumptions,
		},
	}, &out)
	if err != nil {
		return nil, err
	}
	s.fence = domain.Fence{}
	return jsonResult(out), nil
}

// requirementArgs is the capability floor an agent can name on the two capability tools.
type requirementArgs struct {
	Tier         string   `json:"tier"`
	Effort       string   `json:"effort"`
	Capabilities []string `json:"capabilities"`
	Harness      string   `json:"harness"`
	Role         string   `json:"role"`
}

func (a requirementArgs) query() string {
	values := url.Values{}
	for key, value := range map[string]string{
		"tier": a.Tier, "effort": a.Effort, "harness": a.Harness, "role": a.Role,
	} {
		if value != "" {
			values.Set(key, value)
		}
	}
	for _, c := range a.Capabilities {
		if c != "" {
			values.Add("capability", c)
		}
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}

func (a requirementArgs) requirement() map[string]any {
	return map[string]any{
		"tier": a.Tier, "reasoning_effort": a.Effort, "capabilities": a.Capabilities,
		"harness": a.Harness, "role": a.Role,
	}
}

func (s *Server) capabilities(ctx context.Context, raw json.RawMessage) (any, error) {
	var args requirementArgs
	_ = json.Unmarshal(raw, &args)

	var inv coord.CapabilityInventory
	path := "/v1/projects/" + s.project + "/capabilities" + args.query()
	if err := s.api.Get(ctx, path, &inv); err != nil {
		return nil, err
	}
	return jsonResult(inv), nil
}

// delegate hands the current task to a session that clears a capability floor.
//
// It is deliberately separate from coord_handoff. A handoff says "someone else should
// continue this"; a delegation says "this needs more than I have", and the second is the one
// a coordinator running at medium effort needs when it hits work that requires xhigh.
func (s *Server) delegate(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		requirementArgs
		NextAction    string   `json:"next_action"`
		Completed     []string `json:"completed"`
		OpenQuestions []string `json:"open_questions"`
		Assumptions   []string `json:"assumptions"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if err := s.requireFence(); err != nil {
		return nil, err
	}

	var out coord.DelegateResult
	err := s.api.Post(ctx, "/v1/tasks/"+s.fence.TaskID+"/handoff", map[string]any{
		"attempt_id": s.fence.AttemptID, "lease_id": s.fence.LeaseID,
		"fencing_epoch": s.fence.FencingEpoch,
		"to_harness":    args.Harness,
		"to_role":       args.Role,
		"session_id":    s.session,
		"require":       args.requirement(),
		"bundle": map[string]any{
			"recommended_next_action": args.NextAction,
			"completed_work":          args.Completed,
			"open_questions":          args.OpenQuestions,
			"assumptions":             args.Assumptions,
		},
	}, &out)
	if err != nil {
		return nil, err
	}
	// The lease is gone either way: the handoff released it. Clearing the fence keeps the
	// agent from making further fenced calls that would now fail confusingly.
	s.fence = domain.Fence{}
	return jsonResult(out), nil
}

func (s *Server) projectStatus(ctx context.Context) (any, error) {
	var summary coord.StatusSummary
	if err := s.api.Get(ctx, "/v1/projects/"+s.project+"/status", &summary); err != nil {
		return nil, err
	}
	return jsonResult(summary), nil
}

func (s *Server) requireFence() error {
	if s.fence.AttemptID == "" || s.fence.LeaseID == "" {
		return errors.New("no active claim: call coord_start_work first, or run inside a Conductor attempt")
	}
	return nil
}
