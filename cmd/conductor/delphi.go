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
	case "profiles":
		return delphiProfiles(ctx, rest)
	default:
		return fmt.Errorf("unknown delphi subcommand %q (want ask, show, answer, list, profiles, cancel, or reconcile)", sub)
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
	fs.StringVar(&body.Profile, "profile", "", "model-catalog alias to take provider, model and effort from (see `conductor delphi profiles`)")
	fs.StringVar(&body.Provider, "provider", "", "anthropic or openai (default anthropic, or the profile's)")
	fs.StringVar(&body.Model, "model", "", "model id (anthropic default: claude-mythos-5-1; openai: required)")
	fs.StringVar(&body.System, "system", "", "system prompt / instructions")
	fs.StringVar(&body.Effort, "effort", "", "reasoning effort (anthropic: low..max)")
	fs.IntVar(&body.MaxTokens, "max-tokens", 0, "output token limit, reasoning included (default 32000)")
	fs.StringVar(&body.TaskID, "task", "", "task id to link; its timeline gets delphi.finished")
	file := fs.String("file", "", "read the prompt from a file (default: the arguments, else stdin)")
	wait := fs.Bool("wait", false, "wait for the answer and print it")
	fallback := fs.String("fallback", os.Getenv("CONDUCTOR_DELPHI_FALLBACK"),
		"with --wait: if the model refuses, resubmit to these in order (comma-separated provider:model[@effort] or profile names)")
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
	fallbackSet := false
	fs.Visit(func(f *flag.Flag) { fallbackSet = fallbackSet || f.Name == "fallback" })
	if fallbackSet && *fallback != "" && !*wait {
		return errors.New("--fallback needs --wait: the server keeps no request text, so the resubmission is made from here")
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	var d domain.DelphiDelegation
	if err := api.Post(ctx, path, body, &d); err != nil {
		return err
	}
	if *wait && *fallback != "" {
		return delphiAskWithFallback(ctx, api, path, body, d, splitList(*fallback), *asJSON)
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

// delphiPoll is how often a waiting client checks a delegation; tests shorten it.
var delphiPoll = 30 * time.Second

func delphiWaitFinished(ctx context.Context, api *client.Client, path, ref string) (domain.DelphiDelegation, error) {
	for {
		var d domain.DelphiDelegation
		if err := api.Get(ctx, path+"/"+url.PathEscape(ref), &d); err != nil {
			return d, err
		}
		if d.State == domain.DelphiFinished || d.State == domain.DelphiAbandoned {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return d, ctx.Err()
		case <-time.After(delphiPoll):
		}
	}
}

func delphiWaitAnswer(ctx context.Context, api *client.Client, path, ref string, asJSON bool) error {
	if _, err := delphiWaitFinished(ctx, api, path, ref); err != nil {
		return err
	}
	return delphiPrintAnswer(ctx, api, path, ref, asJSON)
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// delphiFallbackBody rewrites a request for the next target: "provider:model[@effort]" or a
// profile name. Params and effort are written for one provider's API, so they are dropped
// when the provider changes.
func delphiFallbackBody(body hosted.SubmitRequest, target string) hosted.SubmitRequest {
	next := body
	next.Profile, next.Model = "", ""
	prov, rest, ok := strings.Cut(target, ":")
	if !ok {
		next.Profile, next.Provider, next.Effort, next.Params = target, "", "", nil
		return next
	}
	if prov != body.Provider {
		next.Params, next.Effort = nil, ""
	}
	next.Provider = prov
	model, effort, _ := strings.Cut(rest, "@")
	next.Model = model
	if effort != "" {
		next.Effort = effort
	}
	return next
}

// delphiAskWithFallback waits for d and, while the answer is a refusal and targets remain,
// resubmits the same request to the next target as REF-fbN, recording which delegation it
// falls back from.
func delphiAskWithFallback(ctx context.Context, api *client.Client, path string, body hosted.SubmitRequest,
	d domain.DelphiDelegation, chain []string, asJSON bool) error {
	origin := d.Ref
	for n := 1; ; n++ {
		fmt.Fprintf(os.Stderr, "submitted %s to %s %s; waiting (batches take minutes to hours)\n", d.Ref, d.Provider, d.Model)
		done, err := delphiWaitFinished(ctx, api, path, d.Ref)
		if err != nil {
			return err
		}
		if done.Status != "refused" || len(chain) == 0 {
			return delphiPrintAnswer(ctx, api, path, d.Ref, asJSON)
		}
		fmt.Fprintf(os.Stderr, "%s %s refused %s (%s); falling back to %s\n", d.Provider, d.Model, d.Ref, orDash(done.RefusalCategory), chain[0])
		body = delphiFallbackBody(body, chain[0])
		chain = chain[1:]
		body.Ref, body.FallbackOf = fmt.Sprintf("%s-fb%d", origin, n), d.Ref
		var next domain.DelphiDelegation
		if err := api.Post(ctx, path, body, &next); err != nil {
			fmt.Fprintf(os.Stderr, "fallback %s: %v\n", body.Ref, err)
			if len(chain) == 0 {
				return delphiPrintAnswer(ctx, api, path, d.Ref, asJSON)
			}
			continue
		}
		d = next
	}
}

func delphiProfiles(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("delphi profiles", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	api, path, err := delphiClient(*project)
	if err != nil {
		return err
	}
	var out struct {
		Profiles []hosted.Profile `json:"profiles"`
	}
	if err := api.Get(ctx, path+"/profiles", &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	if len(out.Profiles) == 0 {
		fmt.Println("no enabled anthropic or openai profiles in the model catalog")
		return nil
	}
	for _, p := range out.Profiles {
		fmt.Printf("%-20s %-10s %-32s %-7s %s\n", p.Name, p.Provider, p.Model, orDash(p.Effort), p.Harness)
	}
	return nil
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
	fmt.Printf("  batch %s\n", orDash(d.BatchID))
	if d.Profile != "" {
		fmt.Printf("  profile %s\n", d.Profile)
	}
	if d.FallbackOf != "" {
		fmt.Printf("  fallback of %s\n", d.FallbackOf)
	}
	if d.RefusalCategory != "" {
		fmt.Printf("  refused: %s\n", d.RefusalCategory)
	}
	fmt.Printf("  submitted %s", d.SubmittedAt.Format(time.RFC3339))
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
