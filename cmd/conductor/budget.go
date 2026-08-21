package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Token budget: see where the team stands, and move headroom to whoever needs it.
//
// The design premise (DESIGN.md §13.8) is that a budget cap should bound the team, not
// strand one person: alice on vacation has a full allowance doing nothing while bob is
// blocked mid-refactor. `conductor budget share` fixes that in one command.

func cmdBudget(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return budgetShow(ctx, args)
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "show":
		return budgetShow(ctx, rest)
	case "share", "give":
		return budgetShare(ctx, rest)
	case "grants", "log":
		return budgetGrants(ctx, rest)
	default:
		return fmt.Errorf("unknown budget subcommand %q (want show, share, or grants)", sub)
	}
}

type memberBudgetView struct {
	Handle    string `json:"handle"`
	Allowance int64  `json:"allowance_tokens"`
	Spent     int64  `json:"spent_tokens"`
	SharedIn  int64  `json:"shared_in_tokens"`
	SharedOut int64  `json:"shared_out_tokens"`
	Remaining int64  `json:"remaining_tokens"`
}

func budgetShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("budget", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor budget — the team's token budget for the current 30-day window

  conductor budget
  conductor budget share rachel 500k
  conductor budget grants

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
		Policy struct {
			MonthlyUSD   float64 `json:"monthly_usd"`
			MemberTokens int64   `json:"member_tokens"`
		} `json:"policy"`
		Project struct {
			SpentUSD float64 `json:"spent_usd"`
		} `json:"project"`
		Members []memberBudgetView `json:"members"`
	}
	if err := api.Get(ctx, "/v1/projects/"+ref+"/budget", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}

	fmt.Printf("%s — token budget, rolling 30 days\n\n", ref)
	if out.Policy.MonthlyUSD > 0 {
		fmt.Printf("  project spend: $%.2f of $%.2f\n", out.Project.SpentUSD, out.Policy.MonthlyUSD)
	}
	if out.Policy.MemberTokens <= 0 {
		fmt.Println("  Per-member budgets are off. Enable them with budget.member.monthly_tokens")
		fmt.Println("  in .conductor/policies.yaml to give every member a shareable allowance.")
		return nil
	}
	fmt.Printf("  allowance: %s tokens per member\n\n", humanTokens(out.Policy.MemberTokens))
	fmt.Printf("  %-20s %10s %11s %12s %11s\n", "MEMBER", "SPENT", "SHARED IN", "SHARED OUT", "REMAINING")
	for _, m := range out.Members {
		fmt.Printf("  %-20s %10s %11s %12s %11s\n", m.Handle,
			humanTokens(m.Spent), humanTokens(m.SharedIn),
			humanTokens(m.SharedOut), humanTokens(m.Remaining))
	}
	fmt.Println("\nMove headroom to whoever needs it: conductor budget share <handle> <tokens>")
	return nil
}

func budgetShare(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("budget share", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	note := fs.String("note", "", "why, for the ledger (optional)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor budget share — give a teammate part of your token allowance

  conductor budget share rachel 500k
  conductor budget share bob 2m --note "finishing the router refactor"

Amounts accept k (thousand) and m (million) suffixes. A share is a transfer, not a loan:
to get it back, the recipient shares it back.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 2 {
		return errors.New("usage: conductor budget share <handle> <tokens>")
	}
	tokens, err := parseTokenAmount(positional[1])
	if err != nil {
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

	var result struct {
		Grant struct {
			ToHandle  string `json:"to_handle"`
			Tokens    int64  `json:"tokens"`
			CreatedAt string `json:"created_at"`
		} `json:"grant"`
		From memberBudgetView `json:"from"`
		To   memberBudgetView `json:"to"`
	}
	if err := api.Post(ctx, "/v1/projects/"+ref+"/budget/share", map[string]any{
		"to": positional[0], "tokens": tokens, "note": *note,
	}, &result); err != nil {
		return err
	}
	if *asJSON {
		return emit(result)
	}

	fmt.Printf("Shared %s tokens with %s.\n\n", humanTokens(result.Grant.Tokens), result.Grant.ToHandle)
	fmt.Printf("  you  %10s remaining\n", humanTokens(result.From.Remaining))
	fmt.Printf("  %-4s %10s remaining\n", result.To.Handle, humanTokens(result.To.Remaining))
	return nil
}

func budgetGrants(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("budget grants", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	limit := fs.Int("limit", 20, "how many recent grants to show")
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

	var out struct {
		Grants []struct {
			FromHandle string    `json:"from_handle"`
			ToHandle   string    `json:"to_handle"`
			Tokens     int64     `json:"tokens"`
			Note       string    `json:"note"`
			CreatedAt  time.Time `json:"created_at"`
		} `json:"grants"`
	}
	if err := api.Get(ctx, fmt.Sprintf("/v1/projects/%s/budget/grants?limit=%d", ref, *limit), &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Grants)
	}
	if len(out.Grants) == 0 {
		fmt.Println("No budget grants yet.")
		return nil
	}
	for _, g := range out.Grants {
		line := fmt.Sprintf("  %s  %s → %s  %s",
			g.CreatedAt.Local().Format("2006-01-02 15:04"),
			g.FromHandle, g.ToHandle, humanTokens(g.Tokens))
		if g.Note != "" {
			line += "  (" + g.Note + ")"
		}
		fmt.Println(line)
	}
	return nil
}

// parseTokenAmount reads "500000", "500k", or "2.5m" as a token count.
func parseTokenAmount(s string) (int64, error) {
	mult := int64(1)
	trimmed := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasSuffix(trimmed, "m"):
		mult, trimmed = 1_000_000, strings.TrimSuffix(trimmed, "m")
	case strings.HasSuffix(trimmed, "k"):
		mult, trimmed = 1_000, strings.TrimSuffix(trimmed, "k")
	}
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot read %q as a token amount (try 500000, 500k, or 2.5m)", s)
	}
	tokens := int64(value * float64(mult))
	if tokens <= 0 {
		return 0, fmt.Errorf("token amount must be positive, got %q", s)
	}
	return tokens, nil
}

// humanTokens renders a count the way people say it: 1234 stays exact, 500k and 2.5m round.
func humanTokens(n int64) string {
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs >= 1_000_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000_000)) + "m"
	case abs >= 10_000:
		return trimZero(fmt.Sprintf("%.1f", float64(n)/1_000)) + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
}

func trimZero(s string) string {
	return strings.TrimSuffix(s, ".0")
}
