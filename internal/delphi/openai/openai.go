// Package openai runs delphi requests through the OpenAI Batch API against
// the Responses endpoint, with the official SDK: the requests are uploaded as
// a JSONL file, a batch is created over it, and the output and error files are
// read back when it ends. Credentials: OPENAI_API_KEY (plus OPENAI_ORG_ID,
// OPENAI_PROJECT_ID, OPENAI_BASE_URL as the SDK reads them).
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/aburan28/conductor/internal/delphi"
)

// LabelKey is the batch metadata key holding delphi's submission label.
const LabelKey = "delphi_label"

// Provider implements delphi.Provider.
type Provider struct {
	client openai.Client
	model  string
	prices map[string][2]float64
}

// New returns a provider; it fails when OPENAI_API_KEY is not set.
func New() (*Provider, error) {
	if os.Getenv("OPENAI_API_KEY") == "" {
		return nil, errors.New("openai: export OPENAI_API_KEY")
	}
	return &Provider{client: openai.NewClient(option.WithMaxRetries(5)), model: os.Getenv("DELPHI_OPENAI_MODEL"), prices: prices()}, nil
}

// Offline returns a provider that can validate but not call the API.
func Offline() *Provider {
	return &Provider{model: os.Getenv("DELPHI_OPENAI_MODEL"), prices: prices()}
}

// prices come from DELPHI_OPENAI_PRICES, e.g. {"model-id":[1.25,5]} (batch $
// per MTok in, out). No prices are built in: OpenAI's model line-up and batch
// rates change, and a wrong built-in number is worse than none.
func prices() map[string][2]float64 {
	out := map[string][2]float64{}
	if s := os.Getenv("DELPHI_OPENAI_PRICES"); s != "" {
		_ = json.Unmarshal([]byte(s), &out)
	}
	return out
}

func (p *Provider) Name() string { return "openai" }

// DefaultModel is DELPHI_OPENAI_MODEL; there is no built-in default.
func (p *Provider) DefaultModel() string { return p.model }

func (p *Provider) Limits() delphi.Limits {
	// API limits: 50,000 requests and a 200 MB input file per batch.
	return delphi.Limits{MaxRequests: 50000, MaxBytes: 180 << 20}
}

func (p *Provider) Price(model string) (float64, float64, bool) {
	pr, ok := p.prices[model]
	return pr[0], pr[1], ok
}

func (p *Provider) Validate(r delphi.Request) []string {
	var out []string
	for _, k := range []string{"reasoning", "max_completion_tokens"} {
		if _, bad := r.Params[k]; bad && (k != "reasoning" || r.Effort != "") {
			out = append(out, fmt.Sprintf("params.%s conflicts with the row-level effort/max_tokens keys", k))
		}
	}
	return out
}

