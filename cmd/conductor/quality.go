package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/quality"
)

// quality.lint — advisory findings on a public task envelope (docs/QUALITY_LINT.md).

func cmdQuality(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: conductor quality lint [--summary S] [--task T] …")
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "lint":
		return qualityLint(ctx, rest)
	default:
		return fmt.Errorf("unknown quality subcommand %q (want lint)", sub)
	}
}

func qualityLint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("quality lint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	project := fs.String("project", "", "project id or slug")
	summary := fs.String("summary", "", "one-sentence intent")
	objective := fs.String("objective", "", "short success condition")
	task := fs.String("task", "", "optional task ref to load")
	asJSON := fs.Bool("json", false, "machine-readable output")
	var labels, criteria, scopes stringList
	fs.Var(&labels, "label", "label (repeatable)")
	fs.Var(&criteria, "criterion", "acceptance criterion text (repeatable)")
	fs.Var(&scopes, "scope", "resource to include (repeatable), e.g. path:FILE or dir:DIR")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if *summary == "" && len(positional) > 0 {
		*summary = strings.Join(positional, " ")
	}

	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}

	acs := make([]domain.AcceptanceCriterion, 0, len(criteria))
	for _, c := range criteria {
		acs = append(acs, domain.AcceptanceCriterion{Text: c})
	}
	sc := make([]domain.ScopeRequest, 0, len(scopes))
	for _, s := range scopes {
		sc = append(sc, domain.ScopeRequest{Resource: s, Mode: domain.ModeWriteExclusive})
	}
	body := map[string]any{
		"summary":             *summary,
		"objective":           *objective,
		"acceptance_criteria": acs,
		"scopes":              sc,
		"labels":              []string(labels),
		"task":                *task,
	}
	var out quality.Result
	if err := api.Post(ctx, "/v1/projects/"+ref+"/quality/lint", body, &out); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("advisory=%v engine=%s findings=%d\n", out.Advisory, out.Engine, len(out.Findings))
	for _, f := range out.Findings {
		fmt.Printf("%-7s %-28s %s: %s\n", f.Severity, f.Code, f.Where, f.Message)
	}
	return nil
}
