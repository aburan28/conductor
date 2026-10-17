package quality

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

func TestHeuristicFlagsEmptyEnvelope(t *testing.T) {
	res := Lint(context.Background(), Envelope{}, Options{})
	if !res.Advisory {
		t.Fatal("advisory must always be true")
	}
	if res.Engine != "heuristic" {
		t.Fatalf("engine = %q", res.Engine)
	}
	codes := map[string]bool{}
	for _, f := range res.Findings {
		codes[f.Code] = true
	}
	for _, want := range []string{"missing_summary", "missing_acceptance_criteria", "missing_scopes"} {
		if !codes[want] {
			t.Errorf("missing finding %s in %#v", want, res.Findings)
		}
	}
}

func TestHeuristicHappyPath(t *testing.T) {
	res := Lint(context.Background(), Envelope{
		Summary:   "Add advisory quality.lint for public task envelopes",
		Objective: "Ship POST quality/lint with heuristic findings",
		AcceptanceCriteria: []domain.AcceptanceCriterion{
			{Text: "go test ./internal/quality/... passes"},
		},
		Scopes: []domain.ScopeRequest{{Resource: "dir:internal/quality", Mode: domain.ModeWriteExclusive}},
		Labels: []string{"quality"},
	}, Options{})
	for _, f := range res.Findings {
		if f.Severity == "warning" || f.Severity == "error" {
			t.Errorf("unexpected %s finding: %+v", f.Severity, f)
		}
	}
}

func TestSecretLikeRedacted(t *testing.T) {
	res := Lint(context.Background(), Envelope{
		Summary: "rotate sk-abcdefghijklmnopqrstuvwxyz0123456789 before merge",
		Scopes:  []domain.ScopeRequest{{Resource: "path:README.md"}},
	}, Options{})
	var saw bool
	for _, f := range res.Findings {
		if f.Code == "secret_like" {
			saw = true
		}
		if strings.Contains(f.Message, "sk-abcdefghijklmnopqrstuvwxyz") {
			t.Fatalf("finding leaked secret material: %s", f.Message)
		}
	}
	if !saw {
		t.Fatal("expected secret_like finding")
	}
}

func TestPrivateRedacted(t *testing.T) {
	res := Lint(context.Background(), Envelope{
		Summary:       "secret intent about billing",
		Objective:     "do not leak",
		RedactPrivate: true,
		Scopes:        []domain.ScopeRequest{{Resource: "dir:internal/billing"}},
	}, Options{})
	if !res.Redacted {
		t.Fatal("want redacted")
	}
	for _, f := range res.Findings {
		if strings.Contains(strings.ToLower(f.Message), "billing intent") ||
			strings.Contains(f.Message, "secret intent") ||
			strings.Contains(f.Message, "do not leak") {
			t.Fatalf("private text leaked: %+v", f)
		}
	}
}

func TestVagueCriterion(t *testing.T) {
	res := Lint(context.Background(), Envelope{
		Summary:            "Tighten task card acceptance checks for agents",
		AcceptanceCriteria: []domain.AcceptanceCriterion{{Text: "done"}},
		Scopes:             []domain.ScopeRequest{{Resource: "path:x.go"}},
	}, Options{})
	var saw bool
	for _, f := range res.Findings {
		if f.Code == "vague_criterion" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("expected vague_criterion")
	}
}

func TestOverbroadDirScope(t *testing.T) {
	res := Lint(context.Background(), Envelope{
		Summary: "Touch everything in the repository root directory",
		Scopes:  []domain.ScopeRequest{{Resource: "dir:."}},
	}, Options{})
	var saw bool
	for _, f := range res.Findings {
		if f.Code == "overbroad_scope" {
			saw = true
		}
	}
	if !saw {
		t.Fatal("expected overbroad_scope")
	}
}

func TestEnhanceMergesSoftly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{
					"content": `[{"severity":"info","code":"llm_tip","where":"objective","message":"name the validation command"}]`,
				}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	res := Lint(context.Background(), Envelope{
		Summary: "Wire quality lint enhance path to local OpenAI-compatible endpoint",
		Scopes:  []domain.ScopeRequest{{Resource: "path:internal/quality/enhance.go"}},
		Labels:  []string{"quality"},
		AcceptanceCriteria: []domain.AcceptanceCriterion{
			{Text: "enhance test passes with httptest"},
		},
	}, Options{Enhancer: &OpenAIEnhancer{BaseURL: srv.URL + "/v1", Model: "tiny"}})
	if !res.Enhanced || res.Engine != "heuristic+llm" {
		t.Fatalf("enhanced=%v engine=%s", res.Enhanced, res.Engine)
	}
	var saw bool
	for _, f := range res.Findings {
		if f.Code == "llm_tip" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("missing llm finding: %#v", res.Findings)
	}
}

func TestEnhanceFailureFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	res := Lint(context.Background(), Envelope{
		Summary: "Fallback when enhance endpoint is unavailable",
		Scopes:  []domain.ScopeRequest{{Resource: "path:x.go"}},
	}, Options{Enhancer: &OpenAIEnhancer{BaseURL: srv.URL}})
	if res.Enhanced || res.Engine != "heuristic" {
		t.Fatalf("want heuristic fallback, got enhanced=%v engine=%s", res.Enhanced, res.Engine)
	}
	if !res.Advisory {
		t.Fatal("still advisory")
	}
}
