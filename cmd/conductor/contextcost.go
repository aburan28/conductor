package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/adamburan/conductor/internal/tokencost"
)

// What does reading actually cost?
//
// Every agent session pays a standing context bill before it does anything: AGENTS.md,
// CLAUDE.md, workflow rules, the README. Then every file it opens is billed again. With
// member token budgets in play (`conductor budget`), those reads are no longer abstract —
// they come out of someone's window allowance. This command makes the bill visible.

func cmdContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	top := fs.Int("top", 15, "how many files to list for directory sweeps")
	exact := fs.Bool("exact", false, "count with the Anthropic tokenizer endpoint instead of estimating (uses your ANTHROPIC_API_KEY; sends content to Anthropic, never to the conductor server)")
	model := fs.String("model", "", "model whose tokenizer answers --exact (default "+tokencost.DefaultExactModel+")")
	project := fs.String("project", "", "project id or slug, for the budget comparison")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor context — what reading costs in tokens

  conductor context                          the standing context files agents load every session
  conductor context internal/db DESIGN.md    any files or directories, largest first
  conductor context --exact CLAUDE.md        the model-exact count, via your own Anthropic credentials

Estimates are local and deterministic; nothing is read by anyone but you. With member
budgets enabled, the total is also shown as a share of your remaining window allowance.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}

	var files []tokencost.FileCost
	var skipped int
	instructionMode := len(positional) == 0
	if instructionMode {
		files, err = tokencost.InstructionFiles(".")
	} else {
		files, skipped, err = collectPaths(positional)
	}
	if err != nil {
		return err
	}

	mode, tokenizer := "estimate", ""
	shown := files
	if !instructionMode && len(shown) > *top {
		shown = shown[:*top]
	}

	if *exact {
		fmt.Fprintln(os.Stderr,
			"note: --exact sends file content to the Anthropic API with your local credentials.\n"+
				"It never touches the conductor server. The endpoint counts tokens; it runs no inference.")
		counter := tokencost.NewExactCounter(*model)
		mode, tokenizer = "exact", counter.Model()
		for i := range shown {
			body, err := os.ReadFile(shown[i].Path)
			if err != nil {
				return err
			}
			n, err := counter.Count(ctx, string(body))
			if err != nil {
				return err
			}
			shown[i].Tokens = n
		}
	}

	if *asJSON {
		return emit(map[string]any{
			"mode": mode, "model": tokenizer, "files": shown,
			"total_tokens": tokencost.Total(shown), "files_total": len(files), "skipped": skipped,
		})
	}

	if instructionMode {
		renderInstructionCosts(shown, mode, tokenizer)
	} else {
		renderPathCosts(shown, len(files), skipped, mode, tokenizer)
	}
	printBudgetShare(ctx, *project, tokencost.Total(shown))
	if mode == "estimate" {
		fmt.Println("\nEstimates are tokenizer approximations (±15-20%). For a model-exact count: conductor context --exact")
	}
	return nil
}

func renderInstructionCosts(files []tokencost.FileCost, mode, model string) {
	if len(files) == 0 {
		fmt.Println("No instruction files found here (AGENTS.md, CLAUDE.md, .conductor/WORKFLOW.md, README.md, …).")
		fmt.Println("Run from the repository root, or pass paths: conductor context internal/db docs/DESIGN.md")
		return
	}
	fmt.Printf("Standing context an agent loads from this repository (%s):\n\n", modeLabel(mode, model))
	for _, f := range files {
		fmt.Printf("  %-40s %8s tokens\n", f.Path, humanTokens(f.Tokens))
	}
	fmt.Printf("\n  %-40s %8s tokens\n", "total, every session, before any work",
		humanTokens(tokencost.Total(files)))
}

func renderPathCosts(shown []tokencost.FileCost, total, skipped int, mode, model string) {
	note := ""
	if skipped > 0 {
		note = fmt.Sprintf("; %d binary/oversized skipped", skipped)
	}
	fmt.Printf("%s tokens across %d file(s) (%s%s)\n\n",
		humanTokens(tokencost.Total(shown)), len(shown), modeLabel(mode, model), note)
	for _, f := range shown {
		fmt.Printf("  %-56s %8s\n", f.Path, humanTokens(f.Tokens))
	}
	if total > len(shown) {
		fmt.Printf("\n  …and %d more file(s); raise --top to see them. Totals cover the files shown.\n",
			total-len(shown))
	}
}

func modeLabel(mode, model string) string {
	if mode == "exact" {
		return "exact, " + model
	}
	return "estimate"
}

// collectPaths estimates every argument: files directly, directories by sweep. Display
// paths keep the argument prefix so they remain valid, readable paths from the caller's
// working directory — which is also what --exact re-reads.
func collectPaths(paths []string) ([]tokencost.FileCost, int, error) {
	var out []tokencost.FileCost
	skipped := 0
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, 0, err
		}
		if !info.IsDir() {
			cost, ok, err := tokencost.File(p)
			if err != nil {
				return nil, 0, err
			}
			if !ok {
				skipped++
				continue
			}
			out = append(out, cost)
			continue
		}
		files, s, err := tokencost.Walk(p)
		if err != nil {
			return nil, 0, err
		}
		skipped += s
		for _, f := range files {
			f.Path = filepath.Join(p, f.Path)
			out = append(out, f)
		}
	}
	sortByTokens(out)
	return out, skipped, nil
}

func sortByTokens(files []tokencost.FileCost) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Tokens != files[j].Tokens {
			return files[i].Tokens > files[j].Tokens
		}
		return files[i].Path < files[j].Path
	})
}

// printBudgetShare relates the bill to the caller's member token budget, when there is one.
// This is garnish, not the meal: any failure — not logged in, budgets disabled, server
// unreachable — just means the line is not printed.
func printBudgetShare(ctx context.Context, projectOverride string, tokens int64) {
	if tokens <= 0 {
		return
	}
	api, creds, err := mustClient()
	if err != nil {
		return
	}
	ref, err := projectRef(projectOverride, creds)
	if err != nil {
		return
	}
	var who struct {
		Principal struct {
			Handle string `json:"handle"`
		} `json:"principal"`
	}
	if err := api.Get(ctx, "/v1/whoami", &who); err != nil {
		return
	}
	var budget struct {
		Policy struct {
			MemberTokens int64 `json:"member_tokens"`
		} `json:"policy"`
		Members []struct {
			Handle    string `json:"handle"`
			Remaining int64  `json:"remaining_tokens"`
		} `json:"members"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/budget", &budget); err != nil {
		return
	}
	if budget.Policy.MemberTokens <= 0 {
		return
	}
	for _, m := range budget.Members {
		if m.Handle != who.Principal.Handle {
			continue
		}
		if m.Remaining <= 0 {
			fmt.Printf("\n  Your member token budget for this window is spent (%s remaining). A teammate can help: conductor budget share\n",
				humanTokens(m.Remaining))
			return
		}
		fmt.Printf("\n  ≈ %.1f%% of your remaining member token budget (%s left this window)\n",
			100*float64(tokens)/float64(m.Remaining), humanTokens(m.Remaining))
		return
	}
}
