// Package hosted runs delphi inside conductord: members delegate prompts to frontier models
// through provider batch APIs, with keys from the project vault and spend in the usage
// ledger.
//
// It keeps the server's privacy rule (DESIGN.md §12.4): a request body is passed straight to
// the provider when it is submitted and is never stored, and an answer is fetched from the
// provider when someone asks for it and is never stored. The database holds metadata only
// (migration 0016). The cost of that rule is that the server cannot resubmit on its own: an
// expired or failed request is resubmitted by whoever holds its text.
package hosted

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/delphi/anthropic"
	"github.com/aburan28/conductor/internal/delphi/openai"
	"github.com/aburan28/conductor/internal/domain"
)

// UsageSource is the usage_buckets source for delegated spend.
const UsageSource = "delegation"

// KeyOpener returns the plaintext of a project secret (the vault's Open, as a function so
// tests need no key material).
type KeyOpener func(ctx context.Context, project domain.Project, name string) (string, error)

// ProviderFactory builds a provider for one API key. Tests replace it.
type ProviderFactory func(kind, key string) (delphi.Provider, error)

// DefaultSecret is the vault secret a provider's key is read from when the project has no
// inference endpoint of that kind naming another one.
var DefaultSecret = map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY"}

// Service is conductord's delphi.
type Service struct {
	Store       *db.Store
	Key         KeyOpener
	NewProvider ProviderFactory
	Logger      *slog.Logger
	Now         func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// DefaultProvider builds the real Anthropic or OpenAI provider.
func DefaultProvider(kind, key string) (delphi.Provider, error) {
	switch kind {
	case "anthropic":
		return anthropic.NewWithKey(key), nil
	case "openai":
		return openai.NewWithKey(key), nil
	}
	return nil, fmt.Errorf("%w: unknown provider %q", domain.ErrInvalidArgument, kind)
}

// Offline validates requests without credentials.
func Offline() delphi.Registry {
	return delphi.Registry{"anthropic": anthropic.Offline(), "openai": openai.Offline()}
}

// provider opens the project's key for kind and builds a provider. The secret is the one an
// enabled inference endpoint of that kind names, or the default name; the base URL is always
// the provider's own (or the operator's environment), never a member-supplied one.
func (s *Service) provider(ctx context.Context, project domain.Project, kind string) (delphi.Provider, error) {
	if s.Key == nil {
		return nil, fmt.Errorf("%w: the secret vault is not enabled on this server", domain.ErrInvalidArgument)
	}
	secret := DefaultSecret[kind]
	if secret == "" {
		return nil, fmt.Errorf("%w: unknown provider %q", domain.ErrInvalidArgument, kind)
	}
	if eps, err := s.Store.ListInferenceEndpoints(ctx, project.ID); err == nil {
		for _, ep := range eps {
			if ep.Enabled && string(ep.Kind) == kind && ep.APIKeySecret != "" {
				secret = ep.APIKeySecret
				break
			}
		}
	}
	key, err := s.Key(ctx, project, secret)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("%w: project secret %s is not set; run `conductor secrets set %s`", domain.ErrInvalidArgument, secret, secret)
	}
	if err != nil {
		return nil, err
	}
	factory := s.NewProvider
	if factory == nil {
		factory = DefaultProvider
	}
	return factory(kind, key)
}

