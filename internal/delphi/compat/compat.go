// Package compat runs delphi requests live against OpenAI-compatible Chat Completions
// endpoints (Fireworks, DeepSeek, Together, OpenRouter, a vLLM or LM Studio server), most of
// which have no batch API. Submit makes the calls itself, a few at a time, and writes every
// answer to a spool file named after the flight's label before it returns; Poll, Results and
// Find read that file. So the engine's crash rule still holds: a flight whose spool file
// exists is complete, and one whose file does not exist left no provider-side state, so
// `delphi reconcile -requeue` can safely run it again (a live call costs tokens, nothing else).
//
// Endpoints come from Conductor's presets (internal/modelsrc) and from an endpoints file
// ($DELPHI_ENDPOINTS, else ~/.delphi/endpoints.json):
//
//	{"deepseek": {"base_url": "https://api.deepseek.com/v1", "key_env": "DEEPSEEK_API_KEY",
//	              "default_model": "deepseek-chat", "concurrency": 8,
//	              "prices": {"deepseek-chat": [0.27, 1.1]}}}
//
// A remote endpoint is live when its key variable is set; a local one (vLLM, LM Studio) only
// when the file names it. Prices are live $ per MTok in, out; none are built in, because a
// wrong built-in number is worse than none.
package compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/modelsrc"
)

// Endpoint is one OpenAI-compatible server.
type Endpoint struct {
	Name         string                `json:"-"`
	BaseURL      string                `json:"base_url"`
	KeyEnv       string                `json:"key_env,omitempty"`
	DefaultModel string                `json:"default_model,omitempty"`
	Concurrency  int                   `json:"concurrency,omitempty"`
	MaxRequests  int                   `json:"max_requests,omitempty"`
	Prices       map[string][2]float64 `json:"prices,omitempty"`
	// Free marks every model on a third-party endpoint as costing nothing; FreeModels marks
	// some. FreeSuffix marks models by id suffix (OpenRouter's ":free"). Free mode
	// ($DELPHI_FREE) sends only to free models; third-party ones may train on prompts.
	Free       bool     `json:"free,omitempty"`
	FreeModels []string `json:"free_models,omitempty"`
	FreeSuffix string   `json:"-"`
	// Local endpoints need no key and are yours, so they are free and private; Listed ones
	// were named by the endpoints file.
	Local  bool `json:"-"`
	Listed bool `json:"-"`
}

// freeSuffix is the id suffix a preset gives its no-cost models.
var freeSuffix = map[string]string{"openrouter": ":free"}

// reserved names belong to the native batch providers.
var reserved = map[string]bool{"anthropic": true, "openai": true}

