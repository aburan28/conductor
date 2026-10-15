package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/delphi/hosted"
	"github.com/aburan28/conductor/internal/domain"
)

// Delphi: delegate a prompt to a frontier model through a provider batch API, at half price,
// answered minutes to hours later (docs/DELPHI.md).

func cmdDelphi(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return delphiList(ctx, args)
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "ask":
		return delphiAsk(ctx, rest)
	case "show":
		return delphiShow(ctx, rest)
	case "answer":
		return delphiAnswer(ctx, rest)
	case "list", "ls":
		return delphiList(ctx, rest)
	case "cancel":
		return delphiCancel(ctx, rest)
	case "reconcile":
		return delphiReconcile(ctx, rest)
	default:
		return fmt.Errorf("unknown delphi subcommand %q (want ask, show, answer, list, cancel, or reconcile)", sub)
	}
}

func delphiClient(project string) (*client.Client, string, error) {
	api, creds, err := mustClient()
	if err != nil {
		return nil, "", err
	}
	ref, err := projectRef(project, creds)
	if err != nil {
		return nil, "", err
	}
	return api, "/v1/projects/" + ref + "/delphi", nil
}

type labelFlag map[string]string

func (l labelFlag) String() string { return fmt.Sprint(map[string]string(l)) }
func (l labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want key=value, got %q", v)
	}
	l[k] = val
	return nil
}

func delphiAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi ask", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	var body hosted.SubmitRequest
	fs.StringVar(&body.Ref, "id", "", "your id for the request (default: generated); resubmitting the same request under it is a no-op")
	fs.StringVar(&body.Provider, "provider", "anthropic", "anthropic or openai")
	fs.StringVar(&body.Model, "model", "", "model id (anthropic default: claude-mythos-5-1; openai: required)")
	fs.StringVar(&body.System, "system", "", "system prompt / instructions")
	fs.StringVar(&body.Effort, "effort", "", "reasoning effort (anthropic: low..max)")
	fs.IntVar(&body.MaxTokens, "max-tokens", 0, "output token limit, reasoning included (default 32000)")
	fs.StringVar(&body.TaskID, "task", "", "task id to link; its timeline gets delphi.finished")
	file := fs.String("file", "", "read the prompt from a file (default: the arguments, else stdin)")
	wait := fs.Bool("wait", false, "wait for the answer and print it")
	asJSON := fs.Bool("json", false, "machine-readable output")
	labels := labelFlag{}
	fs.Var(labels, "label", "key=value tag stored with the delegation (repeatable; ids, not content)")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	switch {
	case *file != "":
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		body.Text = string(b)
	case len(positional) > 0:
		body.Text = strings.Join(positional, " ")
	default:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		body.Text = string(b)
	}
	if strings.TrimSpace(body.Text) == "" {
		return errors.New("usage: conductor delphi ask [flags] PROMPT... (or --file F, or stdin)")
	}
	if len(labels) > 0 {
		body.Labels = labels
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	var d domain.DelphiDelegation
	if err := api.Post(ctx, path, body, &d); err != nil {
		return err
	}
	if !*wait {
		if *asJSON {
			return emit(d)
		}
		fmt.Println(d.Ref)
		if d.State == domain.DelphiSubmitting {
			fmt.Fprintln(os.Stderr, "the provider's answer to the submit was lost; the server will look for the batch (see `conductor delphi show`)")
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "submitted %s to %s %s; waiting (batches take minutes to hours)\n", d.Ref, d.Provider, d.Model)
	return delphiWaitAnswer(ctx, api, path, d.Ref, *asJSON)
}

func delphiWaitAnswer(ctx context.Context, api *client.Client, path, ref string, asJSON bool) error {
	for {
		var d domain.DelphiDelegation
		if err := api.Get(ctx, path+"/"+url.PathEscape(ref), &d); err != nil {
			return err
		}
		if d.State == domain.DelphiFinished || d.State == domain.DelphiAbandoned {
			return delphiPrintAnswer(ctx, api, path, ref, asJSON)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func delphiPrintAnswer(ctx context.Context, api *client.Client, path, ref string, asJSON bool) error {
	var a hosted.Answer
	if err := api.Get(ctx, path+"/"+url.PathEscape(ref)+"/answer", &a); err != nil {
		return err
	}
	if asJSON {
		return emit(a)
	}
	switch {
	case a.Unavailable != "":
		return fmt.Errorf("%s: %s", ref, a.Unavailable)
	case a.Refusal != nil && a.Text == "":
		return fmt.Errorf("%s: the model declined (%s)", ref, strings.TrimSpace(a.Refusal.Category+" "+a.Refusal.Explanation))
	}
	fmt.Println(a.Text)
	if a.Delegation.Truncated {
		fmt.Fprintln(os.Stderr, "warning: the answer stopped at max_tokens")
	}
	return nil
}

func delphiShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi show", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor delphi show REF")
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	var d domain.DelphiDelegation
	if err := api.Get(ctx, path+"/"+url.PathEscape(positional[0]), &d); err != nil {
		return err
	}
	if *asJSON {
		return emit(d)
	}
	fmt.Printf("%s  %s %s  %s", d.Ref, d.Provider, d.Model, d.State)
	if d.Status != "" {
		fmt.Printf("/%s", d.Status)
	}
	fmt.Printf("  batch %s\n  submitted %s", orDash(d.BatchID), d.SubmittedAt.Format(time.RFC3339))
	if d.FinishedAt != nil {
		fmt.Printf(", finished %s", d.FinishedAt.Format(time.RFC3339))
	}
	fmt.Printf("\n  tokens in %d, out %d, cost $%.4f\n", d.InputTokens, d.OutputTokens, d.CostUSD)
	if d.ErrorType != "" {
		fmt.Printf("  error %s: %s\n", d.ErrorType, d.ErrorDetail)
	}
	if d.State == domain.DelphiSubmitting {
		fmt.Println("  the submit's outcome is unknown; a maintainer can run `conductor delphi reconcile`")
	}
	return nil
}

func delphiAnswer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi answer", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	wait := fs.Bool("wait", false, "wait until it is finished")
	asJSON := fs.Bool("json", false, "machine-readable output")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor delphi answer [--wait] REF")
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	if *wait {
		return delphiWaitAnswer(ctx, api, path, positional[0], *asJSON)
	}
	return delphiPrintAnswer(ctx, api, path, positional[0], *asJSON)
}

func delphiList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi list", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	state := fs.String("state", "", "only submitting, running, finished or abandoned")
	limit := fs.Int("limit", 50, "most delegations to show")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	var out struct {
		Delegations []domain.DelphiDelegation `json:"delegations"`
	}
	if err := api.Get(ctx, path+client.Query("state", *state, "limit", fmt.Sprint(*limit)), &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Delegations)
	}
	if len(out.Delegations) == 0 {
		fmt.Println("no delegations yet; try: conductor delphi ask \"...\"")
		return nil
	}
	for _, d := range out.Delegations {
		state := d.State
		if d.Status != "" {
			state += "/" + d.Status
		}
		fmt.Printf("%-34s %-9s %-24s %-20s %s\n", d.Ref, d.Provider, d.Model, state, d.SubmittedAt.Format("2006-01-02 15:04"))
	}
	return nil
}

func delphiCancel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi cancel", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("usage: conductor delphi cancel REF")
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	if err := api.Post(ctx, path+"/"+url.PathEscape(positional[0])+"/cancel", struct{}{}, nil); err != nil {
		return err
	}
	fmt.Println("canceling; the outcome is recorded when the batch ends")
	return nil
}

func delphiReconcile(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi reconcile", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	adopt := fs.String("adopt", "", "the batch id the lost submit created (from the provider console)")
	abandon := fs.Bool("abandon", false, "no batch was created; give the delegation up")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 || (*adopt == "") == !*abandon {
		return errors.New("usage: conductor delphi reconcile REF (--adopt BATCH_ID | --abandon)")
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	var d domain.DelphiDelegation
	body := map[string]any{"batch_id": *adopt, "abandon": *abandon}
	if err := api.Post(ctx, path+"/"+url.PathEscape(positional[0])+"/reconcile", body, &d); err != nil {
		return err
	}
	fmt.Printf("%s is now %s\n", d.Ref, d.State)
	return nil
}
