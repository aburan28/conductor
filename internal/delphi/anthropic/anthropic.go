// Package anthropic runs delphi requests through the Anthropic Message Batches
// API with the official SDK. Credentials: ANTHROPIC_API_KEY,
// ANTHROPIC_AUTH_TOKEN, or complete workload federation environment variables.
// ANTHROPIC_BASE_URL is honoured.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/aburan28/conductor/internal/delphi"
)

// DefaultModel is Claude Mythos 5.1 (Project Glasswing); DELPHI_ANTHROPIC_MODEL overrides it.
const DefaultModel = "claude-mythos-5-1"

const maxOutputTokens = 128000

var efforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// Batch list prices per MTok (50% of interactive), input and output.
var prices = map[string][2]float64{
	"claude-mythos-5-1": {5, 25},
	"claude-fable-5-1":  {5, 25},
	"claude-opus-5-5":   {2, 10},
	"claude-sonnet-5-5": {1, 5},
	"claude-haiku-5-5":  {0.05, 0.25},
}

// Provider implements delphi.Provider.
type Provider struct {
	client anthropic.Client
	model  string
}

// New returns a provider; it fails when no credential is configured.
func New() (*Provider, error) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" && !federationConfigured() {
		return nil, errors.New("anthropic: export ANTHROPIC_API_KEY or configure workload identity federation")
	}
	return &Provider{client: anthropic.NewClient(option.WithMaxRetries(5)), model: model()}, nil
}

func federationConfigured() bool {
	return os.Getenv("ANTHROPIC_FEDERATION_RULE_ID") != "" &&
		os.Getenv("ANTHROPIC_ORGANIZATION_ID") != "" &&
		os.Getenv("ANTHROPIC_SERVICE_ACCOUNT_ID") != "" &&
		(os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE") != "" || os.Getenv("ANTHROPIC_IDENTITY_TOKEN") != "")
}

// NewWithKey returns a provider for an explicit key (a vault secret, not the environment).
// ANTHROPIC_BASE_URL is still honoured, for proxies and tests.
func NewWithKey(key string) *Provider {
	return &Provider{client: anthropic.NewClient(option.WithAPIKey(key), option.WithMaxRetries(5)), model: model()}
}

// Offline returns a provider that can validate but not call the API.
func Offline() *Provider { return &Provider{model: model()} }

func model() string {
	if m := os.Getenv("DELPHI_ANTHROPIC_MODEL"); m != "" {
		return m
	}
	return DefaultModel
}

func (p *Provider) Name() string         { return "anthropic" }
func (p *Provider) DefaultModel() string { return p.model }
func (p *Provider) Limits() delphi.Limits {
	// API limits: 100,000 requests and 256 MB per batch.
	return delphi.Limits{MaxRequests: 100000, MaxBytes: 200 << 20}
}

func (p *Provider) Price(model string) (float64, float64, bool) {
	pr, ok := prices[model]
	return pr[0], pr[1], ok
}

// strict models reject sampling parameters, non-adaptive thinking and forced
// tool choice with a 400; catch that before a batch is billed.
func strict(model string) bool {
	return strings.HasPrefix(model, "claude-mythos") || strings.HasPrefix(model, "claude-fable") ||
		model == "claude-opus-5-5" || model == "claude-sonnet-5-5"
}

// noSampling models accept only default sampling, so any sampling parameter is a 400.
func noSampling(model string) bool {
	return strings.HasPrefix(model, "claude-haiku-5-5")
}

func (p *Provider) Validate(r delphi.Request) []string {
	var out []string
	if r.MaxTokens > maxOutputTokens {
		out = append(out, fmt.Sprintf("max_tokens must be at most %d", maxOutputTokens))
	}
	if r.Effort != "" && !efforts[r.Effort] {
		out = append(out, fmt.Sprintf("effort %q is not one of low, medium, high, xhigh, max", r.Effort))
	}
	if strict(r.Model) || noSampling(r.Model) {
		for _, k := range []string{"temperature", "top_p", "top_k"} {
			if _, bad := r.Params[k]; bad {
				out = append(out, fmt.Sprintf("params.%s: sampling parameters are rejected by %s", k, r.Model))
			}
		}
	}
	if !strict(r.Model) {
		return out
	}
	if t, ok := r.Params["thinking"]; ok {
		if m, _ := t.(map[string]any); m == nil || m["type"] != "adaptive" {
			out = append(out, "params.thinking must be omitted or {\"type\":\"adaptive\"} (thinking is always on; disabled and budget_tokens are a 400)")
		}
	}
	if tc, ok := r.Params["tool_choice"].(map[string]any); ok {
		if t := tc["type"]; t == "any" || t == "tool" {
			out = append(out, fmt.Sprintf("params.tool_choice %q is a 400 on %s; use \"auto\" and name the tool in the prompt", t, r.Model))
		}
	}
	return out
}

func textBlocks(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

// Body is the Messages API request for r, as JSON-ready maps.
func Body(r delphi.Request) map[string]any {
	msgs := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		c := m.Content
		if s, ok := c.(string); ok {
			c = textBlocks(s)
		}
		msgs = append(msgs, map[string]any{"role": m.Role, "content": c})
	}
	body := map[string]any{"model": r.Model, "max_tokens": r.MaxTokens, "messages": msgs}
	if r.System != "" {
		body["system"] = textBlocks(r.System)
	}
	for k, v := range r.Params {
		body[k] = v
	}
	if r.Effort != "" {
		oc := map[string]any{}
		if m, ok := r.Params["output_config"].(map[string]any); ok {
			for k, v := range m {
				oc[k] = v
			}
		}
		oc["effort"] = r.Effort
		body["output_config"] = oc
	}
	return body
}

func (p *Provider) Submit(ctx context.Context, reqs []delphi.Request, label string) (string, error) {
	items := make([]anthropic.MessageBatchNewParamsRequest, 0, len(reqs))
	for _, r := range reqs {
		raw, err := json.Marshal(Body(r))
		if err != nil {
			return "", err
		}
		// The SDK param structs decode the same JSON the API takes, so tools,
		// metadata and other passthrough params survive.
		var params anthropic.MessageBatchNewParamsRequestParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return "", fmt.Errorf("request %q: %w", r.ID, err)
		}
		items = append(items, anthropic.MessageBatchNewParamsRequest{CustomID: r.ID, Params: params})
	}
	b, err := p.client.Messages.Batches.New(ctx, anthropic.MessageBatchNewParams{Requests: items})
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func (p *Provider) Poll(ctx context.Context, id string) (delphi.BatchStatus, error) {
	b, err := p.client.Messages.Batches.Get(ctx, id, anthropic.MessageBatchGetParams{})
	if err != nil {
		return delphi.BatchStatus{}, err
	}
	c := b.RequestCounts
	return delphi.BatchStatus{
		Ended: b.ProcessingStatus == anthropic.MessageBatchProcessingStatusEnded,
		State: string(b.ProcessingStatus),
		Counts: map[string]int64{"processing": c.Processing, "succeeded": c.Succeeded, "errored": c.Errored,
			"canceled": c.Canceled, "expired": c.Expired},
	}, nil
}

func (p *Provider) Results(ctx context.Context, id string, emit func(delphi.Result) error) error {
	stream := p.client.Messages.Batches.ResultsStreaming(ctx, id, anthropic.MessageBatchResultsParams{})
	defer stream.Close()
	for stream.Next() {
		if err := emit(toResult(stream.Current())); err != nil {
			return err
		}
	}
	return stream.Err()
}

func toResult(item anthropic.MessageBatchIndividualResponse) delphi.Result {
	r := delphi.Result{ID: item.CustomID, Provider: "anthropic"}
	switch v := item.Result.AsAny().(type) {
	case anthropic.MessageBatchSucceededResult:
		msg := v.Message
		var text strings.Builder
		for _, b := range msg.Content {
			if tb, ok := b.AsAny().(anthropic.TextBlock); ok {
				text.WriteString(tb.Text)
			}
		}
		r.Status, r.Model, r.Text, r.StopReason = delphi.Succeeded, string(msg.Model), text.String(), string(msg.StopReason)
		u := msg.Usage
		r.Usage = &delphi.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheWriteTokens: u.CacheCreationInputTokens, CacheReadTokens: u.CacheReadInputTokens}
		switch msg.StopReason {
		case anthropic.StopReasonRefusal:
			r.Status = delphi.Refused
			r.Refusal = &delphi.Refusal{Category: string(msg.StopDetails.Category), Explanation: msg.StopDetails.Explanation}
		case anthropic.StopReasonMaxTokens:
			r.Truncated = true
		}
	case anthropic.MessageBatchErroredResult:
		t := v.Error.Error.Type
		r.Status = delphi.Errored
		r.Error = &delphi.Error{Type: t, Message: v.Error.Error.Message, RequestID: v.Error.RequestID,
			Retryable: t != "invalid_request_error" && t != "authentication_error" && t != "permission_error" && t != "not_found_error"}
	case anthropic.MessageBatchCanceledResult:
		r.Status = delphi.Canceled
	case anthropic.MessageBatchExpiredResult:
		r.Status = delphi.Expired
	default:
		r.Status = delphi.Errored
		r.Error = &delphi.Error{Type: "unknown_result", Message: item.Result.RawJSON()}
	}
	return r
}

