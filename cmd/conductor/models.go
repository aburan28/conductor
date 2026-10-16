package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/aburan28/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// models — the catalog, and discovery of what is installed locally
// ---------------------------------------------------------------------------

func cmdModels(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "discover":
			return modelsDiscover(ctx, args[1:])
		case "endpoints", "endpoint":
			return modelsEndpoints(ctx, args[1:])
		case "available":
			return modelsAvailable(ctx, args[1:])
		case "pin":
			return modelsPin(ctx, args[1:], true)
		case "unpin":
			return modelsPin(ctx, args[1:], false)
		}
	}
	return modelsList(ctx, args)
}

func modelsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor models — the model catalog this project routes to

Shows every model profile the organization has declared: its alias, tier, reasoning effort,
harness, and cost. Dispatch policy (.conductor/dispatch.yaml) references these by model id or
alias. A profile marked "auto" follows an inference endpoint: its model is whatever the newest
matching one is, discovered rather than typed.

  conductor models
  conductor models endpoints           the inference APIs models are discovered from
  conductor models endpoints add anthropic|openai|ollama|vllm|lmstudio|NAME
  conductor models available [--new]   what those endpoints serve right now
  conductor models pin ALIAS HARNESS MODEL    choose a model yourself; discovery leaves it alone
  conductor models unpin ALIAS HARNESS        hand the profile back to discovery
  conductor models discover            find Ollama / LM Studio / vLLM on this machine
  conductor serve qwen|flash|glm53     launch local vLLM for OpenCode

Flags:
`)
		fs.PrintDefaults()
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
	var out struct {
		Profiles []domain.ModelProfile `json:"profiles"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/models", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Profiles)
	}
	if len(out.Profiles) == 0 {
		fmt.Println("No models in the catalog. Add them to .conductor/models.yaml and re-run")
		fmt.Println("`conductord bootstrap`, or `conductor models discover` to find local ones.")
		return nil
	}
	sort.SliceStable(out.Profiles, func(i, j int) bool {
		if out.Profiles[i].Alias != out.Profiles[j].Alias {
			return out.Profiles[i].Alias < out.Profiles[j].Alias
		}
		return out.Profiles[i].Harness < out.Profiles[j].Harness
	})
	fmt.Printf("  %-18s %-10s %-26s %-5s %-8s %-10s %s\n",
		"ALIAS", "HARNESS", "MODEL", "TIER", "EFFORT", "BILLING", "STATUS")
	for _, p := range out.Profiles {
		status := "enabled"
		if !p.Enabled {
			status = "disabled"
		}
		model := orDash(p.Model)
		if p.Select != nil {
			model = firstNonEmptyString(p.Model, "(awaiting discovery)")
			status += " · auto " + p.Select.Endpoint
		}
		fmt.Printf("  %-18s %-10s %-26s %-5s %-8s %-10s %s\n",
			p.Alias, p.Harness, model, orDash(string(p.Tier)),
			orDash(string(p.ReasoningEffort)), orDash(p.Billing), status)
	}
	return nil
}

// modelsDiscover probes the machine for locally served models and prints ready-to-paste
// dispatch candidates. It reads only model *names* from each server's list endpoint — never a
// prompt, never a response — the same posture as everywhere else. With --register it also
// adds each server it found as an inference endpoint, so the control plane keeps listing it.
func modelsDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models discover", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	register := fs.Bool("register", false, "add each server found as an inference endpoint of the project")
	project := fs.String("project", "", "project id or slug (with --register)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	found := probeLocal(ctx)

	if *register && len(found) > 0 {
		api, creds, err := mustClient()
		if err != nil {
			return err
		}
		ref, err := projectRef(*project, creds)
		if err != nil {
			return err
		}
		for i := range found {
			var out endpointOut
			err := api.Post(ctx, "/v1/projects/"+ref+"/inference/endpoints", map[string]any{"preset": found[i].Preset}, &out)
			switch {
			case err == nil:
				found[i].Added = true
			case errors.Is(err, domain.ErrInvalidArgument) || strings.Contains(err.Error(), "invalid_argument"):
				found[i].AddNote = "already registered, or refused by the control plane"
			default:
				found[i].AddNote = err.Error()
			}
		}
	}

	if *asJSON {
		return emit(found)
	}
	if len(found) == 0 {
		fmt.Println("No local model servers answered (looked for Ollama :11434, LM Studio :1234, vLLM :8000).")
		fmt.Println("Start one, or add a hosted provider: `conductor models endpoints add anthropic`.")
		return nil
	}
	for _, f := range found {
		fmt.Printf("%s at %s — %d models\n", f.Preset, f.URL, len(f.Models))
		for _, m := range f.Models {
			fmt.Printf("    %s\n", m)
		}
		switch {
		case f.Added:
			fmt.Printf("  registered as endpoint %q.\n", f.Preset)
		case f.AddNote != "":
			fmt.Printf("  not registered: %s\n", f.AddNote)
		}
	}
	if !*register {
		fmt.Println("\nTo keep these discovered as they change (no model ids in models.yaml):")
		fmt.Println("  conductor models discover --register")
		fmt.Println("  then, in .conductor/models.yaml, give a profile")
		fmt.Println("    select: {endpoint: ollama, match: [\"qwen*\"], prefix: \"ollama/\"}")
		fmt.Println("(The control plane must be able to reach these addresses; on another machine, use its URL.)")
	}
	return nil
}

// ---------------------------------------------------------------------------
// policy lint
// ---------------------------------------------------------------------------

func cmdPolicy(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "lint" {
		return errors.New("usage: conductor policy lint [--dir .]")
	}
	return policyLint(args[1:])
}
