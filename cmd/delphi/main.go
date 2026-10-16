// delphi delegates hard prompts to frontier models through provider batch
// APIs (Anthropic, OpenAI) and runs the loop around them: queue, flush as
// batches, poll, collect, retry, hand results on.
//
//	export ANTHROPIC_API_KEY=...   # and/or OPENAI_API_KEY
//	delphi ask -prompt "..."       # queue one request, prints its id
//	delphi serve                   # the loop: flush, poll, collect, forever
//	delphi get -wait ID            # the answer, once it is in
//	delphi run -in jobs.jsonl -out runs/x   # one-shot: queue a file, loop until done
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	openaisdk "github.com/openai/openai-go/v3"

	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/delphi/anthropic"
	"github.com/aburan28/conductor/internal/delphi/openai"
)

const (
	exitOK          = 0
	exitFailures    = 1 // finished, but not every request succeeded
	exitUsage       = 2
	exitAPI         = 3
	exitInterrupted = 130
)

const usage = `delphi: delegate prompts to frontier models through provider batch APIs.

queue and answers:
  ask        queue one prompt (-prompt TEXT, -file F, or stdin); prints its id
  enqueue    queue a JSONL file of requests (-in F, "-" for stdin)
  get        print a finished result (-wait to block, -text for the answer only)
  results    print every finished result as JSONL

the loop:
  serve      run the loop: flush the queue as batches, poll, collect, retry
  run        one-shot: queue -in F into -out DIR and loop until all are answered

operations:
  status     queue, flights and results at a glance
  cancel     cancel in-flight batches (-label L or -all)
  reconcile  list or resolve flights whose submit outcome is unknown
  providers  which providers have credentials
  profiles   the named tiers in the profiles file
  validate   check a JSONL file locally (no key, no cost)

every command takes -home DIR (default $DELPHI_HOME or ~/.delphi).
run "delphi <command> -h" for its flags; README.md has the input format.

environment: ANTHROPIC_API_KEY, OPENAI_API_KEY (one or both); DELPHI_PROVIDER
(default anthropic); DELPHI_ANTHROPIC_MODEL (default claude-mythos-5-1);
DELPHI_OPENAI_MODEL (no default); DELPHI_MAX_TOKENS (default 32000);
DELPHI_OPENAI_PRICES (JSON {"model":[in,out]} batch $/MTok for cost estimates).

tiers and fallback: -profile NAME takes provider, model, effort and max_tokens from
$DELPHI_PROFILES (default ~/.delphi/profiles.json), a JSON object such as
  {"implementer": {"provider": "anthropic", "model": "claude-haiku-5-5", "effort": "max"},
   "frontier": {"provider": "anthropic", "model": "claude-mythos-5-1",
                "fallback": ["openai:MODEL@high", "anthropic:claude-opus-5-5"]}}
A request whose model refuses it is resent, same id, to the next fallback in order
(-fallback, $DELPHI_FALLBACK, the profile's, or a row's "fallback"); the result lists the
models that declined in fallback_from.
`

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (a *app) logf(format string, args ...any) {
	fmt.Fprintf(a.stdout, "[%s] %s\n", time.Now().UTC().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func defaultHome() string {
	if h := os.Getenv("DELPHI_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".delphi")
	}
	return ".delphi"
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// offline can validate requests for every provider without credentials.
func offline() delphi.Registry {
	return delphi.Registry{"anthropic": anthropic.Offline(), "openai": openai.Offline()}
}

// live holds the providers that have credentials; missing lists the others.
func live() (delphi.Registry, map[string]error) {
	reg, missing := delphi.Registry{}, map[string]error{}
	if p, err := anthropic.New(); err == nil {
		reg["anthropic"] = p
	} else {
		missing["anthropic"] = err
	}
	if p, err := openai.New(); err == nil {
		reg["openai"] = p
	} else {
		missing["openai"] = err
	}
	return reg, missing
}

func statusOf(err error) int {
	var ae *anthropicsdk.Error
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	var oe *openaisdk.Error
	if errors.As(err, &oe) {
		return oe.StatusCode
	}
	return 0
}

func describe(err error) string {
	if s, ok := anthropic.Describe(err); ok {
		return s
	}
	if s, ok := openai.Describe(err); ok {
		return s
	}
	return "error: " + err.Error()
}

type requestFlags struct {
	fs       *flag.FlagSet
	d        delphi.Defaults
	meta     multiFlag
	profile  string
	fallback string
}

func defaultProfiles() string {
	if p := os.Getenv("DELPHI_PROFILES"); p != "" {
		return p
	}
	return filepath.Join(defaultHome(), "profiles.json")
}

// defaults resolves -profile against the profiles file. Flags given on the command line
// override the profile; the profile overrides the flags' defaults.
func (rf *requestFlags) defaults() (delphi.Defaults, error) {
	d := rf.d
	profiles, err := delphi.LoadProfiles(defaultProfiles())
	if err != nil {
		return d, err
	}
	d.Profiles = profiles
	set := map[string]bool{}
	rf.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if rf.profile != "" {
		pr, ok := profiles[rf.profile]
		if !ok {
			return d, fmt.Errorf("profile %q is not in %s (have %v)", rf.profile, defaultProfiles(), profiles.Names())
		}
		d.Profile = rf.profile
		if pr.Provider != "" && !set["provider"] {
			if pr.Provider != d.Provider && !set["model"] {
				d.Model = ""
			}
			d.Provider = pr.Provider
		}
		if pr.Model != "" && !set["model"] {
			d.Model = pr.Model
		}
		if pr.Effort != "" && !set["effort"] {
			d.Effort = pr.Effort
		}
		if pr.MaxTokens > 0 && !set["max-tokens"] {
			d.MaxTokens = pr.MaxTokens
		}
		d.Fallback = pr.Fallback
	}
	if set["fallback"] || len(d.Fallback) == 0 {
		d.Fallback = delphi.SplitList(rf.fallback)
	}
	return d, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func addRequestFlags(fs *flag.FlagSet) *requestFlags {
	rf := &requestFlags{fs: fs}
	prov := os.Getenv("DELPHI_PROVIDER")
	if prov == "" {
		prov = "anthropic"
	}
	fs.StringVar(&rf.d.Provider, "provider", prov, "anthropic or openai")
	fs.StringVar(&rf.d.Model, "model", "", "model id (default: the provider's default)")
	fs.IntVar(&rf.d.MaxTokens, "max-tokens", envInt("DELPHI_MAX_TOKENS", 32000), "output token limit (reasoning counts toward it)")
	fs.StringVar(&rf.d.Effort, "effort", "", "reasoning effort (anthropic: low..max; openai: as the model accepts)")
	fs.StringVar(&rf.d.System, "system", "", "system prompt / instructions")
	fs.StringVar(&rf.profile, "profile", os.Getenv("DELPHI_PROFILE"), "named tier from the profiles file ($DELPHI_PROFILES or ~/.delphi/profiles.json)")
	fs.StringVar(&rf.fallback, "fallback", os.Getenv("DELPHI_FALLBACK"),
		"where a refused request goes next: comma-separated provider:model[@effort] or profile names")
	return rf
}

func newFS(name string, a *app) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("delphi "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	home := fs.String("home", defaultHome(), "state directory")
	return fs, home
}

func (a *app) fail(err error) int {
	var re *delphi.RequestError
	switch {
	case errors.As(err, &re):
		fmt.Fprintln(a.stderr, "invalid input:\n  "+strings.Join(re.Problems, "\n  "))
		return exitUsage
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(a.stderr, "interrupted; submitted batches keep running and the next loop picks them up")
		return exitInterrupted
	}
	fmt.Fprintln(a.stderr, describe(err))
	if statusOf(err) != 0 {
		return exitAPI
	}
	return exitUsage
}

func readInput(a *app, path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(a.stdin)
	}
	return os.ReadFile(path)
}

func run(ctx context.Context, args []string, a *app) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(a.stderr, usage)
		return exitUsage
	}
	cmds := map[string]func(context.Context, []string, *app) int{
		"ask": cmdAsk, "enqueue": cmdEnqueue, "get": cmdGet, "results": cmdResults, "serve": cmdServe, "run": cmdRun,
		"status": cmdStatus, "cancel": cmdCancel, "reconcile": cmdReconcile, "providers": cmdProviders, "profiles": cmdProfiles, "validate": cmdValidate,
	}
	fn, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(a.stderr, "unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
	return fn(ctx, args[1:], a)
}

func parse(fs *flag.FlagSet, args []string) bool {
	if err := fs.Parse(args); err != nil {
		return false
	}
	return true
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "q-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func cmdAsk(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("ask", a)
	rf := addRequestFlags(fs)
	fs.Var(&rf.meta, "meta", "key=value carried onto the result (repeatable)")
	id := fs.String("id", "", "request id (default: generated)")
	prompt := fs.String("prompt", "", "the prompt")
	file := fs.String("file", "", "read the prompt from a file (default: stdin when -prompt is empty)")
	if !parse(fs, args) {
		return exitUsage
	}
	d, err := rf.defaults()
	if err != nil {
		return a.fail(err)
	}
	text := *prompt
	if text == "" {
		b, err := readInput(a, map[bool]string{true: "-", false: *file}[*file == ""])
		if err != nil {
			return a.fail(err)
		}
		text = string(b)
	}
	if *id == "" {
		*id = randomID()
	}
	row := map[string]any{"id": *id, "prompt": text}
	if len(rf.meta) > 0 {
		meta := map[string]any{}
		for _, kv := range rf.meta {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				fmt.Fprintf(a.stderr, "-meta %q: want key=value\n", kv)
				return exitUsage
			}
			meta[k] = v
		}
		row["meta"] = meta
	}
	req, problems := delphi.Normalize(row, d, offline(), "ask")
	if len(problems) > 0 {
		return a.fail(&delphi.RequestError{Problems: problems})
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	if _, err := st.Enqueue([]delphi.Request{req}, time.Now()); err != nil {
		return a.fail(err)
	}
	fmt.Fprintln(a.stdout, req.ID)
	return exitOK
}

func cmdEnqueue(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("enqueue", a)
	rf := addRequestFlags(fs)
	in := fs.String("in", "", "JSONL file of requests (\"-\" for stdin)")
	if !parse(fs, args) {
		return exitUsage
	}
	d, err := rf.defaults()
	if err != nil {
		return a.fail(err)
	}
	if *in == "" {
		fmt.Fprintln(a.stderr, "-in is required")
		return exitUsage
	}
	data, err := readInput(a, *in)
	if err != nil {
		return a.fail(err)
	}
	reqs, err := delphi.Parse(data, d, offline())
	if err != nil {
		return a.fail(err)
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	n, err := st.Enqueue(reqs, time.Now())
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.stdout, "queued %d request(s) (%d already known)\n", n, len(reqs)-n)
	return exitOK
}

func cmdValidate(ctx context.Context, args []string, a *app) int {
	fs, _ := newFS("validate", a)
	rf := addRequestFlags(fs)
	in := fs.String("in", "", "JSONL file of requests (\"-\" for stdin)")
	if !parse(fs, args) {
		return exitUsage
	}
	d, err := rf.defaults()
	if err != nil {
		return a.fail(err)
	}
	if *in == "" {
		fmt.Fprintln(a.stderr, "-in is required")
		return exitUsage
	}
	data, err := readInput(a, *in)
	if err != nil {
		return a.fail(err)
	}
	reqs, err := delphi.Parse(data, d, offline())
	if err != nil {
		return a.fail(err)
	}
	count := map[string]int{}
	for _, r := range reqs {
		count[r.Provider+" "+r.Model]++
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(a.stdout, "ok: %d request(s)\n", len(reqs))
	for _, k := range keys {
		fmt.Fprintf(a.stdout, "  %-50s %d\n", k, count[k])
	}
	return exitOK
}

func printResult(a *app, r delphi.Result, textOnly bool) {
	if textOnly {
		fmt.Fprintln(a.stdout, r.Text)
		return
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintln(a.stdout, string(b))
}

func cmdGet(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("get", a)
	wait := fs.Bool("wait", false, "block until the result is in (a running `delphi serve` produces it)")
	text := fs.Bool("text", false, "print only the answer text")
	if !parse(fs, args) {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(a.stderr, "usage: delphi get [-wait] [-text] ID")
		return exitUsage
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	for {
		r, ok, err := st.Result(fs.Arg(0))
		if err != nil {
			return a.fail(err)
		}
		if ok {
			printResult(a, r, *text)
			if r.Status != delphi.Succeeded {
				return exitFailures
			}
			return exitOK
		}
		if !*wait {
			fmt.Fprintf(a.stderr, "%s: not finished (see `delphi status`)\n", fs.Arg(0))
			return exitFailures
		}
		select {
		case <-ctx.Done():
			return exitInterrupted
		case <-time.After(5 * time.Second):
		}
	}
}

func cmdResults(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("results", a)
	if !parse(fs, args) {
		return exitUsage
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	rs, err := st.Results()
	if err != nil {
		return a.fail(err)
	}
	enc := json.NewEncoder(a.stdout)
	for _, r := range rs {
		_ = enc.Encode(r)
	}
	return exitOK
}

type loopFlags struct {
	every, flushEvery time.Duration
	flushAt, attempts int
	onResult          string
}

func addLoopFlags(fs *flag.FlagSet, every, flushEvery time.Duration) *loopFlags {
	lf := &loopFlags{}
	fs.DurationVar(&lf.every, "every", every, "how often to poll batches and check the queue")
	fs.DurationVar(&lf.flushEvery, "flush-every", flushEvery, "submit a provider's queue once its oldest request has waited this long")
	fs.IntVar(&lf.flushAt, "flush-at", 100, "submit as soon as this many requests are queued for one provider")
	fs.IntVar(&lf.attempts, "max-attempts", 3, "attempts per request for expired, missing or transient failures")
	fs.StringVar(&lf.onResult, "on-result", "", "shell command run per finished result, with DELPHI_ID, DELPHI_STATUS, DELPHI_RESULT (file path) set")
	return lf
}

func (a *app) engine(st *delphi.FileStore, reg delphi.Registry, lf *loopFlags) *delphi.Engine {
	e := &delphi.Engine{Store: st, Providers: reg, FlushEvery: lf.flushEvery, FlushAt: lf.flushAt,
		MaxAttempts: lf.attempts, StatusOf: statusOf, Log: a.logf}
	if lf.onResult != "" {
		e.OnResult = func(r delphi.Result) { a.hook(lf.onResult, st, r) }
	}
	return e
}

func (a *app) hook(command string, st *delphi.FileStore, r delphi.Result) {
	shell, flagC := "sh", "-c"
	if runtime.GOOS == "windows" {
		shell, flagC = "cmd", "/C"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, flagC, command)
	cmd.Env = append(os.Environ(), "DELPHI_ID="+r.ID, "DELPHI_STATUS="+string(r.Status),
		"DELPHI_RESULT="+st.ResultPath(r.ID), "DELPHI_HOME="+st.Dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		a.logf("on-result hook for %s failed: %v: %s", r.ID, err, strings.TrimSpace(string(out)))
	}
}

func cmdServe(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("serve", a)
	lf := addLoopFlags(fs, time.Minute, 10*time.Minute)
	once := fs.Bool("once", false, "do one pass and exit (for cron)")
	if !parse(fs, args) {
		return exitUsage
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	unlock, err := st.Lock()
	if err != nil {
		return a.fail(err)
	}
	defer unlock()
	reg, missing := live()
	if len(reg) == 0 {
		fmt.Fprintln(a.stderr, "no provider credentials: export ANTHROPIC_API_KEY and/or OPENAI_API_KEY")
		return exitUsage
	}
	for name, err := range missing {
		a.logf("provider %s disabled: %v", name, err)
	}
	e := a.engine(st, reg, lf)
	a.logf("delphi serving %s (providers: %s; poll %s, flush after %s or %d queued)", st.Dir, strings.Join(reg.Names(), ", "), lf.every, lf.flushEvery, lf.flushAt)
	for {
		rep, err := e.Tick(ctx, false)
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			a.logf("stopping; in-flight batches keep running and are picked up on restart")
			return exitOK
		}
		if err != nil {
			a.logf("tick: %s", describe(err))
		}
		if len(rep.Stuck) > 0 {
			a.logf("%d flight(s) need `delphi reconcile`: %s", len(rep.Stuck), strings.Join(rep.Stuck, ", "))
		}
		if *once {
			if err != nil {
				return a.fail(err)
			}
			return exitOK
		}
		select {
		case <-ctx.Done():
			a.logf("stopping; in-flight batches keep running and are picked up on restart")
			return exitOK
		case <-time.After(lf.every):
		}
	}
}

func cmdRun(ctx context.Context, args []string, a *app) int {
	fs, _ := newFS("run", a)
	rf := addRequestFlags(fs)
	lf := addLoopFlags(fs, time.Minute, 0)
	in := fs.String("in", "", "JSONL file of requests (\"-\" for stdin)")
	prompt := fs.String("prompt", "", "a single prompt instead of -in (id prompt-1)")
	out := fs.String("out", "", "run directory: state, results.jsonl, summary.json")
	if !parse(fs, args) {
		return exitUsage
	}
	d, err := rf.defaults()
	if err != nil {
		return a.fail(err)
	}
	if *out == "" || (*in == "") == (*prompt == "") {
		fmt.Fprintln(a.stderr, "usage: delphi run (-in F | -prompt TEXT) -out DIR [flags]")
		return exitUsage
	}
	var reqs []delphi.Request
	if *prompt != "" {
		r, p := delphi.Normalize(map[string]any{"id": "prompt-1", "prompt": *prompt}, d, offline(), "-prompt")
		if len(p) > 0 {
			return a.fail(&delphi.RequestError{Problems: p})
		}
		reqs = []delphi.Request{r}
	} else {
		data, err := readInput(a, *in)
		if err != nil {
			return a.fail(err)
		}
		var perr error
		if reqs, perr = delphi.Parse(data, d, offline()); perr != nil {
			return a.fail(perr)
		}
	}
	reg, missing := live()
	for _, r := range reqs {
		if _, ok := reg[r.Provider]; !ok {
			fmt.Fprintf(a.stderr, "error: %v\n", missing[r.Provider])
			return exitUsage
		}
	}
	st, err := delphi.OpenFileStore(*out)
	if err != nil {
		return a.fail(err)
	}
	unlock, err := st.Lock()
	if err != nil {
		return a.fail(err)
	}
	defer unlock()
	// Re-running with the same input resumes; a different request under a
	// used id is refused, so nothing is billed twice.
	n, err := st.Enqueue(reqs, time.Now())
	if err != nil {
		return a.fail(err)
	}
	if n < len(reqs) {
		a.logf("resuming %s: %d new, %d already known", st.Dir, n, len(reqs)-n)
	}
	e := a.engine(st, reg, lf)
	for {
		rep, err := e.Tick(ctx, true)
		if err != nil {
			return a.fail(err)
		}
		if len(rep.Stuck) > 0 {
			a.logf("%d flight(s) need `delphi reconcile -home %s`: %s", len(rep.Stuck), st.Dir, strings.Join(rep.Stuck, ", "))
		}
		finished := 0
		for _, r := range reqs {
			if _, ok, _ := st.Result(r.ID); ok {
				finished++
			}
		}
		if finished == len(reqs) {
			return a.writeRun(st, reqs, reg)
		}
		a.logf("%d/%d answered", finished, len(reqs))
		select {
		case <-ctx.Done():
			return a.fail(ctx.Err())
		case <-time.After(lf.every):
		}
	}
}

type modelSummary struct {
	Provider string       `json:"provider"`
	Model    string       `json:"model"`
	Requests int          `json:"requests"`
	Usage    delphi.Usage `json:"usage"`
	CostUSD  *float64     `json:"estimated_cost_usd,omitempty"`
}

type runSummary struct {
	Requests     int                   `json:"requests"`
	StatusCounts map[delphi.Status]int `json:"status_counts"`
	Truncated    int                   `json:"truncated"`
	Retryable    int                   `json:"retryable"`
	Models       []modelSummary        `json:"models"`
	CostNote     string                `json:"cost_note"`
}

func (a *app) writeRun(st *delphi.FileStore, reqs []delphi.Request, reg delphi.Registry) int {
	s := runSummary{Requests: len(reqs), StatusCounts: map[delphi.Status]int{},
		CostNote: "batch list prices for input and output tokens only; cache tokens unpriced; confirm in the provider console"}
	var results, retry []byte
	models := map[string]*modelSummary{}
	for _, req := range reqs {
		r, _, err := st.Result(req.ID)
		if err != nil {
			return a.fail(err)
		}
		b, _ := json.Marshal(r)
		results = append(append(results, b...), '\n')
		s.StatusCounts[r.Status]++
		if r.Truncated {
			s.Truncated++
		}
		if r.Retryable() || r.Status == delphi.Canceled {
			s.Retryable++
			rb, _ := json.Marshal(retryRow(req))
			retry = append(append(retry, rb...), '\n')
		}
		key := r.Provider + "\x00" + req.Model
		m := models[key]
		if m == nil {
			m = &modelSummary{Provider: r.Provider, Model: req.Model}
			models[key] = m
		}
		m.Requests++
		if r.Usage != nil {
			m.Usage.Add(*r.Usage)
		}
	}
	total, priced := 0.0, true
	for _, m := range models {
		if c, ok := delphi.Cost(reg[m.Provider], m.Model, m.Usage); ok {
			c = float64(int(c*10000+0.5)) / 10000
			m.CostUSD = &c
			total += c
		} else {
			priced = false
		}
		s.Models = append(s.Models, *m)
	}
	sort.Slice(s.Models, func(i, j int) bool {
		return s.Models[i].Provider+s.Models[i].Model < s.Models[j].Provider+s.Models[j].Model
	})
	for name, data := range map[string][]byte{"results.jsonl": results, "retryable.jsonl": retry} {
		if err := os.WriteFile(filepath.Join(st.Dir, name), data, 0o644); err != nil {
			return a.fail(err)
		}
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(filepath.Join(st.Dir, "summary.json"), append(b, '\n'), 0o644); err != nil {
		return a.fail(err)
	}
	cost := fmt.Sprintf("estimated $%.2f", total)
	if !priced {
		cost += " (some models unpriced)"
	}
	a.logf("done: %d request(s) %v; %s; results in %s", len(reqs), s.StatusCounts, cost, filepath.Join(st.Dir, "results.jsonl"))
	if s.Truncated > 0 {
		fmt.Fprintf(a.stderr, "warning: %d answer(s) hit max_tokens; raise -max-tokens and resubmit them\n", s.Truncated)
	}
	if s.StatusCounts[delphi.Succeeded] != len(reqs) {
		fmt.Fprintf(a.stderr, "not every request succeeded (see results.jsonl; retryable.jsonl is valid -in for a new run)\n")
		return exitFailures
	}
	return exitOK
}

// retryRow turns a request back into an input row.
func retryRow(r delphi.Request) map[string]any {
	msgs := make([]any, len(r.Messages))
	for i, m := range r.Messages {
		msgs[i] = map[string]any{"role": m.Role, "content": m.Content}
	}
	row := map[string]any{"id": r.ID, "provider": r.Provider, "model": r.Model, "messages": msgs, "max_tokens": r.MaxTokens}
	if r.System != "" {
		row["system"] = r.System
	}
	if r.Effort != "" {
		row["effort"] = r.Effort
	}
	if len(r.Params) > 0 {
		row["params"] = r.Params
	}
	if len(r.Meta) > 0 {
		row["meta"] = r.Meta
	}
	return row
}

func cmdStatus(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("status", a)
	if !parse(fs, args) {
		return exitUsage
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	pending, err := st.Pending()
	if err != nil {
		return a.fail(err)
	}
	flights, err := st.Flights()
	if err != nil {
		return a.fail(err)
	}
	results, err := st.Results()
	if err != nil {
		return a.fail(err)
	}
	byProv := map[string]int{}
	for _, q := range pending {
		byProv[q.Provider]++
	}
	counts := map[delphi.Status]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	fmt.Fprintf(a.stdout, "home     %s\nqueued   %d %v\nflights  %d\nresults  %d %v\n", st.Dir, len(pending), byProv, len(flights), len(results), counts)
	for _, f := range flights {
		fmt.Fprintf(a.stdout, "  %s  %-9s %-10s %-34s %d request(s), state %q %v\n", f.Label, f.Provider, f.State, f.BatchID, len(f.Requests), f.Status.State, f.Status.Counts)
	}
	return exitOK
}

func cmdCancel(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("cancel", a)
	label := fs.String("label", "", "flight to cancel")
	all := fs.Bool("all", false, "cancel every running flight")
	if !parse(fs, args) {
		return exitUsage
	}
	if (*label == "") == !*all {
		fmt.Fprintln(a.stderr, "give -label L or -all")
		return exitUsage
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	flights, err := st.Flights()
	if err != nil {
		return a.fail(err)
	}
	reg, _ := live()
	n := 0
	for _, f := range flights {
		if (*all || f.Label == *label) && f.State == delphi.FlightRunning {
			p, ok := reg[f.Provider]
			if !ok {
				fmt.Fprintf(a.stderr, "%s: provider %s has no credentials\n", f.Label, f.Provider)
				continue
			}
			if err := p.Cancel(ctx, f.BatchID); err != nil {
				return a.fail(err)
			}
			a.logf("canceling %s (%s %s); its results arrive on the next loop pass", f.Label, f.Provider, f.BatchID)
			n++
		}
	}
	if n == 0 {
		fmt.Fprintln(a.stderr, "no running flight matched")
		return exitUsage
	}
	return exitOK
}

func cmdReconcile(ctx context.Context, args []string, a *app) int {
	fs, home := newFS("reconcile", a)
	label := fs.String("label", "", "the stuck flight")
	adopt := fs.String("adopt", "", "batch id the flight created (find it in the provider console or `list`)")
	requeue := fs.Bool("requeue", false, "no batch was created: put the requests back in the queue")
	if !parse(fs, args) {
		return exitUsage
	}
	st, err := delphi.OpenFileStore(*home)
	if err != nil {
		return a.fail(err)
	}
	if *label == "" {
		flights, err := st.Flights()
		if err != nil {
			return a.fail(err)
		}
		n := 0
		for _, f := range flights {
			if f.State == delphi.FlightSubmitting {
				n++
				fmt.Fprintf(a.stdout, "%s  %s  %d request(s)  submitted %s\n", f.Label, f.Provider, len(f.Requests), f.SubmittedAt.Format(time.RFC3339))
			}
		}
		if n > 0 {
			fmt.Fprintln(a.stdout, "\nCheck the provider console for a batch created at that time with that many requests, then:\n  delphi reconcile -label L -adopt BATCH_ID    (it exists)\n  delphi reconcile -label L -requeue           (it does not)")
		}
		return exitOK
	}
	if err := delphi.Reconcile(st, *label, *adopt, *requeue); err != nil {
		return a.fail(err)
	}
	fmt.Fprintln(a.stdout, "ok")
	return exitOK
}

func cmdProviders(ctx context.Context, args []string, a *app) int {
	fs, _ := newFS("providers", a)
	if !parse(fs, args) {
		return exitUsage
	}
	reg, missing := live()
	off := offline()
	for _, name := range off.Names() {
		state := "ready"
		if _, ok := reg[name]; !ok {
			state = missing[name].Error()
		}
		model := off[name].DefaultModel()
		if model == "" {
			model = "(none: set DELPHI_" + strings.ToUpper(name) + "_MODEL or -model)"
		}
		fmt.Fprintf(a.stdout, "%-10s default model %-40s %s\n", name, model, state)
	}
	return exitOK
}

func cmdProfiles(ctx context.Context, args []string, a *app) int {
	fs, _ := newFS("profiles", a)
	if !parse(fs, args) {
		return exitUsage
	}
	path := defaultProfiles()
	profiles, err := delphi.LoadProfiles(path)
	if err != nil {
		return a.fail(err)
	}
	if len(profiles) == 0 {
		fmt.Fprintf(a.stdout, "no profiles in %s\n", path)
		return exitOK
	}
	for _, name := range profiles.Names() {
		p := profiles[name]
		model := p.Model
		if model == "" {
			model = "(provider default)"
		}
		line := fmt.Sprintf("%-14s %s:%s", name, p.Provider, model)
		if p.Effort != "" {
			line += " effort " + p.Effort
		}
		if len(p.Fallback) > 0 {
			line += " -> " + strings.Join(p.Fallback, " -> ")
		}
		fmt.Fprintln(a.stdout, line)
	}
	return exitOK
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := &app{stdin: bufio.NewReader(os.Stdin), stdout: os.Stdout, stderr: os.Stderr}
	os.Exit(run(ctx, os.Args[1:], a))
}