func (p *Provider) Cancel(ctx context.Context, id string) error {
	_, err := p.client.Messages.Batches.Cancel(ctx, id, anthropic.MessageBatchCancelParams{})
	return err
}

// Find is not possible: Message Batches carry no client metadata.
func (p *Provider) Find(context.Context, string) (string, bool, error) {
	return "", false, delphi.ErrNotSupported
}

// Describe returns actionable text for an Anthropic API error, if err is one.
func Describe(err error) (string, bool) {
	var e *anthropic.Error
	if !errors.As(err, &e) {
		return "", false
	}
	hint := map[int]string{
		401: "ANTHROPIC_API_KEY is missing, malformed or revoked.",
		403: "the key may lack access to this model (claude-mythos-5-1 is Project Glasswing only) or to the Batches API.",
		404: "model or batch not found; Mythos is hidden from keys outside Project Glasswing, and batch results expire after 29 days.",
		413: "batch exceeds 256 MB; split the input.",
		429: "rate limited; retry later or submit fewer requests.",
	}[e.StatusCode]
	if e.StatusCode == 400 {
		hint = "the API rejected the request body; fix the input."
		if strings.Contains(strings.ToLower(err.Error()), "data retention") {
			hint = "Mythos and Fable need 30-day data retention on the organization or workspace (no zero data retention)."
		}
	}
	return fmt.Sprintf("anthropic API error (HTTP %d): %v\n  hint: %s", e.StatusCode, err, hint), true
}
