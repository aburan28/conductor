package modelsrc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

// KeyOpener returns the plaintext of a project secret. It is a function so this package does
// not depend on the vault, and so tests need no key material.
type KeyOpener func(ctx context.Context, project domain.Project, name string) (string, error)

// Service refreshes endpoints and keeps selector-driven catalog profiles pointed at what
// the endpoints currently serve.
type Service struct {
	Store  *db.Store
	Lister Lister
	// Key opens an endpoint's API key; nil means only keyless endpoints can be refreshed.
	Key    KeyOpener
	Logger *slog.Logger
	Now    func() time.Time

	locks sync.Map // endpoint id -> *sync.Mutex
}

// Resolution is the outcome for one selector profile after a refresh.
type Resolution struct {
	Alias   string `json:"alias"`
	Harness string `json:"harness"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Changed bool   `json:"changed"`
	// Note explains why a profile kept its model.
	Note string `json:"note,omitempty"`
}

// Result is what one refresh found and changed.
type Result struct {
	Endpoint    domain.InferenceEndpoint `json:"endpoint"`
	Added       []string                 `json:"added"`
	Returned    []string                 `json:"returned"`
	Retired     []string                 `json:"retired"`
	Resolutions []Resolution             `json:"resolutions"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Service) lock(id domain.ID) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// Refresh asks one endpoint what it serves, records the answer, and re-resolves the
// project's selector profiles. A failure to reach the endpoint is recorded on it and
// returned; nothing the endpoint previously reported is dropped.
func (s *Service) Refresh(ctx context.Context, ep domain.InferenceEndpoint) (Result, error) {
	defer s.lock(ep.ID)()
	res := Result{Endpoint: ep, Added: []string{}, Returned: []string{}, Retired: []string{}, Resolutions: []Resolution{}}

	project, err := s.Store.GetProject(ctx, ep.ProjectID)
	if err != nil {
		return res, err
	}
	key := ""
	if ep.APIKeySecret != "" {
		if s.Key == nil {
			return s.fail(ctx, res, project, "the secret vault is not enabled, so the endpoint's key cannot be read")
		}
		if key, err = s.Key(ctx, project, ep.APIKeySecret); err != nil {
			return s.fail(ctx, res, project, fmt.Sprintf("cannot read secret %q: %s", ep.APIKeySecret, keyProblem(err)))
		}
	}

	listed, err := s.Lister.List(ctx, Target{Kind: ep.Kind, BaseURL: ep.BaseURL, Key: key})
	if err != nil {
		return s.fail(ctx, res, project, err.Error())
	}

	inputs := make([]db.DiscoveredInput, 0, len(listed))
	for _, m := range listed {
		in := db.DiscoveredInput{Model: m.ID, DisplayName: m.DisplayName, ContextWindow: m.ContextWindow}
		if !m.Created.IsZero() {
			t := m.Created
			in.Created = &t
		}
		inputs = append(inputs, in)
	}
	firstRefresh := ep.LastRefreshAt == nil
	diff, err := s.Store.RecordInferenceRefresh(ctx, ep.ID, inputs, s.now())
	if err != nil {
		return res, err
	}
	res.Added, res.Returned, res.Retired = nonNilStrings(diff.Added), nonNilStrings(diff.Returned), nonNilStrings(diff.Retired)

	s.Store.Audit(ctx, ep.OrganizationID, ep.ProjectID, "", "inference.refresh", "inference_endpoint", ep.Name,
		map[string]any{"status": "ok", "count": len(listed)})
	// The first refresh reports a whole catalogue as new; naming each one would bury the
	// log, so only appearances after that are recorded individually.
	if !firstRefresh {
		for i, name := range res.Added {
			if i >= 25 {
				break
			}
			s.Store.Audit(ctx, ep.OrganizationID, ep.ProjectID, "", "model.discovered", "inference_model", name,
				map[string]any{"provider": ep.Name})
		}
		for i, name := range res.Retired {
			if i >= 25 {
				break
			}
			s.Store.Audit(ctx, ep.OrganizationID, ep.ProjectID, "", "model.retired", "inference_model", name,
				map[string]any{"provider": ep.Name})
		}
	}

	res.Resolutions, err = ResolveProject(ctx, s.Store, project)
	if err != nil {
		s.log().Warn("resolving selector profiles failed", "project", project.Slug, "error", err)
	}
	if fresh, gerr := s.Store.GetInferenceEndpoint(ctx, ep.ProjectID, ep.ID); gerr == nil {
		res.Endpoint = fresh
	}
	return res, nil
}

