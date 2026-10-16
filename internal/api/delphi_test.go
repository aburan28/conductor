package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/delphi/hosted"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/secretbox"
	"github.com/aburan28/conductor/internal/secrets"
)

// fakeBatches is the Anthropic Message Batches API: it accepts batches, reports them ended,
// and answers each request with "echo: " + its text.
type fakeBatches struct {
	mu      sync.Mutex
	keys    []string
	batches map[string][]map[string]any
	refuse  map[string]bool // custom ids the model declines
	srv     *httptest.Server
}

func newFakeBatches(t *testing.T) *fakeBatches {
	t.Helper()
	f := &fakeBatches{batches: map[string][]map[string]any{}, refuse: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = append(f.keys, r.Header.Get("x-api-key"))
		w.Header().Set("content-type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 messages batches [id [results]]
		batch := func(id string) string {
			return fmt.Sprintf(`{"id":%q,"type":"message_batch","processing_status":"ended","request_counts":{"processing":0,"succeeded":1,"errored":0,"canceled":0,"expired":0},"created_at":"2026-10-15T00:00:00Z","expires_at":"2026-10-16T00:00:00Z","ended_at":null,"cancel_initiated_at":null,"archived_at":null,"results_url":"http://%s/v1/messages/batches/%s/results"}`, id, r.Host, id)
		}
		switch {
		case r.Method == http.MethodPost && len(parts) == 3:
			var body struct {
				Requests []map[string]any `json:"requests"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := fmt.Sprintf("msgbatch_%d", len(f.batches)+1)
			f.batches[id] = body.Requests
			fmt.Fprint(w, batch(id))
		case r.Method == http.MethodGet && len(parts) == 4:
			fmt.Fprint(w, batch(parts[3]))
		case r.Method == http.MethodGet && len(parts) == 5 && parts[4] == "results":
			w.Header().Set("content-type", "application/binary")
			for _, req := range f.batches[parts[3]] {
				if f.refuse[req["custom_id"].(string)] {
					fmt.Fprintf(w, `{"custom_id":%q,"result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-mythos-5-1","content":[],"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},"usage":{"input_tokens":100,"output_tokens":0}}}}`+"\n", req["custom_id"])
					continue
				}
				msg := req["params"].(map[string]any)["messages"].([]any)[0].(map[string]any)
				text := msg["content"].([]any)[0].(map[string]any)["text"].(string)
				fmt.Fprintf(w, `{"custom_id":%q,"result":{"type":"succeeded","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-mythos-5-1","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":200}}}}`+"\n",
					req["custom_id"], "echo: "+text)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (h *harness) withDelphi(t *testing.T) *hosted.Service {
	t.Helper()
	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v := secrets.New(h.store, &secretbox.Source{Env: raw})
	svc := &hosted.Service{Store: h.store, Key: func(ctx context.Context, p domain.Project, name string) (string, error) {
		o, err := v.Open(ctx, p, name)
		return o.Value, err
	}}
	h.server.Close()
	h.server = httptest.NewServer(New(h.store, coord.New(h.store), Options{
		Secrets: v, Delphi: svc, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler())
	t.Cleanup(h.server.Close)
	t.Cleanup(func() {
		pool := h.store.Pool()
		_, _ = pool.Exec(context.Background(), `DELETE FROM delphi_delegations WHERE project_id = $1::uuid`, h.project.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM project_secrets WHERE project_id = $1::uuid`, h.project.ID)
	})
	return svc
}

func TestDelphiNotConfigured(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do(h.bobTok, http.MethodGet, h.projectPath("/delphi"), nil); code != http.StatusServiceUnavailable {
		t.Fatalf("delphi without a service = %d", code)
	}
}

func TestDelphiEndToEnd(t *testing.T) {
	h := newHarness(t)
	svc := h.withDelphi(t)
	fake := newFakeBatches(t)
	t.Setenv("ANTHROPIC_BASE_URL", fake.srv.URL)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	ask := map[string]any{"ref": "q1", "text": "the secret plan is X", "effort": "high"}

	// No key in the vault: refused with the command that fixes it.
	code, resp := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"), ask)
	if code != http.StatusBadRequest || !strings.Contains(string(resp), "conductor secrets set ANTHROPIC_API_KEY") {
		t.Fatalf("no key = %d %s", code, resp)
	}
	if code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/secrets"),
		map[string]any{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-vault"}); code != http.StatusCreated {
		t.Fatalf("seed secret = %d %s", code, resp)
	}

	code, resp = h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"), ask)
	if code != http.StatusCreated {
		t.Fatalf("submit = %d %s", code, resp)
	}
	var d domain.DelphiDelegation
	_ = json.Unmarshal(resp, &d)
	if d.State != domain.DelphiRunning || d.Model != "claude-mythos-5-1" || d.BatchID != "msgbatch_1" {
		t.Fatalf("submitted: %+v", d)
	}
	if fake.keys[0] != "sk-ant-vault" {
		t.Fatalf("the vault key did not reach the provider: %q", fake.keys[0])
	}
	sent := fake.batches["msgbatch_1"][0]["params"].(map[string]any)
	if sent["output_config"].(map[string]any)["effort"] != "high" {
		t.Fatalf("provider body: %v", sent)
	}
	// Retrying the same POST is a no-op (200, no second batch).
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"), ask); code != http.StatusOK || len(fake.batches) != 1 {
		t.Fatalf("retry = %d, batches %d", code, len(fake.batches))
	}

	// The request text is nowhere in the database row.
	var row string
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT row_to_json(d)::text FROM delphi_delegations d WHERE project_id = $1::uuid AND ref = 'q1'`,
		h.project.ID).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row, "secret plan") {
		t.Fatalf("the request text was stored: %s", row)
	}

	if code, _ := h.do(h.bobTok, http.MethodGet, h.projectPath("/delphi/q1/answer"), nil); code != http.StatusConflict {
		t.Fatalf("answer before the loop ran = %d", code)
	}
	if err := svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/delphi/q1/answer"), nil)
	if code != http.StatusOK {
		t.Fatalf("answer = %d %s", code, resp)
	}
	var a hosted.Answer
	_ = json.Unmarshal(resp, &a)
	if a.Text != "echo: the secret plan is X" || a.Delegation.Status != "succeeded" || a.Delegation.OutputTokens != 200 {
		t.Fatalf("answer: %+v", a)
	}
	// Still not stored after the answer was fetched.
	if err := h.store.Pool().QueryRow(context.Background(),
		`SELECT row_to_json(d)::text FROM delphi_delegations d WHERE project_id = $1::uuid AND ref = 'q1'`,
		h.project.ID).Scan(&row); err != nil || strings.Contains(row, "echo:") {
		t.Fatalf("the answer was stored: %v %s", err, row)
	}

	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/delphi?state=finished"), nil)
	if code != http.StatusOK || !strings.Contains(string(resp), `"ref":"q1"`) {
		t.Fatalf("list = %d %s", code, resp)
	}
	// Reconcile is for maintainers.
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi/q1/reconcile"), map[string]any{"abandon": true}); code != http.StatusForbidden {
		t.Fatalf("contributor reconcile = %d", code)
	}
	if code, _ := h.do(maintTok, http.MethodPost, h.projectPath("/delphi/q1/reconcile"), map[string]any{"abandon": true}); code != http.StatusConflict {
		t.Fatalf("reconcile of a finished delegation = %d", code)
	}
}

func TestDelphiProfilesAndFallback(t *testing.T) {
	h := newHarness(t)
	svc := h.withDelphi(t)
	fake := newFakeBatches(t)
	t.Setenv("ANTHROPIC_BASE_URL", fake.srv.URL)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	if code, resp := h.do(maintTok, http.MethodPost, h.projectPath("/secrets"),
		map[string]any{"name": "ANTHROPIC_API_KEY", "value": "sk-ant-vault"}); code != http.StatusCreated {
		t.Fatalf("seed secret = %d %s", code, resp)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = h.store.Pool().Exec(context.Background(), `DELETE FROM model_profiles WHERE organization_id = $1::uuid AND alias LIKE 'dt-%'`, h.project.OrganizationID)
	})
	for _, p := range []domain.ModelProfile{
		{Alias: "dt-implementer", Harness: "claude", Provider: "anthropic", Model: "claude-sonnet-5", ReasoningEffort: "medium", Billing: "tokens", Tier: "T2", Enabled: true},
		{Alias: "dt-implementer", Harness: "delphi", Provider: "anthropic", Model: "claude-haiku-5-5", ReasoningEffort: "max", Billing: "tokens", Tier: "T2", Enabled: true},
		{Alias: "dt-local", Harness: "opencode", Provider: "vllm", Model: "vllm/x", ReasoningEffort: "low", Billing: "tokens", Tier: "T2", Enabled: true},
		{Alias: "dt-off", Harness: "delphi", Provider: "anthropic", Model: "claude-opus-5-5", ReasoningEffort: "high", Billing: "tokens", Tier: "T2", Enabled: false},
	} {
		p.OrganizationID = h.project.OrganizationID
		if _, err := h.store.UpsertModelProfile(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	code, resp := h.do(h.bobTok, http.MethodGet, h.projectPath("/delphi/profiles"), nil)
	if code != http.StatusOK {
		t.Fatalf("profiles = %d %s", code, resp)
	}
	var list struct{ Profiles []hosted.Profile }
	_ = json.Unmarshal(resp, &list)
	got := map[string]hosted.Profile{}
	for _, p := range list.Profiles {
		got[p.Name] = p
	}
	if p := got["dt-implementer"]; p.Model != "claude-haiku-5-5" || p.Effort != "max" || p.Harness != "delphi" {
		t.Fatalf("the delphi profile should win: %+v", list.Profiles)
	}
	if _, ok := got["dt-local"]; ok {
		t.Fatalf("a vllm profile is not a delphi tier: %+v", list.Profiles)
	}
	if _, ok := got["dt-off"]; ok {
		t.Fatalf("a disabled profile is listed: %+v", list.Profiles)
	}

	if code, resp := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"),
		map[string]any{"ref": "x", "text": "q", "profile": "dt-off"}); code != http.StatusBadRequest || !strings.Contains(string(resp), "dt-off") {
		t.Fatalf("disabled profile = %d %s", code, resp)
	}
	fake.refuse["q2"] = true
	code, resp = h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"), map[string]any{"ref": "q2", "text": "probe it", "profile": "dt-implementer"})
	if code != http.StatusCreated {
		t.Fatalf("submit by profile = %d %s", code, resp)
	}
	var d domain.DelphiDelegation
	_ = json.Unmarshal(resp, &d)
	sent := fake.batches[d.BatchID][0]["params"].(map[string]any)
	if d.Model != "claude-haiku-5-5" || d.Profile != "dt-implementer" || sent["output_config"].(map[string]any)["effort"] != "max" {
		t.Fatalf("profile submit: %+v %v", d, sent)
	}
	// An explicit model overrides the profile's.
	code, resp = h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"),
		map[string]any{"ref": "q3", "text": "other", "profile": "dt-implementer", "model": "claude-opus-5-5"})
	if _ = json.Unmarshal(resp, &d); code != http.StatusCreated || d.Model != "claude-opus-5-5" {
		t.Fatalf("model over profile = %d %+v", code, d)
	}
	// fallback_of must name a refused delegation.
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"),
		map[string]any{"ref": "q2-fb1", "text": "probe it", "fallback_of": "q2"}); code != http.StatusBadRequest {
		t.Fatalf("fallback of a running delegation = %d", code)
	}
	if err := svc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	code, resp = h.do(h.bobTok, http.MethodGet, h.projectPath("/delphi/q2"), nil)
	if _ = json.Unmarshal(resp, &d); code != http.StatusOK || d.Status != "refused" || d.RefusalCategory != "cyber" {
		t.Fatalf("refused = %d %+v", code, d)
	}
	code, resp = h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"),
		map[string]any{"ref": "q2-fb1", "text": "probe it", "model": "claude-opus-5-5", "fallback_of": "q2"})
	if _ = json.Unmarshal(resp, &d); code != http.StatusCreated || d.FallbackOf != "q2" {
		t.Fatalf("fallback = %d %s", code, resp)
	}
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"),
		map[string]any{"ref": "q4", "text": "x", "fallback_of": "q3"}); code != http.StatusBadRequest {
		t.Fatalf("fallback of a succeeded delegation = %d", code)
	}
	if code, _ := h.do(h.bobTok, http.MethodPost, h.projectPath("/delphi"),
		map[string]any{"ref": "profiles", "text": "x"}); code != http.StatusBadRequest {
		t.Fatalf("reserved ref = %d", code)
	}
}
