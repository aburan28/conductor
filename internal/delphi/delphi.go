// Package delphi delegates hard prompts to frontier models through provider
// batch APIs (Anthropic Message Batches, OpenAI Batch): requests are queued,
// flushed as batches, and answered minutes to 24 hours later at half the
// interactive price.
//
// The package is provider-neutral: a [Request] names its provider and model, a
// [Provider] turns requests into one provider's batch API calls, and every
// answer comes back as a [Result] with the same status vocabulary. The
// [Engine] runs the loop over a durable [Store].
package delphi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Request is one prompt to delegate. Content of a message is a string or a
// provider-native list of content blocks.
type Request struct {
	ID        string            `json:"id"`
	Provider  string            `json:"provider"`
	Model     string            `json:"model"`
	System    string            `json:"system,omitempty"`
	Messages  []Message         `json:"messages"`
	MaxTokens int               `json:"max_tokens"`
	Effort    string            `json:"effort,omitempty"`
	Params    map[string]any    `json:"params,omitempty"` // provider-specific request fields, passed through
	Meta      map[string]string `json:"meta,omitempty"`   // opaque to delphi; copied onto the result (task ids, paths, ...)
	Attempt   int               `json:"attempt,omitempty"`
}

// Message is one conversation turn.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Status is the outcome of one request.
type Status string

const (
	Succeeded Status = "succeeded"
	Refused   Status = "refused" // the model's safeguards declined (HTTP 200)
	Errored   Status = "errored"
	Expired   Status = "expired"  // the batch window closed before this request ran
	Canceled  Status = "canceled" // the batch was canceled before this request ran
	Missing   Status = "missing"  // the provider returned no result for this id
)

// Usage counts tokens for one request.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
}

// Add accumulates u into the receiver.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CacheReadTokens += o.CacheReadTokens
}