func (s *Service) fail(ctx context.Context, res Result, project domain.Project, reason string) (Result, error) {
	if len(reason) > 300 {
		reason = reason[:300]
	}
	if err := s.Store.RecordInferenceFailure(ctx, res.Endpoint.ID, reason, s.now()); err != nil {
		s.log().Warn("recording endpoint failure", "endpoint", res.Endpoint.Name, "error", err)
	}
	s.Store.Audit(ctx, project.OrganizationID, project.ID, "", "inference.refresh", "inference_endpoint",
		res.Endpoint.Name, map[string]any{"status": "error", "error": reason})
	if fresh, err := s.Store.GetInferenceEndpoint(ctx, project.ID, res.Endpoint.ID); err == nil {
		res.Endpoint = fresh
	}
	return res, errors.New(reason)
}

func keyProblem(err error) string {
	if errors.Is(err, domain.ErrNotFound) {
		return "no such secret in this project"
	}
	return "it could not be opened"
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ResolveProject points each of a project's selector profiles at the best model its endpoint
// currently serves. It reads what is already discovered and makes no network call, so it is
// also what `conductord bootstrap` runs after loading a models file.
//
// A profile whose endpoint has served nothing that matches keeps its last model: a provider
// outage or a selector that is too narrow must not leave the catalog without a model.
func ResolveProject(ctx context.Context, store *db.Store, project domain.Project) ([]Resolution, error) {
	profiles, err := store.SelectorProfiles(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	out := []Resolution{}
	candidates := map[string][]Candidate{}
	endpoints := map[string]domain.InferenceEndpoint{}
	for _, p := range profiles {
		r := Resolution{Alias: p.Alias, Harness: p.Harness, From: p.Model, To: p.Model}
		sel := *p.Select
		if p.Pinned {
			r.Note = "pinned by a person; unpin to follow discovery again"
			out = append(out, r)
			continue
		}

		ep, ok := endpoints[sel.Endpoint]
		if !ok {
			ep, err = store.GetInferenceEndpoint(ctx, project.ID, sel.Endpoint)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return out, err
			}
			endpoints[sel.Endpoint] = ep
			if ep.ID != "" {
				models, lerr := store.ListDiscoveredModels(ctx, ep.ID, false)
				if lerr != nil {
					return out, lerr
				}
				for _, m := range models {
					c := Candidate{ID: m.Model}
					if m.CreatedUpstream != nil {
						c.Created = *m.CreatedUpstream
					}
					candidates[sel.Endpoint] = append(candidates[sel.Endpoint], c)
				}
			}
		}
		switch {
		case ep.ID == "":
			r.Note = fmt.Sprintf("endpoint %q is not configured", sel.Endpoint)
		case !ep.Enabled:
			r.Note = fmt.Sprintf("endpoint %q is disabled", sel.Endpoint)
		default:
			if best, found := Choose(sel, candidates[sel.Endpoint]); !found {
				r.Note = fmt.Sprintf("endpoint %q serves no model matching %v", sel.Endpoint, sel.Match)
			} else {
				r.To = sel.Prefix + best.ID
			}
		}
		if r.To != "" && r.Note == "" {
			changed, previous, err := store.SetProfileModel(ctx, p.ID, r.To, time.Now().UTC())
			if err != nil {
				return out, err
			}
			r.Changed, r.From = changed, previous
			if changed {
				store.Audit(ctx, p.OrganizationID, project.ID, "", "model.resolved", "model_profile",
					p.Alias+"/"+p.Harness, map[string]any{"model_alias": p.Alias, "harness": p.Harness,
						"from": previous, "to": r.To})
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// RefreshAll refreshes every enabled endpoint, one at a time: a list is a handful of small
// requests, and sequential keeps outbound connections and database writes few. Errors are
// recorded on each endpoint and logged, not returned.
func (s *Service) RefreshAll(ctx context.Context) {
	endpoints, err := s.Store.ListEnabledInferenceEndpoints(ctx)
	if err != nil {
		s.log().Warn("listing inference endpoints", "error", err)
		return
	}
	for _, ep := range endpoints {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.Refresh(ctx, ep); err != nil {
			s.log().Warn("model discovery failed", "endpoint", ep.Name, "error", err)
		}
	}
}

// Run refreshes every enabled endpoint now and then every interval until ctx ends. New
// models are therefore noticed within one interval of a provider listing them, with nobody
// asking.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.RefreshAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.RefreshAll(ctx)
		}
	}
}
