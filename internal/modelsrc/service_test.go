package modelsrc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

type fakeLister struct {
	models []Model
	err    error
	got    []Target
}

func (f *fakeLister) List(_ context.Context, t Target) ([]Model, error) {
	f.got = append(f.got, t)
	return f.models, f.err
}

func newFixture(t *testing.T) (context.Context, *db.Store, domain.Organization, domain.Project) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	store, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	org, err := store.CreateOrganization(ctx, "ms-org-"+suffix, "ms")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, db.CreateProjectParams{
		OrganizationID: org.ID, Slug: "app-" + suffix, DisplayName: "app", RepoPath: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, org, project
}

func profileModel(t *testing.T, ctx context.Context, store *db.Store, org domain.ID, alias string) domain.ModelProfile {
	t.Helper()
	profiles, err := store.ListModelProfiles(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.Alias == alias {
			return p
		}
	}
	t.Fatalf("no profile %s", alias)
	return domain.ModelProfile{}
}

func TestRefreshFollowsProviderReleasesWithoutAnyEdit(t *testing.T) {
	ctx, store, org, project := newFixture(t)

	ep, err := store.CreateInferenceEndpoint(ctx, domain.InferenceEndpoint{
		OrganizationID: org.ID, ProjectID: project.ID, Name: "anthropic",
		Kind: domain.InferenceAnthropic, BaseURL: "https://api.anthropic.com", Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInferenceEndpoint(ctx, ep, ""); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("duplicate endpoint name: err = %v, want ErrInvalidArgument", err)
	}

	// A profile that names what it wants, not which model it is.
	declare := domain.ModelProfile{
		OrganizationID: org.ID, Alias: "worker.general", Harness: "claude", Provider: "anthropic",
		Tier: domain.TierT2, ReasoningEffort: domain.EffortMedium, Billing: "tokens", Enabled: true,
		Select: &domain.ModelSelector{
			Endpoint: "anthropic", Match: []string{"claude-sonnet-*"}, ProjectID: project.ID,
		},
	}
	if _, err := store.UpsertModelProfile(ctx, declare); err != nil {
		t.Fatal(err)
	}

	lister := &fakeLister{models: []Model{{ID: "claude-sonnet-5-5"}, {ID: "claude-opus-5"}}}
	svc := &Service{Store: store, Lister: lister}

	// Day one.
	res, err := svc.Refresh(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 2 || res.Endpoint.ModelCount != 2 || res.Endpoint.LastStatus != "ok" {
		t.Fatalf("first refresh: %+v", res)
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.general"); got.Model != "claude-sonnet-5-5" || got.Select == nil || got.Select.ResolvedAt == nil {
		t.Fatalf("profile after first refresh: %+v", got)
	}

	// The provider ships claude-sonnet-5-6 and retires opus.
	lister.models = []Model{{ID: "claude-sonnet-5-5"}, {ID: "claude-sonnet-5-6"}}
	ep = res.Endpoint
	res, err = svc.Refresh(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0] != "claude-sonnet-5-6" || len(res.Retired) != 1 || res.Retired[0] != "claude-opus-5" {
		t.Fatalf("diff: added %v retired %v", res.Added, res.Retired)
	}
	if len(res.Resolutions) != 1 || !res.Resolutions[0].Changed || res.Resolutions[0].To != "claude-sonnet-5-6" {
		t.Fatalf("resolutions: %+v", res.Resolutions)
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.general"); got.Model != "claude-sonnet-5-6" {
		t.Fatalf("profile did not follow the release: %q", got.Model)
	}
	live, _ := store.ListDiscoveredModels(ctx, ep.ID, false)
	all, _ := store.ListDiscoveredModels(ctx, ep.ID, true)
	if len(live) != 2 || len(all) != 3 {
		t.Fatalf("live %d, all %d: retired models should be kept, marked missing", len(live), len(all))
	}

	// The endpoint goes down. Nothing is retired and the catalog keeps its model.
	lister.err = errors.New("HTTP 503 from the endpoint's model list")
	res, err = svc.Refresh(ctx, res.Endpoint)
	if err == nil || res.Endpoint.LastStatus != "error" || res.Endpoint.LastError == "" {
		t.Fatalf("outage: err %v endpoint %+v", err, res.Endpoint)
	}
	if live, _ = store.ListDiscoveredModels(ctx, ep.ID, false); len(live) != 2 {
		t.Fatalf("an outage retired models: %d live", len(live))
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.general"); got.Model != "claude-sonnet-5-6" {
		t.Fatalf("an outage changed the profile to %q", got.Model)
	}

	// The provider now lists nothing that fits: keep the last good model and say why.
	lister.err = nil
	lister.models = []Model{{ID: "claude-haiku-5"}}
	res, err = svc.Refresh(ctx, res.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Resolutions[0]; r.Changed || r.Note == "" || r.To != "claude-sonnet-5-6" {
		t.Fatalf("no-match resolution: %+v", r)
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.general"); got.Model != "claude-sonnet-5-6" {
		t.Fatalf("profile lost its model: %q", got.Model)
	}

	// Re-running bootstrap re-declares the profile without a model; the resolved one stays.
	if _, err := store.UpsertModelProfile(ctx, declare); err != nil {
		t.Fatal(err)
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.general"); got.Model != "claude-sonnet-5-6" {
		t.Fatalf("bootstrap clobbered the resolved model: %q", got.Model)
	}

	// Declaring a fixed model again turns selection off.
	fixed := declare
	fixed.Select, fixed.Model = nil, "claude-pinned"
	if _, err := store.UpsertModelProfile(ctx, fixed); err != nil {
		t.Fatal(err)
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.general"); got.Model != "claude-pinned" || got.Select != nil {
		t.Fatalf("pinning: %+v", got)
	}
}

func TestRefreshOpensTheKeyFromTheVaultAndNeverStoresIt(t *testing.T) {
	ctx, store, org, project := newFixture(t)
	ep, err := store.CreateInferenceEndpoint(ctx, domain.InferenceEndpoint{
		OrganizationID: org.ID, ProjectID: project.ID, Name: "openai", Kind: domain.InferenceOpenAI,
		BaseURL: "https://api.openai.com/v1", APIKeySecret: "OPENAI_API_KEY", Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	lister := &fakeLister{models: []Model{{ID: "gpt-x"}}}
	svc := &Service{Store: store, Lister: lister,
		Key: func(_ context.Context, p domain.Project, name string) (string, error) {
			if p.ID != project.ID || name != "OPENAI_API_KEY" {
				return "", domain.ErrNotFound
			}
			return "sk-from-vault", nil
		}}
	if _, err := svc.Refresh(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if len(lister.got) != 1 || lister.got[0].Key != "sk-from-vault" || lister.got[0].Kind != domain.InferenceOpenAI {
		t.Fatalf("target = %+v", lister.got)
	}

	// A missing secret is reported by name on the endpoint, without the lister being called.
	svc.Key = func(context.Context, domain.Project, string) (string, error) { return "", domain.ErrNotFound }
	res, err := svc.Refresh(ctx, ep)
	if err == nil || res.Endpoint.LastStatus != "error" || len(lister.got) != 1 {
		t.Fatalf("missing secret: err %v, endpoint %+v, calls %d", err, res.Endpoint, len(lister.got))
	}

	// Nothing about the key, or any response, is on the endpoint row that the API lists.
	listed, _ := store.ListInferenceEndpoints(ctx, project.ID)
	if len(listed) != 1 || listed[0].APIKeySecret != "OPENAI_API_KEY" {
		t.Fatalf("listed = %+v", listed)
	}
}

func TestResolveProjectReportsUnconfiguredAndDisabledEndpoints(t *testing.T) {
	ctx, store, org, project := newFixture(t)
	if _, err := store.UpsertModelProfile(ctx, domain.ModelProfile{
		OrganizationID: org.ID, Alias: "worker.fast", Harness: "opencode", Model: "vllm/old",
		Tier: domain.TierT1, ReasoningEffort: domain.EffortLow, Billing: "capacity", Enabled: true,
		Select: &domain.ModelSelector{Endpoint: "vllm", Match: []string{"*"}, Prefix: "vllm/", ProjectID: project.ID},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := ResolveProject(ctx, store, project)
	if err != nil || len(out) != 1 || out[0].Changed || out[0].Note == "" {
		t.Fatalf("no endpoint: %+v err %v", out, err)
	}

	ep, err := store.CreateInferenceEndpoint(ctx, domain.InferenceEndpoint{
		OrganizationID: org.ID, ProjectID: project.ID, Name: "vllm", Kind: domain.InferenceOpenAICompatible,
		BaseURL: "http://localhost:8000/v1", Enabled: true,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: store, Lister: &fakeLister{models: []Model{{ID: "zai-org/GLM-5.3-Flash"}}}}
	res, err := svc.Refresh(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if got := profileModel(t, ctx, store, org.ID, "worker.fast"); got.Model != "vllm/zai-org/GLM-5.3-Flash" {
		t.Fatalf("prefix not applied: %q (resolutions %+v)", got.Model, res.Resolutions)
	}

	if _, err := store.UpdateInferenceEndpoint(ctx, project.ID, ep.ID, ep.BaseURL, "", false); err != nil {
		t.Fatal(err)
	}
	out, _ = ResolveProject(ctx, store, project)
	if len(out) != 1 || out[0].Note == "" {
		t.Fatalf("disabled endpoint should be reported: %+v", out)
	}
}
