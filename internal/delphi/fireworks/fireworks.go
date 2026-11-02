// Package fireworks is delphi's provider for Fireworks batch inference
// (https://docs.fireworks.ai/guides/batch-inference): requests are written to a JSONL
// dataset, a batch inference job runs them at half the serverless price, and the answers are
// read back from the job's output dataset.
//
// It is registered as "fireworks-batch": "fireworks" is the live OpenAI-compatible endpoint
// (internal/delphi/compat), which answers at once at the full price.
//
// A job serves one model, so a flight with several models becomes one job per model; their
// names are joined with "|". Every id is derived from the flight's label (the job id is the
// caller's to choose), so Find recovers a flight after a crash with a plain GET.
//
// Fireworks documents the output dataset as a results file and an error file but not their
// line format; Results reads each line defensively (custom_id with a chat completion, an
// OpenAI-batch-style {response: {body}}, or an error) and reports a line it cannot read.
package fireworks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/delphi/compat"
)

// Name is the provider's registry name.
const Name = "fireworks-batch"

// DefaultBaseURL is the Fireworks API; FIREWORKS_BASE_URL overrides it.
const DefaultBaseURL = "https://api.fireworks.ai"

// Provider is the Fireworks batch inference provider.
type Provider struct {
	key, account, base string
	model              string
	prices             map[string][2]float64
	http               *http.Client
}

// New reads FIREWORKS_API_KEY and FIREWORKS_ACCOUNT_ID.
func New() (*Provider, error) {
	key, acct := os.Getenv("FIREWORKS_API_KEY"), os.Getenv("FIREWORKS_ACCOUNT_ID")
	if key == "" || acct == "" {
		return nil, errors.New(Name + ": export FIREWORKS_API_KEY and FIREWORKS_ACCOUNT_ID")
	}
	p := Offline()
	p.key, p.account, p.http = key, acct, &http.Client{Timeout: 10 * time.Minute}
	return p, nil
}

// Offline validates requests without credentials.
func Offline() *Provider {
	base := os.Getenv("FIREWORKS_BASE_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	prices := map[string][2]float64{}
	if s := os.Getenv("DELPHI_FIREWORKS_BATCH_PRICES"); s != "" {
		_ = json.Unmarshal([]byte(s), &prices)
	}
	return &Provider{base: strings.TrimRight(base, "/"), model: os.Getenv("DELPHI_FIREWORKS_BATCH_MODEL"), prices: prices}
}

func (p *Provider) Name() string         { return Name }
func (p *Provider) DefaultModel() string { return p.model }

// Limits keeps a dataset far under the documented 80 GiB.
func (p *Provider) Limits() delphi.Limits {
	return delphi.Limits{MaxRequests: 50000, MaxBytes: 400 << 20}
}

// Price is the batch price from DELPHI_FIREWORKS_BATCH_PRICES ($/MTok in, out); none built in.
func (p *Provider) Price(model string) (float64, float64, bool) {
	pr, ok := p.prices[model]
	return pr[0], pr[1], ok
}

func (p *Provider) Validate(r delphi.Request) []string {
	if !strings.HasPrefix(r.Model, "accounts/") {
		return []string{fmt.Sprintf("model %q: Fireworks batch jobs name models in full, e.g. accounts/fireworks/models/deepseek-v4", r.Model)}
	}
	return nil
}

// line is one row of the input dataset.
func line(r delphi.Request) map[string]any {
	body := compat.Body(r)
	delete(body, "model") // the job names the model
	return map[string]any{"custom_id": r.ID, "body": body}
}

// HTTPError is a non-2xx answer.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%s: HTTP %d: %s", Name, e.StatusCode, e.Message)
}

// Describe renders a Fireworks API error for people.
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

func (p *Provider) acct(path string) string {
	return p.base + "/v1/accounts/" + url.PathEscape(p.account) + "/" + path
}