// Error describes a failed request.
type Error struct {
	Type      string `json:"type"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	// Retryable is false when resubmitting the same request would fail the
	// same way (invalid requests, a batch the provider rejected outright).
	Retryable bool `json:"retryable"`
}

// Refusal carries the provider's reason for declining.
type Refusal struct {
	Category    string `json:"category,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

// Result is the answer to one Request.
type Result struct {
	ID         string            `json:"id"`
	Provider   string            `json:"provider"`
	Model      string            `json:"model,omitempty"`
	Status     Status            `json:"status"`
	Text       string            `json:"text,omitempty"`
	StopReason string            `json:"stop_reason,omitempty"`
	Truncated  bool              `json:"truncated,omitempty"` // stopped at the output token limit
	Usage      *Usage            `json:"usage,omitempty"`
	Refusal    *Refusal          `json:"refusal,omitempty"`
	Error      *Error            `json:"error,omitempty"`
	BatchID    string            `json:"batch_id,omitempty"`
	Attempt    int               `json:"attempt,omitempty"`
	Meta       map[string]string `json:"meta,omitempty"`
	FinishedAt string            `json:"finished_at,omitempty"`
	Hash       string            `json:"request_hash,omitempty"` // content hash of the request answered
}

// Retryable reports whether resubmitting the request could succeed. A refusal
// or an invalid request would fail the same way; a deliberate cancel is final.
func (r Result) Retryable() bool {
	switch r.Status {
	case Expired, Missing:
		return true
	case Errored:
		return r.Error != nil && r.Error.Retryable
	}
	return false
}

// Limits bound one provider batch.
type Limits struct {
	MaxRequests int
	MaxBytes    int
}

// BatchStatus is a provider batch's progress.
type BatchStatus struct {
	Ended   bool             `json:"ended"`
	State   string           `json:"state"` // the provider's own word for it
	Counts  map[string]int64 `json:"counts,omitempty"`
	Failure string           `json:"failure,omitempty"` // set when the provider rejected the whole batch
}

// ErrNotSupported is returned by optional Provider methods.
var ErrNotSupported = errors.New("not supported by this provider")

// Provider runs requests through one vendor's batch API.
type Provider interface {
	Name() string
	// DefaultModel is used for requests that name no model; "" means the
	// request must name one.
	DefaultModel() string
	Limits() Limits
	// Validate returns provider-specific problems with r (already normalized).
	Validate(r Request) []string
	// Submit creates one batch. label is unique per submission and is stored
	// on the batch where the provider supports metadata, so a crash between
	// creation and bookkeeping can be reconciled with Find.
	Submit(ctx context.Context, reqs []Request, label string) (batchID string, err error)
	Poll(ctx context.Context, batchID string) (BatchStatus, error)
	// Results streams one Result per answered request of an ended batch.
	Results(ctx context.Context, batchID string, emit func(Result) error) error
	Cancel(ctx context.Context, batchID string) error
	// Find returns the batch created with label, or ErrNotSupported.
	Find(ctx context.Context, label string) (batchID string, found bool, err error)
	// Price is the batch price per million input and output tokens, if known.
	Price(model string) (in, out float64, ok bool)
}

// Registry maps provider names to providers.
type Registry map[string]Provider

// Names lists the registered providers.
func (r Registry) Names() []string {
	out := make([]string, 0, len(r))
	for n := range r {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var rowKeys = map[string]bool{"id": true, "custom_id": true, "provider": true, "model": true, "prompt": true, "messages": true,
	"system": true, "max_tokens": true, "effort": true, "params": true, "meta": true}

// reservedParams belong to the neutral request, not to params.
var reservedParams = map[string]string{
	"model": "model", "messages": "prompt/messages", "input": "prompt/messages", "system": "system",
	"instructions": "system", "max_tokens": "max_tokens", "max_output_tokens": "max_tokens", "stream": "",
}

// Defaults fill fields a row leaves out.
type Defaults struct {
	Provider  string
	Model     string // overrides the provider default when set
	MaxTokens int
	Effort    string
	System    string
}

// RequestError lists every problem found in an input.
type RequestError struct{ Problems []string }

func (e *RequestError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0]
	}
	return fmt.Sprintf("%d problems, first: %s", len(e.Problems), e.Problems[0])
}

// Normalize builds a Request from one decoded input row (numbers as
// json.Number) and validates it, including the provider's own rules.
func Normalize(row map[string]any, d Defaults, reg Registry, where string) (Request, []string) {
	var p []string
	add := func(f string, a ...any) { p = append(p, where+": "+fmt.Sprintf(f, a...)) }

	var unknown []string
	for k := range row {
		if !rowKeys[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		add("unknown keys %v", unknown)
	}
	str := func(k string) (string, bool) {
		v, present := row[k]
		if !present || v == nil {
			return "", false
		}
		s, ok := v.(string)
		if !ok {
			add("%s must be a string", k)
		}
		return s, true
	}

	r := Request{Provider: d.Provider, MaxTokens: d.MaxTokens, Effort: d.Effort, System: d.System}
	id, ok := str("id")
	if cid, ok2 := str("custom_id"); ok2 {
		if ok {
			add("give id or custom_id, not both")
		}
		id, ok = cid, true
	}
	if !ok || !idRE.MatchString(id) {
		add("id must match [A-Za-z0-9_-]{1,64}, got %q", id)
	}
	r.ID = id
	if s, ok := str("provider"); ok {
		r.Provider = s
	}
	prov, known := reg[r.Provider]
	if !known {
		add("provider %q is not one of %v", r.Provider, reg.Names())
	}
	r.Model = d.Model
	if s, ok := str("model"); ok {
		r.Model = s
	}
	if r.Model == "" && known {
		r.Model = prov.DefaultModel()
	}
	if r.Model == "" && known {
		add("no model: set it per row, with -model, or with DELPHI_%s_MODEL", strings.ToUpper(r.Provider))
	}
	if s, ok := str("system"); ok {
		r.System = s
	}
	if s, ok := str("effort"); ok {
		r.Effort = s
	}

	_, hasPrompt := row["prompt"]
	_, hasMessages := row["messages"]
	switch {
	case hasPrompt == hasMessages:
		add("give exactly one of prompt or messages")
	case hasPrompt:
		s, _ := row["prompt"].(string)
		if strings.TrimSpace(s) == "" {
			add("prompt must be a non-empty string")
		}
		r.Messages = []Message{{Role: "user", Content: s}}
	default:
		raw, _ := row["messages"].([]any)
		if len(raw) == 0 {
			add("messages must be a non-empty list")
		}
		for i, m := range raw {
			mm, _ := m.(map[string]any)
			role, _ := mm["role"].(string)
			if role != "user" && role != "assistant" && role != "system" {
				add("messages[%d] needs role user, assistant or system", i)
				continue
			}
			switch mm["content"].(type) {
			case string, []any:
			default:
				add("messages[%d].content must be a string or a list of blocks", i)
			}
			r.Messages = append(r.Messages, Message{Role: role, Content: mm["content"]})
		}
		if n := len(r.Messages); n > 0 && r.Messages[n-1].Role != "user" {
			add("the last message must have role user (assistant prefill is not supported)")
		}
	}

	if v, ok := row["max_tokens"]; ok {
		n, isNum := v.(json.Number)
		i, err := int64(0), error(nil)
		if isNum {
			i, err = n.Int64()
		}
		if !isNum || err != nil || i < 1 {
			add("max_tokens must be a positive integer")
		}
		r.MaxTokens = int(i)
	}
	if r.MaxTokens < 1 {
		add("max_tokens must be a positive integer")
	}

	if v, ok := row["params"]; ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			add("params must be an object")
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if use, bad := reservedParams[k]; bad {
				if use == "" {
					add("params.%s is not allowed (batch requests are non-streaming)", k)
				} else {
					add("params.%s: use the row-level %s key", k, use)
				}
			}
		}
		r.Params = m
	}
	if v, ok := row["meta"]; ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			add("meta must be an object of strings")
		}
		r.Meta = map[string]string{}
		for k, x := range m {
			s, isStr := x.(string)
			if !isStr {
				add("meta.%s must be a string", k)
			}
			r.Meta[k] = s
		}
	}
	if len(p) == 0 {
		for _, msg := range prov.Validate(r) {
			add("%s", msg)
		}
	}
	return r, p
}

