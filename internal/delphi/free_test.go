package delphi_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/delphi"
)

// freeFake is a fake provider that knows which of its models cost nothing.
type freeFake struct {
	*fakeProvider
	local bool            // an endpoint you run: every model is free and private
	free  map[string]bool // third-party models that are free
}

func (p *freeFake) Free(model string) (bool, bool) {
	if p.local {
		return true, true
	}
	return p.free[model], false
}

func freeRegistry() (delphi.Registry, *fakeProvider) {
	paid := newFake("anthropic")
	return delphi.Registry{
		"anthropic":  paid,
		"vllm":       &freeFake{fakeProvider: newFake("vllm"), local: true},
		"openrouter": &freeFake{fakeProvider: newFake("openrouter"), free: map[string]bool{"laguna:free": true}},
	}, paid
}

func TestParseFreeMode(t *testing.T) {
	for in, want := range map[string]delphi.FreeMode{"": "", "local": delphi.FreeLocal, " public ": delphi.FreePublic} {
		if got, err := delphi.ParseFreeMode(in); err != nil || got != want {
			t.Errorf("ParseFreeMode(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := delphi.ParseFreeMode("yes"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestFreeModeSubstitutesRefusesAndFiltersFallbacks(t *testing.T) {
	reg, _ := freeRegistry()
	policy := func(mode delphi.FreeMode) *delphi.FreePolicy {
		return &delphi.FreePolicy{Mode: mode, Substitutes: map[string]delphi.Target{
			"anthropic:claude-sonnet-5-5": {Provider: "vllm", Model: "glm-5.3"},
			"anthropic:*":                 {Provider: "vllm", Model: "qwen3.8"},
		}}
	}
	norm := func(row map[string]any, fp *delphi.FreePolicy) (delphi.Request, []string) {
		return delphi.Normalize(row, delphi.Defaults{Provider: "anthropic", MaxTokens: 100, Free: fp}, reg, "row")
	}

	// The exact key wins over the provider wildcard; effort and params, written for the
	// paid provider's API, are dropped; the original target is recorded.
	r, p := norm(map[string]any{"id": "a", "prompt": "x", "model": "claude-sonnet-5-5", "effort": "high",
		"params": map[string]any{"thinking": true}}, policy(delphi.FreeLocal))
	if len(p) > 0 || r.Provider != "vllm" || r.Model != "glm-5.3" || r.Effort != "" || r.Params != nil ||
		r.Meta["free_substitute_for"] != "anthropic:claude-sonnet-5-5@high" {
		t.Fatalf("substitution = %+v, %v", r, p)
	}
	r, _ = norm(map[string]any{"id": "b", "prompt": "x", "model": "claude-haiku-5-5"}, policy(delphi.FreeLocal))
	if r.Model != "qwen3.8" {
		t.Errorf("wildcard substitute = %s:%s", r.Provider, r.Model)
	}

	// A paid target with no substitute is refused before anything is sent.
	_, p = norm(map[string]any{"id": "c", "prompt": "x", "model": "m"}, &delphi.FreePolicy{Mode: delphi.FreeLocal})
	if len(p) != 1 || !strings.Contains(p[0], "free mode") || !strings.Contains(p[0], "paid") {
		t.Errorf("no substitute: %v", p)
	}

	// A third-party free tier is public: refused in local mode, allowed in public mode.
	row := map[string]any{"id": "d", "prompt": "x", "provider": "openrouter", "model": "laguna:free"}
	if _, p := norm(row, policy(delphi.FreeLocal)); len(p) == 0 || !strings.Contains(p[0], "third-party") {
		t.Errorf("public free tier in local mode: %v", p)
	}
	if r, p := norm(row, policy(delphi.FreePublic)); len(p) > 0 || r.Provider != "openrouter" || r.Meta["free_substitute_for"] != "" {
		t.Errorf("public free tier in public mode: %+v %v", r, p)
	}
	// A paid model on that endpoint is still paid.
	if _, p := norm(map[string]any{"id": "e", "prompt": "x", "provider": "openrouter", "model": "gpt-pro"},
		policy(delphi.FreePublic)); len(p) == 0 {
		t.Error("a paid OpenRouter model passed")
	}

	// Fallbacks: a paid target is substituted, a disallowed free tier is dropped.
	r, p = norm(map[string]any{"id": "f", "prompt": "x", "provider": "vllm", "model": "glm-5.3",
		"fallback": []any{"anthropic:claude-opus-5-5", "openrouter:laguna:free"}}, policy(delphi.FreeLocal))
	if len(p) > 0 || len(r.Fallback) != 1 || r.Fallback[0] != (delphi.Target{Provider: "vllm", Model: "qwen3.8"}) {
		t.Errorf("fallback chain = %+v, %v", r.Fallback, p)
	}
}

func TestEngineNeverSubmitsAPaidRequestInFreeMode(t *testing.T) {
	reg, paid := freeRegistry()
	e, st, now := setup(t)
	e.Providers = reg
	e.Free = &delphi.FreePolicy{Mode: delphi.FreeLocal}
	// Requests that reached the queue some other way than Normalize.
	queued := append(reqs("anthropic", "paid-1", "paid-2"), reqs("vllm", "free-1")...)
	if _, err := st.Enqueue(queued, *now); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := e.Tick(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		reg["vllm"].(*freeFake).endAll() // the free endpoint's batch finishes
	}
	paid.mu.Lock()
	sent := len(paid.submits)
	paid.mu.Unlock()
	if sent != 0 {
		t.Fatalf("free mode submitted %d paid batch(es)", sent)
	}
	for _, id := range []string{"paid-1", "paid-2"} {
		r, ok, err := st.Result(id)
		if err != nil || !ok || r.Status != delphi.Errored || r.Error == nil || r.Error.Type != "free_mode_refused" || r.Retryable() {
			t.Errorf("%s = %+v (ok %v, err %v)", id, r, ok, err)
		}
	}
	if r, ok, _ := st.Result("free-1"); !ok || r.Status != delphi.Succeeded {
		t.Errorf("the free request was not answered: %+v", r)
	}
}