// Endpoints merges the OpenAI-compatible presets with the endpoints file at path (an absent
// file is fine); a file entry fills unset fields from the preset of the same name.
func Endpoints(path string) (map[string]Endpoint, error) {
	out := map[string]Endpoint{}
	for _, p := range modelsrc.Presets {
		if p.Kind == domain.InferenceOpenAICompatible && !reserved[p.Name] {
			out[p.Name] = Endpoint{Name: p.Name, BaseURL: p.BaseURL, KeyEnv: p.KeyHint, Local: p.Local, FreeSuffix: freeSuffix[p.Name]}
		}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	var file map[string]Endpoint
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, e := range file {
		if reserved[name] {
			return nil, fmt.Errorf("%s: %q is a native batch provider, not an OpenAI-compatible endpoint", path, name)
		}
		preset := out[name]
		if e.BaseURL == "" {
			e.BaseURL = preset.BaseURL
		}
		if e.KeyEnv == "" {
			e.KeyEnv = preset.KeyEnv
		}
		if e.BaseURL == "" {
			return nil, fmt.Errorf("%s: endpoint %q needs base_url", path, name)
		}
		if err := modelsrc.ValidateBaseURL(e.BaseURL); err != nil {
			return nil, fmt.Errorf("%s: endpoint %q: %w", path, name, err)
		}
		e.Name, e.Listed, e.Local, e.FreeSuffix = name, true, preset.Local || e.KeyEnv == "", preset.FreeSuffix
		out[name] = e
	}
	return out, nil
}

// Provider implements delphi.Provider for one endpoint.
type Provider struct {
	ep    Endpoint
	key   string
	spool string
	http  *http.Client
}

// New returns the provider for e, or an error saying what would make it live.
func New(e Endpoint, spoolDir string) (*Provider, error) {
	key := ""
	switch {
	case e.KeyEnv != "":
		if key = os.Getenv(e.KeyEnv); key == "" {
			return nil, fmt.Errorf("%s: export %s", e.Name, e.KeyEnv)
		}
	case !e.Listed:
		return nil, fmt.Errorf("%s: local endpoint; name it in the endpoints file to use it", e.Name)
	}
	return &Provider{ep: e, key: key, spool: filepath.Join(spoolDir, e.Name), http: &http.Client{Timeout: timeout()}}, nil
}

// Offline returns a provider that can validate but not call the endpoint.
func Offline(e Endpoint) *Provider { return &Provider{ep: e} }

func timeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("DELPHI_COMPAT_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

func (p *Provider) Name() string         { return p.ep.Name }
func (p *Provider) DefaultModel() string { return p.ep.DefaultModel }

// Limits keeps one flight to what Submit can answer live in reasonable time.
func (p *Provider) Limits() delphi.Limits {
	n := p.ep.MaxRequests
	if n <= 0 {
		n = 64
	}
	return delphi.Limits{MaxRequests: n, MaxBytes: 32 << 20}
}

func (p *Provider) Price(model string) (float64, float64, bool) {
	if free, _ := p.Free(model); free {
		return 0, 0, true
	}
	pr, ok := p.ep.Prices[model]
	return pr[0], pr[1], ok
}

// Free implements delphi.FreeKnower: an endpoint you run (local) costs nothing; on a
// third-party endpoint only models it marks free do.
func (p *Provider) Free(model string) (free, local bool) {
	if p.ep.Local {
		return true, true
	}
	if p.ep.Free || (p.ep.FreeSuffix != "" && strings.HasSuffix(model, p.ep.FreeSuffix)) {
		return true, false
	}
	for _, m := range p.ep.FreeModels {
		if m == model {
			return true, false
		}
	}
	return false, false
}

func (p *Provider) Validate(r delphi.Request) []string {
	var out []string
	if _, bad := r.Params["reasoning_effort"]; bad && r.Effort != "" {
		out = append(out, "params.reasoning_effort conflicts with the row-level effort key")
	}
	if _, bad := r.Params["max_tokens"]; bad {
		out = append(out, "params.max_tokens conflicts with the row-level max_tokens key")
	}
	if r.Model == "" {
		out = append(out, p.ep.Name+" has no default model: name one, or set default_model in the endpoints file")
	}
	return out
}

// Body is the Chat Completions request for r.
func Body(r delphi.Request) map[string]any {
	msgs := make([]any, 0, len(r.Messages)+1)
	if r.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": r.System})
	}
	for _, m := range r.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": r.Model, "messages": msgs, "max_tokens": r.MaxTokens}
	if r.Effort != "" {
		body["reasoning_effort"] = r.Effort
	}
	for k, v := range r.Params {
		body[k] = v
	}
	return body
}

func (p *Provider) path(label string) string { return filepath.Join(p.spool, label+".jsonl") }

// Submit answers every request live and spools the results; the batch id is the label.
func (p *Provider) Submit(ctx context.Context, reqs []delphi.Request, label string) (string, error) {
	if err := os.MkdirAll(p.spool, 0o700); err != nil {
		return "", err
	}
	n := p.ep.Concurrency
	if n <= 0 {
		n = 4
	}
	results := make([]delphi.Result, len(reqs))
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i, r := range reqs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			results[i] = p.call(ctx, r)
		}()
	}
	wg.Wait()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return "", err
		}
	}
	// Write-then-rename: the spool file exists only once every answer is in it.
	tmp := p.path(label) + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return "", err
	}
	return label, os.Rename(tmp, p.path(label))
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content any    `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// call answers one request; every failure becomes the request's result, never Submit's error.
func (p *Provider) call(ctx context.Context, r delphi.Request) delphi.Result {
	res := delphi.Result{ID: r.ID, Model: r.Model}
	fail := func(typ, msg string, retryable bool) delphi.Result {
		res.Status, res.Error = delphi.Errored, &delphi.Error{Type: typ, Message: msg, Retryable: retryable}
		return res
	}
	body, err := json.Marshal(Body(r))
	if err != nil {
		return fail("invalid_request", err.Error(), false)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.ep.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fail("invalid_request", err.Error(), false)
	}
	hr.Header.Set("Content-Type", "application/json")
	if p.key != "" {
		hr.Header.Set("Authorization", "Bearer "+p.key)
	}
	resp, err := p.http.Do(hr)
	if err != nil {
		return fail("network", err.Error(), true)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fail("network", err.Error(), true)
	}
	var cr chatResponse
	jerr := json.Unmarshal(raw, &cr)
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if jerr == nil && cr.Error != nil && cr.Error.Message != "" {
			msg = cr.Error.Message
		}
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		s := resp.StatusCode
		return fail(fmt.Sprintf("http_%d", s), msg, s == 408 || s == 409 || s == 429 || s >= 500)
	}
	if jerr != nil || len(cr.Choices) == 0 {
		return fail("bad_response", fmt.Sprintf("no choices in the answer (%v)", jerr), true)
	}
	c := cr.Choices[0]
	if cr.Model != "" {
		res.Model = cr.Model
	}
	res.Status, res.Text, res.StopReason = delphi.Succeeded, text(c.Message.Content), c.FinishReason
	if u := cr.Usage; u != nil {
		res.Usage = &delphi.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
		if u.PromptTokensDetails != nil {
			res.Usage.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		}
	}
	switch {
	case c.Message.Refusal != "" && res.Text == "":
		res.Status, res.Refusal = delphi.Refused, &delphi.Refusal{Category: "refusal", Explanation: c.Message.Refusal}
	case c.FinishReason == "content_filter" && res.Text == "":
		res.Status, res.Refusal = delphi.Refused, &delphi.Refusal{Category: "content_filter"}
	case c.FinishReason == "length":
		res.Truncated = true
	}
	return res
}

// text reads content that is a string or a list of {type: text, text: ...} parts.
func text(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if m, ok := part.(map[string]any); ok && m["type"] == "text" {
				s, _ := m["text"].(string)
				b.WriteString(s)
			}
		}
		return b.String()
	}
	return ""
}

func (p *Provider) Poll(_ context.Context, id string) (delphi.BatchStatus, error) {
	counts := map[string]int64{}
	if err := p.Results(context.Background(), id, func(r delphi.Result) error {
		counts[string(r.Status)]++
		return nil
	}); err != nil {
		return delphi.BatchStatus{}, err
	}
	return delphi.BatchStatus{Ended: true, State: "completed", Counts: counts}, nil
}

func (p *Provider) Results(_ context.Context, id string, emit func(delphi.Result) error) error {
	f, err := os.Open(p.path(id))
	if err != nil {
		return fmt.Errorf("%s: no spooled answers for %s: %w", p.ep.Name, id, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 128<<20)
	for sc.Scan() {
		var r delphi.Result
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return err
		}
		if err := emit(r); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Cancel has nothing to stop: Submit returns only once every request is answered.
func (p *Provider) Cancel(context.Context, string) error { return delphi.ErrNotSupported }

// Find reports a flight complete when its spool file exists. No file means the calls never
// finished and left nothing at the provider, so the flight can be requeued.
func (p *Provider) Find(_ context.Context, label string) (string, bool, error) {
	if _, err := os.Stat(p.path(label)); err == nil {
		return label, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	return "", false, nil
}
