package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/modelsrc"
)

// ---------------------------------------------------------------------------
// models endpoints / available — where models come from, and what they serve right now
// ---------------------------------------------------------------------------

const modelsEndpointsUsage = `conductor models endpoints — the inference APIs this project routes to

An endpoint is an API Conductor asks "which models do you serve?". It reads model names only,
using a key held in the project secret vault. New releases then show up in the catalog by
themselves: a profile in .conductor/models.yaml that says

    select: {endpoint: anthropic, match: ["claude-sonnet-*"]}

follows the newest matching model, with nobody editing a model id.

  conductor models endpoints                       list endpoints and their last refresh
  conductor models endpoints add anthropic         from a preset (see the list below)
  conductor models endpoints add gw --kind openai_compatible --url https://llm.corp/v1 --key-secret GATEWAY_KEY
  conductor models endpoints set NAME [--url U] [--key-secret S | --no-key] [--enable | --disable]
  conductor models endpoints refresh [NAME]        ask now (default: all)
  conductor models endpoints rm NAME
  conductor models available [--endpoint NAME]     the models endpoints serve, newest first

Presets: %s.
Kinds: anthropic, openai, openai_compatible (vLLM, LM Studio, OpenRouter, a gateway), ollama.
`

func presetNames() string {
	names := make([]string, 0, len(modelsrc.Presets))
	for _, p := range modelsrc.Presets {
		names = append(names, p.Name)
	}
	return strings.Join(names, ", ")
}

func modelsEndpoints(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return endpointsList(ctx, nil)
	}
	switch args[0] {
	case "add":
		return endpointsAdd(ctx, args[1:])
	case "set", "edit":
		return endpointsSet(ctx, args[1:])
	case "refresh":
		return endpointsRefresh(ctx, args[1:])
	case "rm", "remove", "delete":
		return endpointsRemove(ctx, args[1:])
	case "list", "ls":
		return endpointsList(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Printf(modelsEndpointsUsage, presetNames())
		return nil
	default:
		if strings.HasPrefix(args[0], "-") {
			return endpointsList(ctx, args)
		}
		return fmt.Errorf("unknown subcommand %q; try `conductor models endpoints --help`", args[0])
	}
}

type endpointsOut struct {
	Endpoints []domain.InferenceEndpoint `json:"endpoints"`
	Presets   []modelsrc.Preset          `json:"presets"`
}

func endpointsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models endpoints", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() { fmt.Fprintf(os.Stderr, modelsEndpointsUsage, presetNames()); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	var out endpointsOut
	if err := api.Get(ctx, "/v1/projects/"+ref+"/inference/endpoints", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Endpoints)
	}
	if len(out.Endpoints) == 0 {
		fmt.Println("No inference endpoints. Add one so models are discovered instead of typed:")
		fmt.Println()
		fmt.Println("  conductor models endpoints add anthropic       (key from the ANTHROPIC_API_KEY secret)")
		fmt.Println("  conductor models endpoints add ollama          (a local Ollama)")
		fmt.Printf("\nPresets: %s.\n", presetNames())
		return nil
	}
	fmt.Printf("  %-14s %-18s %-7s %-6s %-16s %s\n", "NAME", "KIND", "MODELS", "STATE", "REFRESHED", "ADDRESS")
	for _, e := range out.Endpoints {
		state := e.LastStatus
		if !e.Enabled {
			state = "off"
		}
		fmt.Printf("  %-14s %-18s %-7d %-6s %-16s %s\n", e.Name, e.Kind, e.ModelCount, state,
			ago(e.LastRefreshAt), e.BaseURL)
		if e.LastStatus == "error" && e.LastError != "" {
			fmt.Printf("  %-14s ↳ %s\n", "", e.LastError)
		}
	}
	return nil
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

type endpointOut struct {
	Endpoint     domain.InferenceEndpoint `json:"endpoint"`
	Refresh      *modelsrc.Result         `json:"refresh,omitempty"`
	RefreshError string                   `json:"refresh_error,omitempty"`
}

func endpointsAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models endpoints add", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	kind := fs.String("kind", "", "anthropic, openai, openai_compatible or ollama (default: from the preset)")
	addr := fs.String("url", "", "API address (default: from the preset)")
	keySecret := fs.String("key-secret", "", "name of the project secret that holds the API key (default: the preset's usual name, if the secret exists)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: conductor models endpoints add NAME [--kind K --url URL --key-secret SECRET]\n\n")
		fs.PrintDefaults()
	}
	// The name comes first; flags may follow it.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		fs.Usage()
		return errors.New("an endpoint name is required")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	body := map[string]any{"name": name}
	if _, isPreset := modelsrc.PresetByName(name); isPreset {
		body["preset"] = name
	} else if *kind == "" || *addr == "" {
		return fmt.Errorf("%q is not a preset (%s); give --kind and --url", name, presetNames())
	}
	if *kind != "" {
		body["kind"] = *kind
	}
	if *addr != "" {
		body["base_url"] = *addr
	}
	if *keySecret != "" {
		body["api_key_secret"] = *keySecret
	}
	var out endpointOut
	if err := api.Post(ctx, "/v1/projects/"+ref+"/inference/endpoints", body, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	fmt.Printf("Added %s (%s) at %s\n", out.Endpoint.Name, out.Endpoint.Kind, out.Endpoint.BaseURL)
	if out.Endpoint.APIKeySecret != "" {
		fmt.Printf("  key: project secret %s\n", out.Endpoint.APIKeySecret)
	} else if p, ok := modelsrc.PresetByName(name); ok && p.KeyHint != "" {
		fmt.Printf("  no key yet: `conductor secrets set %s`, then `conductor models endpoints refresh %s`\n", p.KeyHint, name)
	}
	return printRefresh(out)
}

func printRefresh(out endpointOut) error {
	switch {
	case out.RefreshError != "":
		fmt.Printf("  could not list models: %s\n", out.RefreshError)
	case out.Refresh != nil:
		fmt.Printf("  %d models served.\n", out.Endpoint.ModelCount)
		for _, name := range out.Refresh.Added {
			if len(out.Refresh.Added) <= 12 {
				fmt.Printf("    + %s\n", name)
			}
		}
		for _, name := range out.Refresh.Returned {
			fmt.Printf("    ↺ %s (listed again)\n", name)
		}
		for _, name := range out.Refresh.Retired {
			fmt.Printf("    - %s (no longer listed)\n", name)
		}
		for _, r := range out.Refresh.Resolutions {
			switch {
			case r.Changed:
				fmt.Printf("  %s/%s: %s → %s\n", r.Alias, r.Harness, orDash(r.From), r.To)
			case r.Note != "":
				fmt.Printf("  %s/%s kept %s: %s\n", r.Alias, r.Harness, orDash(r.From), r.Note)
			}
		}
	}
	return nil
}

func endpointsRefresh(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models endpoints refresh", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	names := []string{name}
	if name == "" {
		var list endpointsOut
		if err := api.Get(ctx, "/v1/projects/"+ref+"/inference/endpoints", &list); err != nil {
			return err
		}
		names = names[:0]
		for _, e := range list.Endpoints {
			if e.Enabled {
				names = append(names, e.Name)
			}
		}
		if len(names) == 0 {
			fmt.Println("No enabled endpoints to refresh.")
			return nil
		}
	}
	var all []endpointOut
	failed := 0
	for _, n := range names {
		var out endpointOut
		if err := api.Post(ctx, "/v1/projects/"+ref+"/inference/endpoints/"+url.PathEscape(n)+"/refresh", nil, &out); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		all = append(all, out)
		if out.RefreshError != "" {
			failed++
		}
		if !*asJSON {
			fmt.Printf("%s:\n", n)
			_ = printRefresh(out)
		}
	}
	if *asJSON {
		return emit(all)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d endpoints could not be listed", failed, len(names))
	}
	return nil
}

func endpointsSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models endpoints set", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	addr := fs.String("url", "", "new API address")
	keySecret := fs.String("key-secret", "", "name of the project secret that holds the API key")
	noKey := fs.Bool("no-key", false, "the endpoint needs no key")
	enable := fs.Bool("enable", false, "include the endpoint in refreshes")
	disable := fs.Bool("disable", false, "stop refreshing the endpoint (its models stay)")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		return errors.New("usage: conductor models endpoints set NAME [--url U] [--key-secret S | --no-key] [--enable | --disable]")
	}
	if *enable && *disable || *noKey && *keySecret != "" {
		return errors.New("conflicting flags")
	}
	body := map[string]any{}
	if *addr != "" {
		body["base_url"] = *addr
	}
	if *keySecret != "" {
		body["api_key_secret"] = *keySecret
	}
	if *noKey {
		body["api_key_secret"] = ""
	}
	if *enable {
		body["enabled"] = true
	}
	if *disable {
		body["enabled"] = false
	}
	if len(body) == 0 {
		return errors.New("nothing to change")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	var out endpointOut
	if err := api.Put(ctx, "/v1/projects/"+ref+"/inference/endpoints/"+url.PathEscape(name), body, &out); err != nil {
		return err
	}
	fmt.Printf("Updated %s. Run `conductor models endpoints refresh %s` to list its models now.\n", name, name)
	return nil
}

func endpointsRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models endpoints rm", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if name == "" {
		return errors.New("usage: conductor models endpoints rm NAME")
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	if err := api.Delete(ctx, "/v1/projects/"+ref+"/inference/endpoints/"+url.PathEscape(name), nil); err != nil {
		return err
	}
	fmt.Printf("Removed %s. Profiles that selected from it keep the model they last resolved to.\n", name)
	return nil
}

