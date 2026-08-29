package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// ---------------------------------------------------------------------------
// models — the catalog, and discovery of what is installed locally
// ---------------------------------------------------------------------------

func cmdModels(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "discover" {
		return modelsDiscover(ctx, args[1:])
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
alias. Run `+"`conductor models discover`"+` to find models installed on this machine.

  conductor models
  conductor models discover            find local Ollama / LM Studio / vLLM models
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
		fmt.Printf("  %-18s %-10s %-26s %-5s %-8s %-10s %s\n",
			p.Alias, p.Harness, orDash(p.Model), orDash(string(p.Tier)),
			orDash(string(p.ReasoningEffort)), orDash(p.Billing), status)
	}
	return nil
}

// modelsDiscover probes the machine for locally served models and prints ready-to-paste
// dispatch candidates. It reads only model *names* from each server's list endpoint — never a
// prompt, never a response — the same posture as everywhere else.
func modelsDiscover(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("models discover", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	type found struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Via      string `json:"via"`
	}
	var models []found

	// Ollama: `ollama list` if the CLI is present.
	if _, err := exec.LookPath("ollama"); err == nil {
		if out, err := exec.CommandContext(ctx, "ollama", "list").Output(); err == nil {
			for i, line := range strings.Split(string(out), "\n") {
				if i == 0 || strings.TrimSpace(line) == "" {
					continue // header
				}
				name := strings.Fields(line)[0]
				models = append(models, found{Provider: "ollama", Model: "ollama/" + name, Via: "opencode"})
			}
		}
	}

	if *asJSON {
		return emit(models)
	}
	if len(models) == 0 {
		fmt.Println("No local models found. Install Ollama (https://ollama.com) and `ollama pull`")
		fmt.Println("a model, or add remote models to .conductor/models.yaml by hand.")
		return nil
	}
	fmt.Print("Local models found. Add to .conductor/dispatch.yaml under a lane's candidates:\n\n")
	for _, m := range models {
		fmt.Printf("    - model: %s\n      harness: %s\n      tags: [local, cheap]\n", m.Model, m.Via)
	}
	fmt.Println("\nThen declare them in .conductor/models.yaml with a tier so tier floors can be met.")
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
