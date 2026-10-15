package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/modelsrc"
	"github.com/aburan28/conductor/internal/secretbox"
	"github.com/aburan28/conductor/internal/secrets"
)

// fakeProvider is an Anthropic-shaped /v1/models whose catalogue the test can change, standing
// in for a provider that ships a model between two refreshes.
type fakeProvider struct {
	mu     sync.Mutex
	models []string
	keys   []string
	srv    *httptest.Server
}

func newFakeProvider(t *testing.T, models ...string) *fakeProvider {
	t.Helper()
	f := &fakeProvider{models: models}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		type row struct {
			ID        string `json:"id"`
			CreatedAt string `json:"created_at"`
		}
		rows := []row{}
		for i, m := range f.models {
			rows = append(rows, row{ID: m, CreatedAt: "2026-0" + string(rune('1'+i)) + "-01T00:00:00Z"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "has_more": false})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) set(models ...string) {
	f.mu.Lock()
	f.models = models
	f.mu.Unlock()
}

func (h *harness) withDiscovery(t *testing.T) *secrets.Vault {
	t.Helper()
	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := secrets.New(h.store, &secretbox.Source{Env: raw})
	disc := &modelsrc.Service{
		Store: h.store, Lister: modelsrc.NewHTTPLister(),
		Key: func(ctx context.Context, p domain.Project, name string) (string, error) {
			o, err := v.Open(ctx, p, name)
			return o.Value, err
		},
	}
	h.server.Close()
	h.server = httptest.NewServer(New(h.store, coord.New(h.store), Options{
		Secrets: v, Models: disc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler())
	t.Cleanup(h.server.Close)
	t.Cleanup(func() {
		pool := h.store.Pool()
		_, _ = pool.Exec(context.Background(), `DELETE FROM inference_endpoints WHERE project_id = $1::uuid`, h.project.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_secrets WHERE project_id = $1::uuid`, h.project.ID)
	})
	return v
}

func TestInferenceDiscoveryEndToEnd(t *testing.T) {
	h := newHarness(t)
	h.withDiscovery(t)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	provider := newFakeProvider(t, "claude-sonnet-5-5", "claude-opus-5")
	ctx := context.Background()

	// The catalog says what it wants, not which model it is.
	if _, err := h.store.UpsertModelProfile(ctx, domain.ModelProfile{
		OrganizationID: h.org.ID, Alias: "worker.general", Harness: "claude", Provider: "anthropic",
		Tier: domain.TierT2, ReasoningEffort: domain.EffortMedium, Billing: "tokens", Enabled: true,
		Select: &domain.ModelSelector{Endpoint: "anthropic", Match: []string{"claude-sonnet-*"}, ProjectID: h.project.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Roles: members read, only maintainers change.
	body := map[string]any{"preset": "anthropic", "base_url": provider.srv.URL, "api_key_secret": "ANTHROPIC_API_KEY"}
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/inference/endpoints"), body); code != http.StatusForbidden {
		t.Fatalf("contributor POST = %d", code)
	}
	// A key reference the vault cannot satisfy is refused up front, not at first refresh.
	if code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"), body); code != http.StatusBadRequest {
		t.Fatalf("missing secret = %d\n%s", code, resp)
	}
	if code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/secrets"),
		map[string]any{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-test"}); code != http.StatusCreated {
		t.Fatalf("seed secret = %d\n%s", code, resp)
	}

	// Cloud metadata addresses are refused whatever else is right.
	meta := map[string]any{"name": "meta", "kind": "openai", "base_url": "http://169.254.169.254/latest"}
	if code, _ := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"), meta); code != http.StatusBadRequest {
		t.Fatalf("metadata address = %d", code)
	}

	code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"), body)
	if code != http.StatusCreated {
		t.Fatalf("create = %d\n%s", code, resp)
	}
	if strings.Contains(string(resp), "sk-ant-test") {
		t.Fatalf("create response leaked the key: %s", resp)
	}
	var created endpointResponse
	if err := json.Unmarshal(resp, &created); err != nil {
		t.Fatal(err)
	}
	if created.RefreshError != "" || created.Endpoint.ModelCount != 2 || created.Endpoint.LastStatus != "ok" || created.Endpoint.Name != "anthropic" {
		t.Fatalf("create did not discover: %+v", created)
	}
	if got := provider.keys[0]; got != "sk-ant-test" {
		t.Fatalf("the vault key never reached the provider: %q", got)
	}

	// The dropdown's data: every member can read it, and the catalog has followed.
	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/inference/models"), nil)
	if code != http.StatusOK {
		t.Fatalf("list models = %d\n%s", code, resp)
	}
	var listed struct{ Models []ModelChoice }
	if err := json.Unmarshal(resp, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Models) != 2 {
		t.Fatalf("models = %+v", listed.Models)
	}
	for _, m := range listed.Models {
		if m.Model == "claude-sonnet-5-5" && (len(m.UsedBy) != 1 || m.UsedBy[0] != "worker.general/claude") {
			t.Fatalf("used_by = %v", m.UsedBy)
		}
		if m.New {
			t.Fatalf("a first refresh must not flag everything new: %+v", m)
		}
	}

	// A release lands at the provider. A maintainer's refresh (or the hourly loop) adopts it.
	provider.set("claude-sonnet-5-5", "claude-sonnet-5-6", "claude-opus-5")
	h.expireRefreshGap(t, created.Endpoint.ID)
	code, resp = h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints/anthropic/refresh"), nil)
	if code != http.StatusOK {
		t.Fatalf("refresh = %d\n%s", code, resp)
	}
	var refreshed endpointResponse
	_ = json.Unmarshal(resp, &refreshed)
	if refreshed.Refresh == nil || len(refreshed.Refresh.Added) != 1 || refreshed.Refresh.Added[0] != "claude-sonnet-5-6" {
		t.Fatalf("refresh = %s", resp)
	}
	profiles, _ := h.store.ListModelProfiles(ctx, h.org.ID)
	for _, p := range profiles {
		if p.Alias == "worker.general" && p.Model != "claude-sonnet-5-6" {
			t.Fatalf("catalog did not follow the release: %q", p.Model)
		}
	}

	// Rotating the key to a wrong value is reported on the endpoint, and the models stay.
	h.do(maintTok, http.MethodPost, h.projectPath("/secrets"), map[string]any{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-wrong"})
	h.expireRefreshGap(t, created.Endpoint.ID)
	code, resp = h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints/anthropic/refresh"), nil)
	if code != http.StatusOK || !strings.Contains(string(resp), "rejected the API key") || strings.Contains(string(resp), "sk-ant-wrong") {
		t.Fatalf("rejected key: %d\n%s", code, resp)
	}
	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/inference/endpoints"), nil)
	if code != http.StatusOK || !strings.Contains(string(resp), `"last_status":"error"`) || !strings.Contains(string(resp), `"model_count":3`) {
		t.Fatalf("endpoint list after failure: %d\n%s", code, resp)
	}

	// Removing the endpoint removes its models; the catalog keeps the last model it had.
	if code, _ := h.do(h.bobTok, http.MethodDelete, h.projectPath("/inference/endpoints/anthropic"), nil); code != http.StatusForbidden {
		t.Fatalf("contributor DELETE = %d", code)
	}
	if code, _ := h.do(maintTok, http.MethodDelete, h.projectPath("/inference/endpoints/anthropic"), nil); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/inference/models"), nil)
	if code != http.StatusOK || !strings.Contains(string(resp), `"models":[]`) {
		t.Fatalf("models after delete: %d %s", code, resp)
	}
}

// expireRefreshGap backdates the last refresh so a request-driven refresh is not answered from
// the stored result.
func (h *harness) expireRefreshGap(t *testing.T, id domain.ID) {
	t.Helper()
	if _, err := h.store.Pool().Exec(context.Background(),
		`UPDATE inference_endpoints SET last_refresh_at = now() - interval '1 minute' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
}

func TestInferenceUpdateKeepsOrClearsTheKey(t *testing.T) {
	h := newHarness(t)
	h.withDiscovery(t)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	h.do(maintTok, http.MethodPost, h.projectPath("/secrets"), map[string]any{"name": "K", "value": "v"})

	create := map[string]any{"name": "gw", "kind": "openai_compatible", "base_url": "http://localhost:1/v1", "api_key_secret": "K", "enabled": false}
	if code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"), create); code != http.StatusCreated {
		t.Fatalf("create = %d\n%s", code, resp)
	}
	// Leaving the field out keeps the key; sending "" drops it.
	code, resp := h.do(maintTok, http.MethodPut, h.projectPath("/inference/endpoints/gw"), map[string]any{"enabled": false})
	if code != http.StatusOK || !strings.Contains(string(resp), `"api_key_secret":"K"`) {
		t.Fatalf("omitted field: %d\n%s", code, resp)
	}
	code, resp = h.do(maintTok, http.MethodPut, h.projectPath("/inference/endpoints/gw"), map[string]any{"api_key_secret": ""})
	if code != http.StatusOK || strings.Contains(string(resp), "api_key_secret") {
		t.Fatalf("cleared field: %d\n%s", code, resp)
	}
	// Duplicate names are refused; so are names that are not slugs.
	if code, _ := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"), create); code != http.StatusBadRequest {
		t.Fatalf("duplicate = %d", code)
	}
	create["name"] = "Has Spaces"
	if code, _ := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"), create); code != http.StatusBadRequest {
		t.Fatalf("bad name = %d", code)
	}
}

func TestInferenceRoutesReportNotConfigured(t *testing.T) {
	h := newHarness(t)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	code, _ := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"),
		map[string]any{"name": "x", "kind": "ollama", "base_url": "http://localhost:11434"})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("POST without discovery = %d, want 503", code)
	}
	// Reading is harmless without it: nothing is configured, and nothing is promised.
	if code, _ := h.do(h.bobTok, http.MethodGet, h.projectPath("/inference/endpoints"), nil); code != http.StatusOK {
		t.Fatalf("GET without discovery = %d", code)
	}
}

func TestPinnedModelSurvivesDiscoveryAndBootstrapUntilUnpinned(t *testing.T) {
	h := newHarness(t)
	h.withDiscovery(t)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	provider := newFakeProvider(t, "claude-sonnet-5-5")
	ctx := context.Background()

	declare := domain.ModelProfile{
		OrganizationID: h.org.ID, Alias: "worker.general", Harness: "claude", Provider: "anthropic",
		Model: "claude-sonnet-5", Tier: domain.TierT2, ReasoningEffort: domain.EffortMedium,
		Billing: "tokens", Enabled: true,
		Select: &domain.ModelSelector{Endpoint: "anthropic", Match: []string{"claude-sonnet-*"}, ProjectID: h.project.ID},
	}
	if _, err := h.store.UpsertModelProfile(ctx, declare); err != nil {
		t.Fatal(err)
	}
	h.do(maintTok, http.MethodPost, h.projectPath("/secrets"), map[string]any{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-test"})
	if code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints"),
		map[string]any{"preset": "anthropic", "base_url": provider.srv.URL}); code != http.StatusCreated {
		t.Fatalf("create = %d\n%s", code, resp)
	}
	current := func() domain.ModelProfile {
		profiles, _ := h.store.ListModelProfiles(ctx, h.org.ID)
		for _, p := range profiles {
			if p.Alias == "worker.general" {
				return p
			}
		}
		t.Fatal("profile missing")
		return domain.ModelProfile{}
	}
	if got := current().Model; got != "claude-sonnet-5-5" {
		t.Fatalf("auto model = %q", got)
	}

	pin := h.projectPath("/models/profiles/worker.general/claude/pin")
	if code, _ := h.do(h.bobTok, http.MethodPut, pin, map[string]any{"model": "claude-sonnet-5"}); code != http.StatusForbidden {
		t.Fatalf("contributor pin = %d", code)
	}
	for _, bad := range []string{"", "two words"} {
		if code, _ := h.do(maintTok, http.MethodPut, pin, map[string]any{"model": bad}); code != http.StatusBadRequest {
			t.Fatalf("pin %q = %d", bad, code)
		}
	}
	if code, _ := h.do(maintTok, http.MethodPut, h.projectPath("/models/profiles/nope/claude/pin"), map[string]any{"model": "x"}); code != http.StatusNotFound {
		t.Fatalf("pin unknown profile = %d", code)
	}
	if code, resp := h.do(maintTok, http.MethodPut, pin, map[string]any{"model": "claude-sonnet-5"}); code != http.StatusOK {
		t.Fatalf("pin = %d\n%s", code, resp)
	}
	if p := current(); p.Model != "claude-sonnet-5" || !p.Pinned {
		t.Fatalf("after pin: %+v", p)
	}

	// A release and a bootstrap re-run both leave the person's choice alone.
	provider.set("claude-sonnet-5-5", "claude-sonnet-5-6")
	h.expireRefreshGap(t, h.endpointID(t, "anthropic"))
	h.do(maintTok, http.MethodPost, h.projectPath("/inference/endpoints/anthropic/refresh"), nil)
	declare.Model = "claude-sonnet-fallback"
	if _, err := h.store.UpsertModelProfile(ctx, declare); err != nil {
		t.Fatal(err)
	}
	if p := current(); p.Model != "claude-sonnet-5" || !p.Pinned {
		t.Fatalf("pin did not hold: %+v", p)
	}

	// Unpinning returns the profile to discovery immediately.
	code, resp := h.do(maintTok, http.MethodDelete, pin, nil)
	if code != http.StatusOK {
		t.Fatalf("unpin = %d\n%s", code, resp)
	}
	if p := current(); p.Pinned || p.Model != "claude-sonnet-5-6" {
		t.Fatalf("after unpin: %+v", p)
	}
}

func (h *harness) endpointID(t *testing.T, name string) domain.ID {
	t.Helper()
	ep, err := h.store.GetInferenceEndpoint(context.Background(), h.project.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return ep.ID
}
