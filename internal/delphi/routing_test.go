package delphi_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
)

func refuseOn(model string) func(delphi.Request, int) (delphi.Result, bool) {
	return func(r delphi.Request, _ int) (delphi.Result, bool) {
		if r.Model == model {
			return delphi.Result{ID: r.ID, Status: delphi.Refused, Refusal: &delphi.Refusal{Category: "cyber"}}, true
		}
		return delphi.Result{ID: r.ID, Status: delphi.Succeeded, Text: r.Provider + ":" + r.Model + " answered"}, true
	}
}

func TestRefusalFallsBackAlongTheChain(t *testing.T) {
	a, o := newFake("anthropic"), newFake("openai")
	a.answer = refuseOn("m1")
	o.answer = refuseOn("o-strict")
	e, st, _ := setup(t, a, o)
	r := reqs("anthropic", "q1")[0]
	r.Effort = "max"
	r.Params = map[string]any{"thinking": map[string]any{"type": "adaptive"}}
	r.Fallback = []delphi.Target{{Provider: "nowhere", Model: "x"}, {Provider: "openai", Model: "o-strict"},
		{Provider: "openai", Model: "o-loose", Effort: "high"}}
	if _, err := st.Enqueue([]delphi.Request{r}, *new(time.Time)); err != nil {
		t.Fatal(err)
	}
	var fellBack int
	for i := 0; i < 3; i++ {
		if _, err := e.Tick(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		a.endAll()
		o.endAll()
		rep, err := e.Tick(context.Background(), false)
		if err != nil {
			t.Fatal(err)
		}
		fellBack += rep.FellBack
	}
	res, ok, err := st.Result("q1")
	if err != nil || !ok {
		t.Fatalf("no result: %v", err)
	}
	if res.Status != delphi.Succeeded || res.Text != "openai:o-loose answered" || fellBack != 2 {
		t.Fatalf("result %+v, fell back %d times", res, fellBack)
	}
	// The unconfigured provider is skipped, and recorded as passed over.
	want := []string{"anthropic:m1 refused (cyber)", "nowhere:x skipped (provider not configured)", "openai:o-strict refused (cyber)"}
	if !reflect.DeepEqual(res.FallbackFrom, want) {
		t.Fatalf("fallback_from = %q", res.FallbackFrom)
	}
	// Crossing providers drops the provider-specific params and effort; the target's effort applies.
	last := o.submits[len(o.submits)-1][0]
	if last.Params != nil || last.Effort != "high" || last.ID != "q1" {
		t.Fatalf("resent as %+v", last)
	}
	if mid := o.submits[0][0]; mid.Effort != "" {
		t.Fatalf("effort carried across providers: %+v", mid)
	}
	// The original request is still recognised: enqueueing it again is a no-op, not a conflict.
	if n, err := st.Enqueue([]delphi.Request{r}, *new(time.Time)); err != nil || n != 0 {
		t.Fatalf("re-enqueue after fallback = %d, %v", n, err)
	}
}

func TestRefusalWithoutFallbackFinishes(t *testing.T) {
	a := newFake("anthropic")
	a.answer = refuseOn("m1")
	e, st, _ := setup(t, a)
	if _, err := st.Enqueue(reqs("anthropic", "q1"), *new(time.Time)); err != nil {
		t.Fatal(err)
	}
	e.Tick(context.Background(), true)
	a.endAll()
	rep, _ := e.Tick(context.Background(), false)
	res, _, _ := st.Result("q1")
	if res.Status != delphi.Refused || rep.FellBack != 0 || len(a.submits) != 1 {
		t.Fatalf("%+v %+v", res, rep)
	}
}

func TestParseTargetsAndProfiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	b, _ := json.Marshal(map[string]any{
		"implementer": map[string]any{"provider": "anthropic", "model": "claude-haiku-5-5", "effort": "max"},
		"frontier": map[string]any{"provider": "anthropic", "model": "claude-mythos-5-1",
			"fallback": []string{"openai:gpt-x@high", "implementer"}},
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	profiles, err := delphi.LoadProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if missing, err := delphi.LoadProfiles(filepath.Join(dir, "nope.json")); err != nil || len(missing) != 0 {
		t.Fatalf("missing file = %v, %v", missing, err)
	}
	reg := delphi.Registry{"anthropic": newFake("anthropic"), "openai": newFake("openai")}
	got, err := delphi.ParseTargets([]string{"openai:gpt-x@high", " implementer ", "anthropic:", ""}, profiles, reg)
	if err != nil {
		t.Fatal(err)
	}
	want := []delphi.Target{{Provider: "openai", Model: "gpt-x", Effort: "high"},
		{Provider: "anthropic", Model: "claude-haiku-5-5", Effort: "max"}, {Provider: "anthropic", Model: "m1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targets = %+v", got)
	}
	for _, bad := range []string{"nosuch", "gemini:x"} {
		if _, err := delphi.ParseTargets([]string{bad}, profiles, reg); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}

	d := delphi.Defaults{Provider: "openai", Model: "gpt-default", MaxTokens: 1000, Profiles: profiles}
	row := map[string]any{"id": "a", "prompt": "hi", "profile": "frontier"}
	r, p := delphi.Normalize(row, d, reg, "row")
	if len(p) > 0 {
		t.Fatal(p)
	}
	if r.Provider != "anthropic" || r.Model != "claude-mythos-5-1" || r.Profile != "frontier" || len(r.Fallback) != 2 ||
		r.Fallback[1].Model != "claude-haiku-5-5" {
		t.Fatalf("profile row = %+v", r)
	}
	// Row keys override the profile; a row fallback replaces the profile's.
	row = map[string]any{"id": "b", "prompt": "hi", "profile": "implementer", "effort": "low", "fallback": "openai:gpt-y"}
	r, p = delphi.Normalize(row, d, reg, "row")
	if len(p) > 0 || r.Model != "claude-haiku-5-5" || r.Effort != "low" || len(r.Fallback) != 1 || r.Fallback[0].Model != "gpt-y" {
		t.Fatalf("override row = %+v %v", r, p)
	}
	// A provider change without a model takes the new provider's default, not the old default model.
	r, p = delphi.Normalize(map[string]any{"id": "c", "prompt": "hi", "provider": "anthropic"}, d, reg, "row")
	if len(p) > 0 || r.Model != "m1" {
		t.Fatalf("provider switch = %+v %v", r, p)
	}
	_, p = delphi.Normalize(map[string]any{"id": "d", "prompt": "hi", "profile": "ghost"}, d, reg, "row")
	if len(p) == 0 || !strings.Contains(strings.Join(p, " "), "ghost") {
		t.Fatalf("unknown profile: %v", p)
	}
}