// Parse reads JSONL rows and reports every problem, not just the first.
func Parse(data []byte, d Defaults, reg Registry) ([]Request, error) {
	var out []Request
	var problems []string
	seen := map[string]int{}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		where := fmt.Sprintf("line %d", n+1)
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		var row map[string]any
		if err := dec.Decode(&row); err != nil || row == nil {
			problems = append(problems, where+": invalid JSON object")
			continue
		}
		r, p := Normalize(row, d, reg, where)
		if len(p) > 0 {
			problems = append(problems, p...)
			continue
		}
		if first, dup := seen[r.ID]; dup {
			problems = append(problems, fmt.Sprintf("%s: duplicate id %q (first on line %d)", where, r.ID, first))
			continue
		}
		seen[r.ID] = n + 1
		out = append(out, r)
	}
	if len(out) == 0 && len(problems) == 0 {
		problems = append(problems, "no requests found")
	}
	if len(problems) > 0 {
		return nil, &RequestError{Problems: problems}
	}
	return out, nil
}

// Chunk splits requests into groups within a provider's batch limits.
func Chunk(reqs []Request, lim Limits) ([][]Request, error) {
	var chunks [][]Request
	var cur []Request
	size := 0
	for _, r := range reqs {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		n := len(b) + 256 // envelope overhead added by the provider encoding
		if n > lim.MaxBytes {
			return nil, &RequestError{Problems: []string{fmt.Sprintf("request %q alone exceeds the batch size limit", r.ID)}}
		}
		if len(cur) > 0 && (len(cur) >= lim.MaxRequests || size+n > lim.MaxBytes) {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, r)
		size += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks, nil
}

// Cost estimates the batch cost of usage; ok is false when the model has no
// known price. Cache tokens are not priced.
func Cost(p Provider, model string, u Usage) (usd float64, ok bool) {
	in, out, ok := p.Price(model)
	if !ok {
		return 0, false
	}
	return (float64(u.InputTokens)*in + float64(u.OutputTokens)*out) / 1e6, true
}
