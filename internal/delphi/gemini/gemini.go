// Package gemini is delphi's provider for the Gemini API's Batch Mode
// (https://ai.google.dev/api/batch-mode): requests go inline to
// models/{model}:batchGenerateContent at half the interactive price, and their answers come
// back inline on the finished batch, which Google keeps for six weeks.
//
// A Gemini batch serves one model, while delphi submits a provider's queued requests as one
// flight; Submit therefore creates one batch per model and joins their names with "|". Each
// batch carries the flight's label as its display name (with "#n" per model), so Find can
// recover the flight after a crash between creating the batches and recording them.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
)

// DefaultBaseURL is the Gemini API; GEMINI_BASE_URL overrides it (proxies, tests).
const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// Provider is the Gemini batch provider.
type Provider struct {
	key    string
	base   string
	model  string
	prices map[string][2]float64
	http   *http.Client
}

// New reads GEMINI_API_KEY (or GOOGLE_API_KEY) from the environment.
func New() (*Provider, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		key = os.Getenv("GOOGLE_API_KEY")
	}
	if key == "" {
		return nil, errors.New("gemini: export GEMINI_API_KEY")
	}
	return NewWithKey(key), nil
}

// NewWithKey returns a provider for an explicit key (a vault secret).
func NewWithKey(key string) *Provider {
	p := Offline()
	p.key, p.http = key, &http.Client{Timeout: 5 * time.Minute}
	return p
}

// Offline validates requests without a key.
func Offline() *Provider {
	base := os.Getenv("GEMINI_BASE_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	return &Provider{base: strings.TrimRight(base, "/"), model: os.Getenv("DELPHI_GEMINI_MODEL"), prices: prices()}
}

// prices come from DELPHI_GEMINI_PRICES, e.g. {"gemini-3.8-flash":[0.15,1.25]} (batch $ per
// MTok in, out). None are built in: a wrong built-in number is worse than none.
func prices() map[string][2]float64 {
	out := map[string][2]float64{}
	if s := os.Getenv("DELPHI_GEMINI_PRICES"); s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

func (p *Provider) Name() string         { return "gemini" }
func (p *Provider) DefaultModel() string { return p.model }

// Limits keeps an inline batch under the documented 20 MB.
func (p *Provider) Limits() delphi.Limits {
	return delphi.Limits{MaxRequests: 5000, MaxBytes: 18 << 20}
}

func (p *Provider) Price(model string) (float64, float64, bool) {
	pr, ok := p.prices[model]
	return pr[0], pr[1], ok
}

var efforts = map[string]bool{"minimal": true, "low": true, "medium": true, "high": true}

func (p *Provider) Validate(r delphi.Request) []string {
	var out []string
	if r.Effort != "" && !efforts[r.Effort] {
		out = append(out, fmt.Sprintf("effort %q: gemini takes minimal, low, medium or high (thinkingLevel)", r.Effort))
	}
	for _, k := range []string{"contents", "systemInstruction", "system_instruction"} {
		if _, bad := r.Params[k]; bad {
			out = append(out, "params."+k+" conflicts with the row's messages and system")
		}
	}
	for _, m := range r.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			out = append(out, fmt.Sprintf("message role %q: gemini takes user and assistant turns (system goes in system)", m.Role))
		}
	}
	return out
}

// Body is the GenerateContentRequest for r. Params are merged into generationConfig, except
// the request-level keys (tools, toolConfig, safetySettings, cachedContent) which go on top.
func Body(r delphi.Request) map[string]any {
	contents := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		role := m.Role
		if role == "assistant" {
			role = "model"
		}
		var parts any
		switch c := m.Content.(type) {
		case string:
			parts = []any{map[string]any{"text": c}}
		default:
			parts = c // provider-native parts
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	gen := map[string]any{}
	if r.MaxTokens > 0 {
		gen["maxOutputTokens"] = r.MaxTokens
	}
	if r.Effort != "" {
		gen["thinkingConfig"] = map[string]any{"thinkingLevel": r.Effort}
	}
	body := map[string]any{"contents": contents}
	for k, v := range r.Params {
		switch k {
		case "tools", "toolConfig", "safetySettings", "cachedContent", "labels":
			body[k] = v
		default:
			gen[k] = v
		}
	}
	if len(gen) > 0 {
		body["generationConfig"] = gen
	}
	if r.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": r.System}}}
	}
	return body
}

// HTTPError is a non-2xx answer from the API.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("gemini: HTTP %d: %s", e.StatusCode, e.Message)
}

// Describe renders a Gemini API error for people.
func Describe(err error) (string, bool) {
	var he *HTTPError
	if errors.As(err, &he) {
		return fmt.Sprintf("HTTP %d: %s", he.StatusCode, he.Message), true
	}
	return "", false
}