// SubmitRequest is the body of a delegation: delphi's input row plus Conductor fields.
type SubmitRequest struct {
	Ref       string            `json:"ref,omitempty"`
	Provider  string            `json:"provider,omitempty"`
	Model     string            `json:"model,omitempty"`
	System    string            `json:"system,omitempty"`
	Text      string            `json:"text,omitempty"`
	Messages  []any             `json:"messages,omitempty"`
	MaxTokens int               `json:"max_tokens,omitempty"`
	Effort    string            `json:"effort,omitempty"`
	Params    map[string]any    `json:"params,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	TaskID    domain.ID         `json:"task_id,omitempty"`
}

// Limits on caller labels, which are stored: they are for ids and tags, not content.
const (
	maxLabels     = 16
	maxLabelBytes = 200
	defaultTokens = 32000
)

func newRef() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "d-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func newLabel() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "conductor-" + hex.EncodeToString(b)
}

// toRow converts the body into delphi's input row so delphi.Normalize does all validation.
func (in SubmitRequest) toRow() map[string]any {
	row := map[string]any{"id": in.Ref}
	set := func(k string, v any, ok bool) {
		if ok {
			row[k] = v
		}
	}
	set("provider", in.Provider, in.Provider != "")
	set("model", in.Model, in.Model != "")
	set("system", in.System, in.System != "")
	set("prompt", in.Text, in.Text != "")
	set("messages", in.Messages, in.Messages != nil)
	set("effort", in.Effort, in.Effort != "")
	set("params", in.Params, in.Params != nil)
	if in.MaxTokens != 0 {
		row["max_tokens"] = jsonNumber(in.MaxTokens)
	}
	return row
}

// Submit validates a request, records it, and submits it to the provider as a batch of
// one. created is false when the same request was already submitted under this ref (a
// retry), in which case nothing new is sent.
//
// When the provider's answer to the submit is lost (a dropped connection), the delegation
// stays submitting: the batch may exist, so it is never resent blindly. The loop finds it
// again by label on OpenAI; on Anthropic an operator reconciles it.
func (s *Service) Submit(ctx context.Context, project domain.Project, by domain.ID, in SubmitRequest) (d domain.DelphiDelegation, created bool, err error) {
	if in.Ref == "" {
		in.Ref = newRef()
	}
	if len(in.Labels) > maxLabels {
		return d, false, fmt.Errorf("%w: at most %d labels", domain.ErrInvalidArgument, maxLabels)
	}
	for k, v := range in.Labels {
		if len(k)+len(v) > maxLabelBytes {
			return d, false, fmt.Errorf("%w: label %q is too long (labels hold ids and tags, not content)", domain.ErrInvalidArgument, k)
		}
	}
	if in.TaskID != "" {
		t, err := s.Store.GetTask(ctx, in.TaskID)
		if err != nil || t.ProjectID != project.ID {
			return d, false, fmt.Errorf("%w: task %s is not in this project", domain.ErrInvalidArgument, in.TaskID)
		}
	}
	defaults := delphi.Defaults{Provider: "anthropic", MaxTokens: defaultTokens}
	req, problems := delphi.Normalize(in.toRow(), defaults, Offline(), "request")
	if len(problems) > 0 {
		return d, false, fmt.Errorf("%w: %s", domain.ErrInvalidArgument, strings.Join(problems, "; "))
	}
	prov, err := s.provider(ctx, project, req.Provider)
	if err != nil {
		return d, false, err
	}
	row := domain.DelphiDelegation{OrganizationID: project.OrganizationID, ProjectID: project.ID, Ref: req.ID,
		PrincipalID: by, TaskID: in.TaskID, Provider: req.Provider, Model: req.Model,
		BodySHA256: delphi.Hash(req), Label: newLabel(), Labels: in.Labels}
	d, err = s.Store.CreateDelphiDelegation(ctx, row)
	if errors.Is(err, domain.ErrDuplicate) {
		if d.BodySHA256 == row.BodySHA256 {
			return d, false, nil
		}
		return domain.DelphiDelegation{}, false, fmt.Errorf("%w: ref %q is already used by a different request", domain.ErrInvalidArgument, req.ID)
	}
	if err != nil {
		return d, false, err
	}
	batchID, err := prov.Submit(ctx, []delphi.Request{req}, d.Label)
	if err != nil {
		status, sure := definitelyNotCreated(err)
		if !sure {
			s.log().Warn("delphi submit outcome unknown; the delegation waits for reconciliation",
				"project", project.ID, "ref", d.Ref, "provider", d.Provider, "error", err)
			return d, true, nil
		}
		if derr := s.Store.DeleteUnsubmittedDelphi(context.WithoutCancel(ctx), d.ID); derr != nil {
			return d, false, derr
		}
		if status >= 400 && status < 500 && status != 408 && status != 409 && status != 429 {
			return domain.DelphiDelegation{}, false, fmt.Errorf("%w: %s rejected the request: %s", domain.ErrInvalidArgument, d.Provider, describe(err))
		}
		return domain.DelphiDelegation{}, false, fmt.Errorf("%w: %s did not accept the batch (%s); retry later", domain.ErrCapacity, d.Provider, describe(err))
	}
	d, err = s.Store.SetDelphiRunning(context.WithoutCancel(ctx), d.ID, batchID)
	return d, true, err
}

func describe(err error) string {
	if s, ok := anthropic.Describe(err); ok {
		return s
	}
	if s, ok := openai.Describe(err); ok {
		return s
	}
	return err.Error()
}

func statusOf(err error) int {
	var ae *anthropicsdk.Error
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	var oe *openaisdk.Error
	if errors.As(err, &oe) {
		return oe.StatusCode
	}
	return 0
}

// definitelyNotCreated reports whether a failed submit cannot have created a batch: the
// provider answered with an error status, or the connection was never made.
func definitelyNotCreated(err error) (status int, sure bool) {
	if s := statusOf(err); s != 0 {
		return s, true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return 0, true
	}
	return 0, false
}

// Answer is a finished delegation with the model's text, fetched from the provider now.
type Answer struct {
	Delegation  domain.DelphiDelegation `json:"delegation"`
	Text        string                  `json:"text"`
	Refusal     *delphi.Refusal         `json:"refusal,omitempty"`
	Unavailable string                  `json:"unavailable,omitempty"`
}

// Answer fetches a finished delegation's answer from the provider. Nothing is stored; the
// provider keeps results for about 29 days (Anthropic) or until the output file expires
// (OpenAI).
func (s *Service) Answer(ctx context.Context, project domain.Project, ref string) (Answer, error) {
	d, err := s.Store.GetDelphiDelegation(ctx, project.ID, ref)
	if err != nil {
		return Answer{}, err
	}
	a := Answer{Delegation: d}
	if d.State != domain.DelphiFinished {
		return a, fmt.Errorf("%w: delegation %s is %s, not finished", domain.ErrIllegalTransition, ref, d.State)
	}
	if d.Status != string(delphi.Succeeded) && d.Status != string(delphi.Refused) {
		a.Unavailable = "the request " + d.Status + "; there is no answer"
		return a, nil
	}
	prov, err := s.provider(ctx, project, d.Provider)
	if err != nil {
		return a, err
	}
	found := false
	err = prov.Results(ctx, d.BatchID, func(r delphi.Result) error {
		if r.ID == d.Ref {
			a.Text, a.Refusal, found = r.Text, r.Refusal, true
		}
		return nil
	})
	if err != nil {
		return a, fmt.Errorf("%w: fetching the answer from %s: %s", domain.ErrCapacity, d.Provider, describe(err))
	}
	if !found {
		a.Unavailable = "the provider no longer has this result (results are kept for a limited time)"
	}
	return a, nil
}

// Cancel asks the provider to cancel a running delegation's batch. The loop records the
// outcome when the batch ends.
func (s *Service) Cancel(ctx context.Context, project domain.Project, ref string) (domain.DelphiDelegation, error) {
	d, err := s.Store.GetDelphiDelegation(ctx, project.ID, ref)
	if err != nil {
		return d, err
	}
	if d.State != domain.DelphiRunning {
		return d, fmt.Errorf("%w: delegation %s is %s; only a running one can be canceled", domain.ErrIllegalTransition, ref, d.State)
	}
	prov, err := s.provider(ctx, project, d.Provider)
	if err != nil {
		return d, err
	}
	if err := prov.Cancel(ctx, d.BatchID); err != nil {
		return d, fmt.Errorf("%w: %s", domain.ErrCapacity, describe(err))
	}
	return d, nil
}

// Reconcile resolves a delegation stuck in submitting: adopt binds it to a batch an
// operator found in the provider console; abandon declares that no batch was created.
func (s *Service) Reconcile(ctx context.Context, project domain.Project, ref, adoptBatchID string, abandon bool) (domain.DelphiDelegation, error) {
	d, err := s.Store.GetDelphiDelegation(ctx, project.ID, ref)
	if err != nil {
		return d, err
	}
	switch {
	case adoptBatchID != "" && !abandon:
		return s.Store.SetDelphiRunning(ctx, d.ID, adoptBatchID)
	case abandon && adoptBatchID == "":
		return s.Store.AbandonDelphi(ctx, d.ID)
	}
	return d, fmt.Errorf("%w: give a batch id to adopt, or abandon", domain.ErrInvalidArgument)
}

// Run ticks every interval until ctx ends. Only one replica works at a time.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.Store.TryExclusive(ctx, "conductor/delphi", s.Tick); err != nil && ctx.Err() == nil {
			s.log().Warn("delphi tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick polls every open delegation once and records the ones whose batch ended.
func (s *Service) Tick(ctx context.Context) error {
	open, err := s.Store.ListOpenDelphiDelegations(ctx)
	if err != nil {
		return err
	}
	type key struct{ project, provider string }
	providers := map[key]delphi.Provider{}
	projects := map[domain.ID]domain.Project{}
	for _, d := range open {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		project, ok := projects[d.ProjectID]
		if !ok {
			if project, err = s.Store.GetProject(ctx, d.ProjectID); err != nil {
				return err
			}
			projects[d.ProjectID] = project
		}
		k := key{d.ProjectID, d.Provider}
		prov, ok := providers[k]
		if !ok {
			if prov, err = s.provider(ctx, project, d.Provider); err != nil {
				s.log().Warn("delphi: no provider for open delegations", "project", d.ProjectID, "provider", d.Provider, "error", err)
				providers[k] = nil
				continue
			}
			providers[k] = prov
		}
		if prov == nil {
			continue
		}
		if err := s.advance(ctx, project, prov, d); err != nil {
			s.log().Warn("delphi: delegation not advanced", "project", d.ProjectID, "ref", d.Ref, "error", describe(err))
		}
	}
	return nil
}

func (s *Service) advance(ctx context.Context, project domain.Project, prov delphi.Provider, d domain.DelphiDelegation) error {
	if d.State == domain.DelphiSubmitting {
		if s.now().Sub(d.SubmittedAt) < time.Minute {
			return nil // a submit may still be in flight on another request
		}
		id, found, err := prov.Find(ctx, d.Label)
		if errors.Is(err, delphi.ErrNotSupported) || (err == nil && !found) {
			return nil // waits for `conductor delphi reconcile`
		}
		if err != nil {
			return err
		}
		if d, err = s.Store.SetDelphiRunning(ctx, d.ID, id); err != nil {
			return err
		}
	}
	st, err := prov.Poll(ctx, d.BatchID)
	if err != nil || !st.Ended {
		return err
	}
	var res delphi.Result
	found := false
	if st.Failure == "" {
		err = prov.Results(ctx, d.BatchID, func(r delphi.Result) error {
			if r.ID == d.Ref {
				res, found = r, true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	switch {
	case st.Failure != "":
		res = delphi.Result{Status: delphi.Errored, Error: &delphi.Error{Type: "batch_failed", Message: st.Failure}}
	case !found:
		res = delphi.Result{Status: delphi.Missing}
	}
	o := db.DelphiOutcome{Status: string(res.Status), Truncated: res.Truncated, StopReason: res.StopReason, FinishedAt: s.now()}
	if res.Usage != nil {
		o.InputTokens, o.OutputTokens = res.Usage.InputTokens, res.Usage.OutputTokens
		o.CacheReadTokens, o.CacheWriteTokens = res.Usage.CacheReadTokens, res.Usage.CacheWriteTokens
		if c, ok := delphi.Cost(prov, d.Model, *res.Usage); ok {
			o.CostUSD = c
		}
	}
	if res.Error != nil {
		o.ErrorType, o.ErrorDetail = res.Error.Type, clamp(res.Error.Message, 500)
	}
	if res.Refusal != nil {
		o.RefusalCategory = res.Refusal.Category
	}
	// The answer text in res is dropped here: it is fetched again when asked for.
	fin, changed, err := s.Store.FinishDelphi(ctx, d.ID, o)
	if err != nil || !changed {
		return err
	}
	s.recordUsage(ctx, fin)
	s.announce(ctx, project, fin)
	return nil
}

func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// recordUsage adds the delegation's tokens to the usage ledger under source "delegation".
// Cost is the batch price when the model's price is known, and is otherwise left unpriced:
// catalog list prices would overstate batch spend by half.
func (s *Service) recordUsage(ctx context.Context, d domain.DelphiDelegation) {
	if d.PrincipalID == "" || d.InputTokens+d.OutputTokens == 0 {
		return
	}
	at := d.SubmittedAt
	if d.FinishedAt != nil {
		at = *d.FinishedAt
	}
	row := domain.UsageBucket{ProjectID: d.ProjectID, PrincipalID: d.PrincipalID, Source: UsageSource,
		Harness: "delphi", Model: d.Model, Provider: d.Provider, ExternalSessionID: "delphi:" + d.ID,
		BucketStart: at.UTC().Truncate(time.Hour), Requests: 1, InputTokens: d.InputTokens,
		OutputTokens: d.OutputTokens, CacheReadTokens: d.CacheReadTokens, CacheWriteTokens: d.CacheWriteTokens,
		CostUSD: d.CostUSD}
	if row.CostUSD > 0 {
		row.CostSource = "reported"
	}
	if _, err := s.Store.UpsertUsageBuckets(ctx, []domain.UsageBucket{row}); err != nil {
		s.log().Warn("delphi: usage not recorded", "ref", d.Ref, "error", err)
	}
}

// announce posts delphi.finished on the delegation's task, if it has one. The payload uses
// allowlisted keys only (privacy.EventPayloadAllowlist) and carries no answer text.
func (s *Service) announce(ctx context.Context, project domain.Project, d domain.DelphiDelegation) {
	if d.TaskID == "" {
		return
	}
	payload := map[string]any{"kind": "delphi", "status": d.Status, "provider": d.Provider,
		"summary": "delphi " + d.Ref + " " + d.Status, "tokens_in": d.InputTokens, "tokens_out": d.OutputTokens}
	if err := s.Store.AppendEvent(ctx, project.OrganizationID, project.ID, d.PrincipalID, "task", d.TaskID,
		"delphi.finished", domain.VisibilityTeamSummary, payload); err != nil {
		s.log().Warn("delphi: task event not recorded", "ref", d.Ref, "task", d.TaskID, "error", err)
	}
}

// SortByRef orders delegations by ref, for stable output.
func SortByRef(ds []domain.DelphiDelegation) {
	sort.Slice(ds, func(i, j int) bool { return ds[i].Ref < ds[j].Ref })
}

// jsonNumber renders an int the way delphi.Normalize expects decoded JSON numbers.
func jsonNumber(n int) json.Number { return json.Number(strconv.Itoa(n)) }