func (p *Provider) send(ctx context.Context, method, u, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.key)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw))
		var e struct {
			Error   any    `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) == nil {
			if e.Message != "" {
				msg = e.Message
			} else if s, ok := e.Error.(string); ok && s != "" {
				msg = s
			}
		}
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		return &HTTPError{StatusCode: resp.StatusCode, Message: msg}
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (p *Provider) postJSON(ctx context.Context, u string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return p.send(ctx, http.MethodPost, u, "application/json", bytes.NewReader(b), out)
}

var unsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// ids derives the job and dataset ids for model i of n from the flight label.
func ids(label string, i, n int) (job, in, out string) {
	base := strings.Trim(unsafe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(base) > 50 {
		base = base[:50]
	}
	if n > 1 {
		base = fmt.Sprintf("%s-m%d", base, i+1)
	}
	return base, base + "-in", base + "-out"
}

// Submit uploads one dataset and creates one job per model; it returns the jobs' names.
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
	var jobs []string
	for i, model := range models {
		jobID, inID, outID := ids(label, i, len(models))
		var data bytes.Buffer
		enc := json.NewEncoder(&data)
		for _, r := range byModel[model] {
			if err := enc.Encode(line(r)); err != nil {
				return strings.Join(jobs, "|"), err
			}
		}
		// Nothing billable exists until the job: dataset errors are definite.
		if err := p.postJSON(ctx, p.acct("datasets"), map[string]any{"datasetId": inID, "dataset": map[string]any{"userUploaded": map[string]any{}}}, nil); err != nil && StatusOf(err) != http.StatusConflict {
			return p.partial(jobs, len(models), err)
		}
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		fw, err := mw.CreateFormFile("file", inID+".jsonl")
		if err != nil {
			return p.partial(jobs, len(models), err)
		}
		_, _ = fw.Write(data.Bytes())
		_ = mw.Close()
		if err := p.send(ctx, http.MethodPost, p.acct("datasets/"+inID+":upload"), mw.FormDataContentType(), &form, nil); err != nil {
			return p.partial(jobs, len(models), err)
		}
		job := map[string]any{"displayName": label, "model": model,
			"inputDatasetId":  "accounts/" + p.account + "/datasets/" + inID,
			"outputDatasetId": "accounts/" + p.account + "/datasets/" + outID}
		var created struct {
			Name string `json:"name"`
		}
		if err := p.postJSON(ctx, p.acct("batchInferenceJobs?batchInferenceJobId="+url.QueryEscape(jobID)), job, &created); err != nil {
			return p.partial(jobs, len(models), err)
		}
		if created.Name == "" {
			created.Name = "accounts/" + p.account + "/batchInferenceJobs/" + jobID
		}
		jobs = append(jobs, created.Name)
	}
	return strings.Join(jobs, "|"), nil
}

// partial reports a failure after some jobs exist without it reading as "nothing created".
func (p *Provider) partial(jobs []string, n int, err error) (string, error) {
	if len(jobs) == 0 {
		return "", err
	}
	return strings.Join(jobs, "|"), fmt.Errorf("%s: created %d of %d jobs, then: %v", Name, len(jobs), n, err)
}

type job struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	OutputDatasetID string `json:"outputDatasetId"`
	Status          *struct {
		Message string `json:"message"`
	} `json:"status"`
	JobProgress map[string]any `json:"jobProgress"`
}

func (p *Provider) get(ctx context.Context, name string) (job, error) {
	var j job
	err := p.send(ctx, http.MethodGet, p.base+"/v1/"+strings.TrimPrefix(name, "/"), "", nil, &j)
	return j, err
}

var ended = map[string]bool{"COMPLETED": true, "FAILED": true, "CANCELLED": true, "EXPIRED": true,
	"EARLY_STOPPED": true, "DELETED": true, "ARCHIVED": true}

// Poll ends when every job of the flight has.
func (p *Provider) Poll(ctx context.Context, id string) (delphi.BatchStatus, error) {
	st := delphi.BatchStatus{Ended: true, Counts: map[string]int64{}}
	var states, failures []string
	for _, name := range strings.Split(id, "|") {
		j, err := p.get(ctx, name)
		if err != nil {
			return st, err
		}
		s := strings.TrimPrefix(j.State, "JOB_STATE_")
		states = append(states, s)
		st.Counts[strings.ToLower(s)]++
		if !ended[s] {
			st.Ended = false
		}
		if s == "FAILED" {
			msg := name + " failed"
			if j.Status != nil && j.Status.Message != "" {
				msg += ": " + j.Status.Message
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

// Results reads every file of each job's output dataset.
func (p *Provider) Results(ctx context.Context, id string, emit func(delphi.Result) error) error {
	for _, name := range strings.Split(id, "|") {
		j, err := p.get(ctx, name)
		if err != nil {
			return err
		}
		out := j.OutputDatasetID
		if out == "" {
			return fmt.Errorf("%s: job %s names no output dataset", Name, name)
		}
		if i := strings.LastIndex(out, "/datasets/"); i >= 0 {
			out = out[i+len("/datasets/"):]
		}
		var dl struct {
			FilenameToSignedUrls map[string]string `json:"filenameToSignedUrls"`
		}
		if err := p.send(ctx, http.MethodGet, p.acct("datasets/"+url.PathEscape(out)+":getDownloadEndpoint"), "", nil, &dl); err != nil {
			return err
		}
		files := make([]string, 0, len(dl.FilenameToSignedUrls))
		for f := range dl.FilenameToSignedUrls {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			if err := p.readFile(ctx, dl.FilenameToSignedUrls[f], emit); err != nil {
				return fmt.Errorf("%s: %s: %w", Name, f, err)
			}
		}
	}
	return nil
}

func (p *Provider) readFile(ctx context.Context, signed string, emit func(delphi.Result) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signed, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req) // a signed URL: no Authorization header
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &HTTPError{StatusCode: resp.StatusCode, Message: "downloading a result file"}
	}
	return ParseOutput(resp.Body, emit)
}

// ParseOutput reads output JSONL. Lines without a custom_id (headers, summaries) are skipped.
func ParseOutput(rd io.Reader, emit func(delphi.Result) error) error {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return fmt.Errorf("unreadable output line: %w", err)
		}
		var id string
		for _, k := range []string{"custom_id", "customId"} { // never a completion's own id
			if v, ok := row[k]; ok && json.Unmarshal(v, &id) == nil && id != "" {
				break
			}
		}
		if id == "" {
			continue
		}
		if err := emit(toResult(id, row)); err != nil {
			return err
		}
	}
	return sc.Err()
}

type completion struct {
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
}

func toResult(id string, row map[string]json.RawMessage) delphi.Result {
	res := delphi.Result{ID: id}
	if e, ok := row["error"]; ok && string(e) != "null" {
		msg := strings.Trim(string(e), `"`)
		var obj struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		}
		if json.Unmarshal(e, &obj) == nil && obj.Message != "" {
			msg = obj.Message
		}
		res.Status, res.Error = delphi.Errored, &delphi.Error{Type: "batch_request_failed", Message: msg, Retryable: true}
		return res
	}
	// The completion is the row itself, its response, or its response's body.
	body := json.RawMessage(nil)
	if r, ok := row["response"]; ok {
		var wrapped struct {
			Body json.RawMessage `json:"body"`
		}
		if json.Unmarshal(r, &wrapped) == nil && len(wrapped.Body) > 0 {
			body = wrapped.Body
		} else {
			body = r
		}
	} else if _, ok := row["choices"]; ok {
		b, _ := json.Marshal(row)
		body = b
	}
	var c completion
	if len(body) == 0 || json.Unmarshal(body, &c) != nil || len(c.Choices) == 0 {
		res.Status, res.Error = delphi.Errored, &delphi.Error{Type: "bad_response", Message: "no completion in the output line", Retryable: true}
		return res
	}
	ch := c.Choices[0]
	res.Model, res.StopReason = c.Model, ch.FinishReason
	res.Status, res.Text = delphi.Succeeded, text(ch.Message.Content)
	if u := c.Usage; u != nil {
		res.Usage = &delphi.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
		if u.PromptTokensDetails != nil {
			res.Usage.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		}
	}
	switch {
	case ch.Message.Refusal != "" && res.Text == "":
		res.Status, res.Refusal = delphi.Refused, &delphi.Refusal{Category: "refusal", Explanation: ch.Message.Refusal}
	case ch.FinishReason == "content_filter" && res.Text == "":
		res.Status, res.Refusal = delphi.Refused, &delphi.Refusal{Category: "content_filter"}
	case ch.FinishReason == "length":
		res.Truncated = true
	}
	return res
}

func text(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if m, ok := part.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// Cancel is not supported: Fireworks documents no cancel for batch jobs, and deleting one
// is not documented to stop it.
func (p *Provider) Cancel(context.Context, string) error { return delphi.ErrNotSupported }

// Find recovers a flight from its label: the job ids are derived from it.
func (p *Provider) Find(ctx context.Context, label string) (string, bool, error) {
	acct := "accounts/" + p.account + "/batchInferenceJobs/"
	single, _, _ := ids(label, 0, 1)
	if _, err := p.get(ctx, acct+single); err == nil {
		return acct + single, true, nil
	} else if StatusOf(err) != http.StatusNotFound {
		return "", false, err
	}
	var names []string
	for i := 0; i < 64; i++ {
		jobID, _, _ := ids(label, i, 2)
		if _, err := p.get(ctx, acct+jobID); err != nil {
			if StatusOf(err) == http.StatusNotFound {
				break
			}
			return "", false, err
		}
		names = append(names, acct+jobID)
	}
	return strings.Join(names, "|"), len(names) > 0, nil
}
