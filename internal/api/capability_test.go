package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
)

// End-to-end capability routing over HTTP: a session advertises a model, the catalog decides
// what that model is worth, and work that names a floor reaches a session that clears it.

// seedProfiles puts two models in the org catalog: a cheap one and a frontier one.
func (h *harness) seedProfiles(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []domain.ModelProfile{
		{
			OrganizationID: h.org.ID, Alias: "worker.fast", Harness: "claude", Provider: "anthropic",
			Model: "test-haiku", Tier: domain.TierT1, ReasoningEffort: domain.EffortLow,
			Capabilities: []string{"code_edit"}, ContextWindow: 100000, Enabled: true, Billing: "tokens",
		},
		{
			OrganizationID: h.org.ID, Alias: "planner.frontier", Harness: "claude", Provider: "anthropic",
			Model: "test-opus", Tier: domain.TierT4, ReasoningEffort: domain.EffortHigh,
			Capabilities:  []string{"code_edit", "architecture", "long_context"},
			ContextWindow: 1000000, Enabled: true, Billing: "tokens",
		},
	} {
		if _, err := h.store.UpsertModelProfile(ctx, p); err != nil {
			t.Fatalf("seed profile %s: %v", p.Model, err)
		}
	}
}

func (h *harness) registerSession(t *testing.T, token string, caps map[string]any) domain.Session {
	t.Helper()
	code, body := h.do(token, http.MethodPost, h.projectPath("/sessions"), map[string]any{
		"harness": "claude", "capabilities": caps,
	})
	if code != http.StatusCreated {
		t.Fatalf("register session = %d\n%s", code, body)
	}
	var session domain.Session
	if err := json.Unmarshal(body, &session); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	return session
}

func (h *harness) createTask(t *testing.T, token, title string) domain.Task {
	t.Helper()
	code, body := h.do(token, http.MethodPost, h.projectPath("/tasks"), map[string]any{
		"title": title,
	})
	if code != http.StatusCreated {
		t.Fatalf("create task = %d\n%s", code, body)
	}
	var task domain.Task
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return task
}

// A session's declared tier is not its tier. The catalog's is.
func TestSessionCapabilitiesAreResolvedAgainstTheCatalog(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)

	session := h.registerSession(t, h.aliceTok, map[string]any{
		"model": "test-opus", "reasoning_effort": "xhigh", "tier": "T0",
	})
	if session.Capabilities.Tier != domain.TierT4 {
		t.Errorf("tier = %s, want the catalog's T4 rather than the declared T0", session.Capabilities.Tier)
	}
	if !session.Capabilities.Resolved {
		t.Error("a catalogued model should resolve")
	}
	if session.Capabilities.Effort != domain.EffortXHigh {
		t.Errorf("effort = %s, want the declared xhigh", session.Capabilities.Effort)
	}
	if !session.Capabilities.HasCapabilities([]string{"architecture"}) {
		t.Error("capability tags should come from the catalog entry")
	}
}

func TestCapabilityInventoryAnswersServability(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku", "reasoning_effort": "low"})

	code, body := h.do(h.aliceTok, http.MethodGet,
		h.projectPath("/capabilities?tier=T4&effort=xhigh"), nil)
	if code != http.StatusOK {
		t.Fatalf("capabilities = %d\n%s", code, body)
	}
	var inv coord.CapabilityInventory
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if inv.Inventory.MaxTier != domain.TierT1 {
		t.Errorf("max tier = %s, want T1", inv.Inventory.MaxTier)
	}
	if inv.Servable == nil || inv.Servable.Servable {
		t.Fatalf("servable = %+v, want an explicit no", inv.Servable)
	}
	if len(inv.Servable.Reasons) == 0 {
		t.Error("an unservable requirement must say what is missing")
	}

	// And the same question with a floor the live session does clear.
	code, body = h.do(h.aliceTok, http.MethodGet, h.projectPath("/capabilities?tier=T1"), nil)
	if code != http.StatusOK {
		t.Fatalf("capabilities = %d\n%s", code, body)
	}
	inv = coord.CapabilityInventory{}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if inv.Servable == nil || !inv.Servable.Servable {
		t.Errorf("servable = %+v, want yes for T1", inv.Servable)
	}
}