// StatusOf is a failed call's HTTP status, or 0.
func StatusOf(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.StatusCode
	}
	return 0
}

func (p *Provider) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+"/"+strings.TrimLeft(path, "/"), rd)
	if err != nil {
		return err
	}
	req.Header.Set("x-goog-api-key", p.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		return &HTTPError{StatusCode: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Submit creates one batch per model and returns their names joined with "|".
func (p *Provider) Submit(ctx context.Context, reqs []delphi.Request, label string) (string, error) {
	byModel := map[string][]delphi.Request{}
	var models []string
	for _, r := range reqs {
		if _, ok := byModel[r.Model]; !ok {
			models = append(models, r.Model)
		}
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	sort.Strings(models)
	var names []string
	for i, model := range models {
		inline := make([]any, 0, len(byModel[model]))
		for _, r := range byModel[model] {
			inline = append(inline, map[string]any{"request": Body(r), "metadata": map[string]any{"key": r.ID}})
		}
		display := label
		if len(models) > 1 {
			display = fmt.Sprintf("%s#%d", label, i+1)
		}
		body := map[string]any{"batch": map[string]any{"displayName": display,
			"inputConfig": map[string]any{"requests": map[string]any{"requests": inline}}}}
		var op struct {
			Name string `json:"name"`
		}
		if err := p.do(ctx, http.MethodPost, "models/"+url.PathEscape(model)+":batchGenerateContent", body, &op); err != nil {
			if len(names) > 0 {
				// Some batches exist: report them, so the flight is not resubmitted wholesale.
				// Not wrapped: a batch exists, so the outcome must not read as "nothing created".
				return strings.Join(names, "|"), fmt.Errorf("gemini: created %d of %d batches, then: %v", len(names), len(models), err)
			}
			return "", err
		}
		if op.Name == "" {
			return strings.Join(names, "|"), errors.New("gemini: batchGenerateContent returned no batch name")
		}
		names = append(names, op.Name)
	}
	return strings.Join(names, "|"), nil
}

// operation is batches.get: an Operation whose metadata (and response, once done) carry the
// batch. Field locations differ between the reference and the guide, so all are read.
type operation struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Metadata batch  `json:"metadata"`
	Response batch  `json:"response"`
	Error    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	State string `json:"state"`
}

type batch struct {
	State            string   `json:"state"`
	DisplayName      string   `json:"displayName"`
	Output           *output  `json:"output"`
	InlinedResponses *inlined `json:"inlinedResponses"`
}

type output struct {
	InlinedResponses *inlined `json:"inlinedResponses"`
}

type inlined struct {
	InlinedResponses []inlinedResponse `json:"inlinedResponses"`
}

type inlinedResponse struct {
	Metadata map[string]any  `json:"metadata"`
	Response json.RawMessage `json:"response"`
	Error    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (o operation) state() string {
	for _, s := range []string{o.Metadata.State, o.Response.State, o.State} {
		if s != "" {
			return strings.TrimPrefix(strings.TrimPrefix(s, "BATCH_STATE_"), "JOB_STATE_")
		}
	}
	return ""
}

func (o operation) responses() []inlinedResponse {
	for _, b := range []batch{o.Response, o.Metadata} {
		if b.Output != nil && b.Output.InlinedResponses != nil {
			return b.Output.InlinedResponses.InlinedResponses
		}
		if b.InlinedResponses != nil {
			return b.InlinedResponses.InlinedResponses
		}
	}
	return nil
}

func (p *Provider) get(ctx context.Context, name string) (operation, error) {
	var op operation
	err := p.do(ctx, http.MethodGet, name, nil, &op)
	return op, err
}

// Poll ends when every batch of the flight has.
func (p *Provider) Poll(ctx context.Context, id string) (delphi.BatchStatus, error) {
	st := delphi.BatchStatus{Ended: true, Counts: map[string]int64{}}
	var states, failures []string
	for _, name := range strings.Split(id, "|") {
		op, err := p.get(ctx, name)
		if err != nil {
			return st, err
		}
		s := op.state()
		states = append(states, s)
		st.Counts[strings.ToLower(s)]++
		ended := op.Done || s == "SUCCEEDED" || s == "FAILED" || s == "CANCELLED" || s == "EXPIRED"
		if !ended {
			st.Ended = false
		}
		if s == "FAILED" || (op.Error != nil && op.Error.Code != 1 && s != "SUCCEEDED") {
			msg := name + " failed"
			if op.Error != nil && op.Error.Message != "" {
				msg += ": " + op.Error.Message
			}
			failures = append(failures, msg)
		}
	}
	st.State = strings.ToLower(strings.Join(states, ","))
	if st.Ended && len(failures) == len(states) {
		st.Failure = strings.Join(failures, "; ")
	}
	return st, nil
}

// Results emits one Result per answered request, matched by the metadata key it was sent
// with. A cancelled or expired batch's unanswered requests are left out (the engine marks
// them missing and may resubmit them).
func (p *Provider) Results(ctx context.Context, id string, emit func(delphi.Result) error) error {
	for _, name := range strings.Split(id, "|") {
		op, err := p.get(ctx, name)
		if err != nil {
			return err
		}
		for i, ir := range op.responses() {
			key, _ := ir.Metadata["key"].(string)
			if key == "" {
				return fmt.Errorf("gemini: %s response %d carries no metadata key", name, i)
			}
			if err := emit(toResult(key, ir)); err != nil {
				return err
			}
		}
	}
	return nil
}

type genResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text    string `json:"text"`
				Thought bool   `json:"thought"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

// refusals are finish reasons that mean the model's safeguards declined.
var refusals = map[string]bool{"SAFETY": true, "RECITATION": true, "BLOCKLIST": true, "PROHIBITED_CONTENT": true,
	"SPII": true, "IMAGE_SAFETY": true, "LANGUAGE": false}

func toResult(key string, ir inlinedResponse) delphi.Result {
	res := delphi.Result{ID: key}
	if ir.Error != nil {
		// google.rpc codes: 4 deadline, 8 resource exhausted, 13 internal, 14 unavailable.
		retry := ir.Error.Code == 4 || ir.Error.Code == 8 || ir.Error.Code == 13 || ir.Error.Code == 14
		res.Status, res.Error = delphi.Errored, &delphi.Error{Type: fmt.Sprintf("rpc_%d", ir.Error.Code), Message: ir.Error.Message, Retryable: retry}
		return res
	}
	var g genResponse
	if err := json.Unmarshal(ir.Response, &g); err != nil {
		res.Status, res.Error = delphi.Errored, &delphi.Error{Type: "bad_response", Message: err.Error(), Retryable: true}
		return res
	}
	res.Model = g.ModelVersion
	if u := g.UsageMetadata; u != nil {
		// Thinking tokens are billed as output.
		res.Usage = &delphi.Usage{InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
			CacheReadTokens: u.CachedContentTokenCount}
	}
	if g.PromptFeedback != nil && g.PromptFeedback.BlockReason != "" {
		res.Status, res.Refusal = delphi.Refused, &delphi.Refusal{Category: strings.ToLower(g.PromptFeedback.BlockReason)}
		return res
	}
	if len(g.Candidates) == 0 {
		res.Status, res.Error = delphi.Errored, &delphi.Error{Type: "no_candidates", Message: "the response has no candidates", Retryable: true}
		return res
	}
	c := g.Candidates[0]
	var text strings.Builder
	for _, part := range c.Content.Parts {
		if !part.Thought {
			text.WriteString(part.Text)
		}
	}
	res.Status, res.Text, res.StopReason = delphi.Succeeded, text.String(), c.FinishReason
	switch {
	case refusals[c.FinishReason] && res.Text == "":
		res.Status, res.Refusal = delphi.Refused, &delphi.Refusal{Category: strings.ToLower(c.FinishReason)}
	case c.FinishReason == "MAX_TOKENS":
		res.Truncated = true
	}
	return res
}

// Cancel cancels every batch of the flight.
func (p *Provider) Cancel(ctx context.Context, id string) error {
	for _, name := range strings.Split(id, "|") {
		if err := p.do(ctx, http.MethodPost, name+":cancel", map[string]any{}, nil); err != nil {
			return err
		}
	}
	return nil
}

// Find returns the flight created with label: its batches, by display name.
func (p *Provider) Find(ctx context.Context, label string) (string, bool, error) {
	var names []string
	token := ""
	for page := 0; page < 20; page++ {
		path := "batches?pageSize=100"
		if token != "" {
			path += "&pageToken=" + url.QueryEscape(token)
		}
		var list struct {
			Operations    []operation `json:"operations"`
			Batches       []operation `json:"batches"`
			NextPageToken string      `json:"nextPageToken"`
		}
		if err := p.do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return "", false, err
		}
		for _, op := range append(list.Operations, list.Batches...) {
			d := op.Metadata.DisplayName
			if d == "" {
				d = op.Response.DisplayName
			}
			if d == label || strings.HasPrefix(d, label+"#") {
				names = append(names, op.Name)
			}
		}
		if token = list.NextPageToken; token == "" {
			break
		}
	}
	sort.Strings(names)
	return strings.Join(names, "|"), len(names) > 0, nil
}