// Body is the Responses API request for r.
func Body(r delphi.Request) map[string]any {
	input := make([]any, 0, len(r.Messages))
	for _, m := range r.Messages {
		input = append(input, map[string]any{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{"model": r.Model, "input": input, "max_output_tokens": r.MaxTokens}
	if r.System != "" {
		body["instructions"] = r.System
	}
	for k, v := range r.Params {
		body[k] = v
	}
	if r.Effort != "" {
		body["reasoning"] = map[string]any{"effort": r.Effort}
	}
	return body
}

// line is one row of the batch input file.
type line struct {
	CustomID string         `json:"custom_id"`
	Method   string         `json:"method"`
	URL      string         `json:"url"`
	Body     map[string]any `json:"body"`
}

func (p *Provider) Submit(ctx context.Context, reqs []delphi.Request, label string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range reqs {
		if err := enc.Encode(line{CustomID: r.ID, Method: "POST", URL: "/v1/responses", Body: Body(r)}); err != nil {
			return "", fmt.Errorf("request %q: %w", r.ID, err)
		}
	}
	f, err := p.client.Files.New(ctx, openai.FileNewParams{
		File:    openai.File(&buf, label+".jsonl", "application/jsonl"),
		Purpose: openai.FilePurposeBatch,
	})
	if err != nil {
		return "", fmt.Errorf("uploading batch input: %w", err)
	}
	b, err := p.client.Batches.New(ctx, openai.BatchNewParams{
		InputFileID:      f.ID,
		Endpoint:         openai.BatchNewParamsEndpointV1Responses,
		CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
		Metadata:         map[string]string{LabelKey: label},
	})
	if err != nil {
		return "", err
	}
	return b.ID, nil
}

func ended(s openai.BatchStatus) bool {
	switch s {
	case openai.BatchStatusCompleted, openai.BatchStatusFailed, openai.BatchStatusExpired, openai.BatchStatusCancelled:
		return true
	}
	return false
}

func (p *Provider) Poll(ctx context.Context, id string) (delphi.BatchStatus, error) {
	b, err := p.client.Batches.Get(ctx, id)
	if err != nil {
		return delphi.BatchStatus{}, err
	}
	st := delphi.BatchStatus{Ended: ended(b.Status), State: string(b.Status),
		Counts: map[string]int64{"total": b.RequestCounts.Total, "completed": b.RequestCounts.Completed, "failed": b.RequestCounts.Failed}}
	if b.Status == openai.BatchStatusFailed {
		var msgs []string
		for _, e := range b.Errors.Data {
			m := e.Message
			if e.Line > 0 {
				m = fmt.Sprintf("line %d: %s", e.Line, m)
			}
			msgs = append(msgs, m)
		}
		st.Failure = "batch rejected: " + strings.Join(msgs, "; ")
		if len(msgs) == 0 {
			st.Failure = "batch rejected by the provider"
		}
	}
	return st, nil
}

func (p *Provider) Results(ctx context.Context, id string, emit func(delphi.Result) error) error {
	b, err := p.client.Batches.Get(ctx, id)
	if err != nil {
		return err
	}
	for _, fid := range []string{b.OutputFileID, b.ErrorFileID} {
		if fid == "" {
			continue
		}
		if err := p.readFile(ctx, fid, emit); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) readFile(ctx context.Context, fileID string, emit func(delphi.Result) error) error {
	resp, err := p.client.Files.Content(ctx, fileID)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", fileID, err)
	}
	defer resp.Body.Close()
	return ParseOutput(resp.Body, emit)
}

// outputLine is one row of a batch output or error file.
type outputLine struct {
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		RequestID  string          `json:"request_id"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// responseBody holds the Responses API fields delphi reads.
type responseBody struct {
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens        int64 `json:"input_tokens"`
		OutputTokens       int64 `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseOutput converts batch output/error file lines into results.
func ParseOutput(rd io.Reader, emit func(delphi.Result) error) error {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var l outputLine
		if err := json.Unmarshal(raw, &l); err != nil {
			return fmt.Errorf("batch output: %w", err)
		}
		if err := emit(toResult(l)); err != nil {
			return err
		}
	}
	return sc.Err()
}

func toResult(l outputLine) delphi.Result {
	r := delphi.Result{ID: l.CustomID, Provider: "openai"}
	if l.Error != nil && (l.Response == nil || l.Response.StatusCode == 0) {
		switch l.Error.Code {
		case "batch_expired":
			r.Status = delphi.Expired
		case "batch_cancelled":
			r.Status = delphi.Canceled
		default:
			r.Status = delphi.Errored
			r.Error = &delphi.Error{Type: l.Error.Code, Message: l.Error.Message, Retryable: true}
		}
		return r
	}
	if l.Response == nil {
		r.Status = delphi.Errored
		r.Error = &delphi.Error{Type: "malformed_output", Message: "no response or error in batch output line", Retryable: true}
		return r
	}
	var body responseBody
	_ = json.Unmarshal(l.Response.Body, &body)
	if code := l.Response.StatusCode; code != 200 {
		r.Status = delphi.Errored
		e := &delphi.Error{Type: fmt.Sprintf("http_%d", code), RequestID: l.Response.RequestID, Retryable: code == 429 || code >= 500}
		if body.Error != nil {
			e.Type, e.Message = firstNonEmpty(body.Error.Type, body.Error.Code, e.Type), body.Error.Message
		}
		r.Error = e
		return r
	}
	r.Model = body.Model
	r.StopReason = body.Status
	r.Usage = &delphi.Usage{InputTokens: body.Usage.InputTokens, OutputTokens: body.Usage.OutputTokens,
		CacheReadTokens: body.Usage.InputTokensDetails.CachedTokens}
	var text, refusal strings.Builder
	for _, item := range body.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			switch c.Type {
			case "output_text":
				text.WriteString(c.Text)
			case "refusal":
				refusal.WriteString(c.Refusal)
			}
		}
	}
	r.Text = text.String()
	switch {
	case body.Status == "failed":
		r.Status = delphi.Errored
		r.Error = &delphi.Error{Type: "response_failed", RequestID: l.Response.RequestID, Retryable: true}
		if body.Error != nil {
			r.Error.Type, r.Error.Message = firstNonEmpty(body.Error.Code, body.Error.Type, "response_failed"), body.Error.Message
		}
	case refusal.Len() > 0 && r.Text == "":
		r.Status = delphi.Refused
		r.Refusal = &delphi.Refusal{Explanation: refusal.String()}
	default:
		r.Status = delphi.Succeeded
		if body.Status == "incomplete" && body.IncompleteDetails != nil && body.IncompleteDetails.Reason == "max_output_tokens" {
			r.Truncated = true
		}
	}
	return r
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func (p *Provider) Cancel(ctx context.Context, id string) error {
	_, err := p.client.Batches.Cancel(ctx, id)
	return err
}

// Find looks for the batch submitted with label among recent batches.
func (p *Provider) Find(ctx context.Context, label string) (string, bool, error) {
	it := p.client.Batches.ListAutoPaging(ctx, openai.BatchListParams{Limit: openai.Int(100)})
	for n := 0; it.Next() && n < 1000; n++ {
		if b := it.Current(); b.Metadata[LabelKey] == label {
			return b.ID, true, nil
		}
	}
	return "", false, it.Err()
}

// Describe returns actionable text for an OpenAI API error, if err is one.
func Describe(err error) (string, bool) {
	var e *openai.Error
	if !errors.As(err, &e) {
		return "", false
	}
	hint := map[int]string{
		400: "the API rejected the request; check the model id and the request fields.",
		401: "OPENAI_API_KEY is missing, malformed or revoked.",
		403: "the key or project lacks access to this model or to the Batch API.",
		404: "model, file or batch not found.",
		429: "rate or batch-queue limit reached; retry later or submit fewer requests.",
	}[e.StatusCode]
	return fmt.Sprintf("openai API error (HTTP %d): %v\n  hint: %s", e.StatusCode, err, hint), true
}
