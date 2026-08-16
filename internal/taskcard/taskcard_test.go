package taskcard

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

func TestRoundTrip(t *testing.T) {
	original := Card{
		ID:          "T-42",
		Project:     "conductor",
		ExternalRef: "GH-812",
		Status:      "running",
		Visibility:  "team_summary",
		Owner:       "user:adam",
		Executor:    "agent:opencode/attempt-7",
		Lease: &CardLease{
			ID: "lease_789", FencingEpoch: 7,
			ExpiresAt: time.Date(2026, 8, 16, 18, 1, 30, 0, time.UTC),
		},
		BaseSHA:     "abc123",
		Branch:      "agent/T-42/attempt-7",
		Worktree:    "/var/lib/conductor/worktrees/T-42-a7",
		WorkflowSHA: "def456",
		Route: &CardRoute{
			Role: "implementer", Harness: "opencode",
			ModelAlias: "worker.fast", ResolvedModel: "provider/model-id",
			ReasoningEffort: "low",
		},
		Budget: &CardBudget{Class: "small", MaxAttempts: 3},
		Scopes: []CardScope{
			{Resource: "dir:packages/router", Mode: "write_exclusive"},
			{Resource: "path:packages/config/schema.ts", Mode: "write_exclusive"},
		},
		Dependencies:       []string{"T-39"},
		Checks:             []string{"go test ./...", "go vet ./..."},
		Title:              "Add retry-aware model routing",
		Objective:          "Implement deterministic routing rules before any learned classifier.",
		AcceptanceCriteria: []string{"Hard policy cannot be overridden", "Unit tests cover escalation"},
		CurrentProgress:    "Policy evaluator implemented. Tests in progress.",
		Decisions:          []string{"PostgreSQL remains authoritative"},
		OpenQuestions:      []string{"Should rate-limit exhaustion prefer another provider?"},
		Artifacts:          []string{"Commit: not yet published"},
	}

	rendered, err := original.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	parsed, err := Parse(rendered)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !reflect.DeepEqual(original, parsed) {
		t.Errorf("round trip lost data.\noriginal: %+v\nparsed:   %+v", original, parsed)
	}
}

func TestRenderIncludesPrivacyNote(t *testing.T) {
	out, err := Card{ID: "T-1", Title: "x"}.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, PrivacyNote) {
		t.Error("rendered card is missing the privacy note")
	}
}

func TestFromTask(t *testing.T) {
	task := domain.Task{
		Ref: "T-9", Title: "Do the thing", Objective: "Because",
		Status: domain.TaskRunning, Visibility: domain.VisibilityTeamSummary,
		MaxAttempts: 3, BaseSHA: "base1",
		AcceptanceCriteria: []domain.AcceptanceCriterion{
			{Text: "It works", Status: "pass"},
			{Text: "It is tested"},
		},
	}
	attempt := &domain.Attempt{
		AttemptNumber: 2, Harness: "claude", Branch: "agent/T-9/attempt-2",
		Role: domain.RoleImplementer, ModelAlias: "worker.general",
		ResolvedModel: "some-model", ReasoningEffort: domain.EffortMedium,
		CommitSHA: "commit1",
	}
	card := FromTask(task, "proj", "adam", attempt, nil,
		[]domain.ScopeReservation{
			{ResourceType: domain.ResourceDir, ResourceKey: "internal/api", Mode: domain.ModeWriteExclusive},
		},
		[]string{"T-8"}, []string{"go test ./..."})

	if card.ID != "T-9" || card.Owner != "user:adam" {
		t.Errorf("identity fields wrong: %+v", card)
	}
	if card.Executor != "agent:claude/attempt-2" {
		t.Errorf("executor = %q", card.Executor)
	}
	if len(card.Scopes) != 1 || card.Scopes[0].Resource != "dir:internal/api" {
		t.Errorf("scopes = %+v", card.Scopes)
	}
	if card.AcceptanceCriteria[0] != "It works — pass" {
		t.Errorf("criterion status not rendered: %q", card.AcceptanceCriteria[0])
	}
}

// The instruction is the entire input to an attempt. It must carry forward structured
// continuation state and never suggest that a prior transcript exists to be retrieved.
func TestInstructionCarriesHandoffNotTranscript(t *testing.T) {
	card := Card{ID: "T-9", Title: "Do the thing", Checks: []string{"go test ./..."}}
	handoff := &domain.HandoffBundle{
		RecommendedNextAction: "Write the tests",
		CompletedWork:         []string{"Implemented the evaluator"},
		OpenQuestions:         []string{"Which provider on rate limit?"},
		Decisions: []domain.DecisionRecord{
			{Summary: "Postgres stays authoritative", By: "adam"},
		},
		Validation: []domain.ValidationResult{
			{Command: "go test ./...", ExitCode: 1},
		},
	}

	out, err := Instruction(card, "# Workflow\n\nFollow the rules.", handoff)
	if err != nil {
		t.Fatalf("Instruction: %v", err)
	}

	for _, want := range []string{
		"Write the tests",
		"Implemented the evaluator",
		"Which provider on rate limit?",
		"Postgres stays authoritative",
		"go test ./...",
		"failed (exit 1)",
		"Follow the rules.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instruction missing %q", want)
		}
	}
	if !strings.Contains(out, "not receiving the previous session's conversation") {
		t.Error("instruction should state plainly that no transcript is provided")
	}
}

func TestParseHandlesCardWithoutFrontmatter(t *testing.T) {
	c, err := Parse("# Just a title\n\n## Objective\n\nDo it.\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Title != "Just a title" || c.Objective != "Do it." {
		t.Errorf("parsed = %+v", c)
	}
}

func TestParseRejectsUnterminatedFrontmatter(t *testing.T) {
	if _, err := Parse("---\nid: T-1\n"); err == nil {
		t.Error("unterminated frontmatter should be an error, not a silent empty card")
	}
}