func TestAssignRefusesWhenNothingLiveMeetsTheFloor(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku", "reasoning_effort": "low"})
	task := h.createTask(t, h.aliceTok, "needs a frontier model")

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/assign", map[string]any{
		"require": map[string]any{"tier": "T4", "reasoning_effort": "xhigh"},
	})
	if code != http.StatusConflict {
		t.Fatalf("assign with no capable session = %d, want 409\n%s", code, body)
	}
	var out struct {
		Code     string `json:"code"`
		Rejected []struct {
			Reason string `json:"reason"`
		} `json:"rejected"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Code != "no_capable_session" {
		t.Errorf("code = %q, want no_capable_session", out.Code)
	}
	if len(out.Rejected) != 1 {
		t.Fatalf("rejections = %+v, want the one live session explained", out.Rejected)
	}
}

func TestAssignReachesTheCapableSessionAndTheOfferLands(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	cheap := h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku", "reasoning_effort": "low"})
	strong := h.registerSession(t, h.aliceTok, map[string]any{
		"model": "test-opus", "reasoning_effort": "high", "max_reasoning_effort": "xhigh",
	})
	task := h.createTask(t, h.aliceTok, "coordinate the migration")

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/assign", map[string]any{
		"require": map[string]any{"tier": "T4", "reasoning_effort": "xhigh"},
	})
	if code != http.StatusCreated {
		t.Fatalf("assign = %d\n%s", code, body)
	}
	var result coord.AssignResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Choice.SessionID != strong.ID {
		t.Fatalf("assigned to %s, want the frontier session %s", result.Choice.SessionID, strong.ID)
	}
	if result.Assignment.State != domain.AssignmentOffered {
		t.Errorf("state = %s, want offered", result.Assignment.State)
	}

	// The offer is visible to the session it was made to, and to nobody else.
	code, body = h.do(h.aliceTok, http.MethodGet, "/v1/sessions/"+strong.ID+"/assignments", nil)
	if code != http.StatusOK {
		t.Fatalf("inbox = %d\n%s", code, body)
	}
	var inbox struct {
		Assignments []domain.Assignment `json:"assignments"`
	}
	if err := json.Unmarshal(body, &inbox); err != nil {
		t.Fatalf("decode inbox: %v", err)
	}
	if len(inbox.Assignments) != 1 || inbox.Assignments[0].TaskRef != task.Ref {
		t.Fatalf("inbox = %+v, want the offered task", inbox.Assignments)
	}
	if code, _ := h.do(h.bobTok, http.MethodGet, "/v1/sessions/"+strong.ID+"/assignments", nil); code != http.StatusNotFound {
		t.Errorf("another principal reading the inbox = %d, want 404", code)
	}
	if code, _ := h.do(h.bobTok, http.MethodGet, "/v1/sessions/"+cheap.ID+"/assignments", nil); code != http.StatusOK {
		t.Error("a principal must be able to read their own session's inbox")
	}

	// Only one open offer per task: a second placement is refused rather than duplicated.
	if code, _ := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/assign", map[string]any{
		"require": map[string]any{"tier": "T4"},
	}); code != http.StatusConflict {
		t.Errorf("second assign = %d, want a conflict", code)
	}

	// Accepting is recorded. An accepted offer can still be handed back — someone who took
	// work and then hit something they cannot do must be able to release it.
	code, body = h.do(h.aliceTok, http.MethodPost,
		"/v1/assignments/"+result.Assignment.ID+"/respond", map[string]any{"accept": true})
	if code != http.StatusOK {
		t.Fatalf("accept = %d\n%s", code, body)
	}
	code, body = h.do(h.aliceTok, http.MethodPost,
		"/v1/assignments/"+result.Assignment.ID+"/respond",
		map[string]any{"accept": false, "note": "hit a wall"})
	if code != http.StatusOK {
		t.Fatalf("handing accepted work back = %d\n%s", code, body)
	}
	// But a closed offer is closed: answering a declined one must not revive it.
	code, body = h.do(h.aliceTok, http.MethodPost,
		"/v1/assignments/"+result.Assignment.ID+"/respond", map[string]any{"accept": true})
	if code != http.StatusConflict && code != http.StatusUnprocessableEntity {
		t.Errorf("answering a closed offer = %d, want a refusal\n%s", code, body)
	}
}

// Naming a session directly is a preference, not a way around the floor.
func TestAssignToNamedSessionStillChecksTheFloor(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	cheap := h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku", "reasoning_effort": "low"})
	task := h.createTask(t, h.aliceTok, "security review")

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/assign", map[string]any{
		"session_id": cheap.ID,
		"require":    map[string]any{"tier": "T4"},
	})
	if code != http.StatusConflict {
		t.Fatalf("assign to an under-powered named session = %d, want 409\n%s", code, body)
	}
}

func TestUnknownEffortIsRejectedRatherThanMatchingNothing(t *testing.T) {
	h := newHarness(t)
	task := h.createTask(t, h.aliceTok, "typo in the floor")

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/assign", map[string]any{
		"require": map[string]any{"reasoning_effort": "xhig"},
	})
	if code != http.StatusBadRequest && code != http.StatusUnprocessableEntity {
		t.Fatalf("typo'd effort = %d, want a validation failure\n%s", code, body)
	}
}

func TestSessionCapabilitiesCanBeUpdatedMidSession(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)
	session := h.registerSession(t, h.aliceTok, map[string]any{"model": "test-haiku", "reasoning_effort": "low"})

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/capabilities",
		map[string]any{"model": "test-opus", "reasoning_effort": "xhigh"})
	if code != http.StatusOK {
		t.Fatalf("update capabilities = %d\n%s", code, body)
	}
	var updated domain.Session
	if err := json.Unmarshal(body, &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Capabilities.Tier != domain.TierT4 {
		t.Errorf("tier = %s, want T4 after switching model", updated.Capabilities.Tier)
	}
	if code, _ := h.do(h.bobTok, http.MethodPost, "/v1/sessions/"+session.ID+"/capabilities",
		map[string]any{"model": "test-haiku"}); code == http.StatusOK {
		t.Error("another principal must not be able to rewrite a session's capabilities")
	}
}
