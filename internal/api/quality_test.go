package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

func TestQualityLintRequiresAuth(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do("", http.MethodPost, h.projectPath("/quality/lint"), map[string]any{
		"summary": "unauthenticated quality lint must be rejected by the API",
	})
	if code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestQualityLintRejectsOutsider(t *testing.T) {
	h := newHarness(t)
	code, _ := h.do(h.outTok, http.MethodPost, h.projectPath("/quality/lint"), map[string]any{
		"summary": "outsider must not lint this project's envelopes",
	})
	if code != http.StatusNotFound && code != http.StatusForbidden {
		t.Fatalf("status = %d, want 404 or 403", code)
	}
}

func TestQualityLintHeuristic(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/quality/lint"), map[string]any{
		"summary":   "Add quality.lint advisory findings for public task envelopes",
		"objective": "Ship heuristic lint with advisory:true",
		"acceptance_criteria": []map[string]any{
			{"text": "go test ./internal/quality/... passes"},
		},
		"scopes": []map[string]any{
			{"resource": "dir:internal/quality", "mode": "write_exclusive"},
		},
		"labels": []string{"quality"},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%s", code, body)
	}
	var out struct {
		Advisory bool   `json:"advisory"`
		Engine   string `json:"engine"`
		Findings []struct {
			Code string `json:"code"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Advisory {
		t.Fatal("advisory must be true")
	}
	if out.Engine != "heuristic" {
		t.Fatalf("engine = %q", out.Engine)
	}
}

func TestQualityLintEmptyEnvelopeFindings(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.bobTok, http.MethodPost, h.projectPath("/quality/lint"), map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%s", code, body)
	}
	var out struct {
		Advisory bool `json:"advisory"`
		Findings []struct {
			Code string `json:"code"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Advisory {
		t.Fatal("advisory must be true")
	}
	codes := map[string]bool{}
	for _, f := range out.Findings {
		codes[f.Code] = true
	}
	if !codes["missing_summary"] {
		t.Fatalf("want missing_summary, got %#v", out.Findings)
	}
}

func TestQualityLintPrivateTaskRedacted(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	task, err := h.store.CreateTask(ctx, db.CreateTaskParams{
		ProjectID:  h.project.ID,
		CreatedBy:  h.alice.ID,
		Title:      "private billing rewrite",
		Objective:  "must not leak to bob",
		Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code, body := h.do(h.bobTok, http.MethodPost, h.projectPath("/quality/lint"), map[string]any{
		"task": task.Ref,
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d body=%s", code, body)
	}
	var out struct {
		Redacted bool `json:"redacted"`
		Findings []struct {
			Message string `json:"message"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Redacted {
		t.Fatal("want redacted for non-owner")
	}
	raw := string(body)
	for _, n := range []string{"private billing", "must not leak"} {
		if strings.Contains(raw, n) {
			t.Fatalf("leaked private text %q in %s", n, raw)
		}
	}
}