type availableOut struct {
	Models []struct {
		Endpoint    string               `json:"endpoint"`
		Kind        domain.InferenceKind `json:"kind"`
		Model       string               `json:"model"`
		DisplayName string               `json:"display_name"`
		Released    *time.Time           `json:"released"`
		New         bool                 `json:"new"`
		UsedBy      []string             `json:"used_by"`
		Missing     bool                 `json:"missing"`
	} `json:"models"`
}

func modelsAvailable(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models available", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	endpoint := fs.String("endpoint", "", "only this endpoint")
	onlyNew := fs.Bool("new", false, "only models first seen in the last week")
	missing := fs.Bool("missing", false, "include models an endpoint no longer lists")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	q := url.Values{}
	if *endpoint != "" {
		q.Set("endpoint", *endpoint)
	}
	if *missing {
		q.Set("missing", "1")
	}
	path := "/v1/projects/" + ref + "/inference/models"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out availableOut
	if err := api.Get(ctx, path, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Models)
	}
	rows := 0
	fmt.Printf("  %-12s %-44s %-12s %s\n", "ENDPOINT", "MODEL", "RELEASED", "")
	for _, m := range out.Models {
		if *onlyNew && !m.New {
			continue
		}
		note := strings.Join(m.UsedBy, ", ")
		if note != "" {
			note = "← " + note
		}
		if m.New {
			note = strings.TrimSpace("new " + note)
		}
		if m.Missing {
			note = "no longer listed"
		}
		released := "—"
		if m.Released != nil {
			released = m.Released.Format("2006-01-02")
		}
		fmt.Printf("  %-12s %-44s %-12s %s\n", m.Endpoint, m.Model, released, note)
		rows++
	}
	if rows == 0 {
		fmt.Println("  (none) — add an endpoint with `conductor models endpoints add`.")
	}
	return nil
}

// modelsPin pins (or unpins) the model of one catalog profile.
func modelsPin(ctx context.Context, args []string, pin bool) error {
	fs := flag.NewFlagSet("models pin", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	// Positionals come first and flags after, as elsewhere in this CLI.
	want := 2
	usage := "usage: conductor models unpin ALIAS HARNESS"
	if pin {
		want, usage = 3, "usage: conductor models pin ALIAS HARNESS MODEL"
	}
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") && len(pos) < want {
		pos, args = append(pos, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(pos) != want {
		return errors.New(usage)
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	path := "/v1/projects/" + ref + "/models/profiles/" + url.PathEscape(pos[0]) + "/" + url.PathEscape(pos[1]) + "/pin"
	var out struct {
		Profile domain.ModelProfile `json:"profile"`
	}
	if pin {
		if err := api.Put(ctx, path, map[string]any{"model": pos[2]}, &out); err != nil {
			return err
		}
		fmt.Printf("%s/%s is pinned to %s. Discovery and bootstrap will not change it; `conductor models unpin %s %s` releases it.\n",
			pos[0], pos[1], out.Profile.Model, pos[0], pos[1])
		return nil
	}
	if err := api.Delete(ctx, path, &out); err != nil {
		return err
	}
	if out.Profile.Select != nil {
		fmt.Printf("%s/%s follows %s again: %s.\n", pos[0], pos[1], out.Profile.Select.Endpoint, orDash(out.Profile.Model))
	} else {
		fmt.Printf("%s/%s is unpinned; the models file decides at the next bootstrap (now %s).\n", pos[0], pos[1], orDash(out.Profile.Model))
	}
	return nil
}

// ---------------------------------------------------------------------------
// models discover — find model servers on this machine
// ---------------------------------------------------------------------------

// localProbes are the local presets, in the order they are tried.
func localProbes() []modelsrc.Preset {
	var out []modelsrc.Preset
	for _, p := range modelsrc.Presets {
		if p.Local {
			out = append(out, p)
		}
	}
	return out
}

type foundServer struct {
	Preset  string   `json:"preset"`
	Kind    string   `json:"kind"`
	URL     string   `json:"url"`
	Models  []string `json:"models"`
	Added   bool     `json:"added,omitempty"`
	AddNote string   `json:"note,omitempty"`
}

// probeLocal lists the models of every local server that answers. It reads names only.
func probeLocal(ctx context.Context) []foundServer {
	lister := modelsrc.NewHTTPLister()
	var out []foundServer
	for _, p := range localProbes() {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		models, err := lister.List(pctx, modelsrc.Target{Kind: p.Kind, BaseURL: p.BaseURL})
		cancel()
		if err != nil || len(models) == 0 {
			continue
		}
		f := foundServer{Preset: p.Name, Kind: string(p.Kind), URL: p.BaseURL}
		for _, m := range models {
			f.Models = append(f.Models, m.ID)
		}
		sort.Strings(f.Models)
		out = append(out, f)
	}
	return out
}
