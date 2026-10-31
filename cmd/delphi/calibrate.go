package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aburan28/conductor/internal/capability"
	"github.com/aburan28/conductor/internal/delphi"
)

func defaultRegistry() string {
	if p := os.Getenv("DELPHI_REGISTRY"); p != "" {
		return p
	}
	return filepath.Join(defaultHome(), "registry")
}

// cmdCalibrate runs one suite against one target through the ordinary engine, grades every
// answer with the suite's grader, and adds a write-once calibration record to the registry.
func cmdCalibrate(ctx context.Context, args []string, a *app) int {
	fs, _ := newFS("calibrate", a)
	rf := addRequestFlags(fs)
	lf := addLoopFlags(fs, 30*time.Second, 0)
	suitePath := fs.String("suite", "", "calibration suite file (conductor.capability.suite.v1)")
	registry := fs.String("registry", defaultRegistry(), "registry directory ($DELPHI_REGISTRY)")
	out := fs.String("out", "", "run directory (default: <registry>/runs/<suite>--<target>--<time>)")
	if !parse(fs, args) {
		return exitUsage
	}
	if *suitePath == "" {
		fmt.Fprintln(a.stderr, "usage: delphi calibrate -suite FILE [-provider P -model M | -profile NAME] [-effort E]")
		return exitUsage
	}
	suite, err := capability.LoadSuite(*suitePath)
	if err != nil {
		return a.fail(err)
	}
	d, err := rf.defaults()
	if err != nil {
		return a.fail(err)
	}
	// Calibration measures one target: a refusal is that model's answer, not a reason to
	// ask another model, so the fallback chain is dropped.
	d.Fallback = nil
	reqs := make([]delphi.Request, 0, len(suite.Items))
	for _, it := range suite.Items {
		row := map[string]any{"id": it.ID, "prompt": it.Prompt}
		if suite.System != "" {
			row["system"] = suite.System
		}
		if suite.MaxTokens > 0 {
			row["max_tokens"] = json.Number(strconv.Itoa(suite.MaxTokens)) // rows are decoded with UseNumber
		}
		r, problems := delphi.Normalize(row, d, offline(), "item "+it.ID)
		if len(problems) > 0 {
			return a.fail(&delphi.RequestError{Problems: problems})
		}
		r.Fallback = nil
		reqs = append(reqs, r)
	}
	target := capability.Target{Provider: reqs[0].Provider, Model: reqs[0].Model, Effort: reqs[0].Effort}
	reg, missing := live()
	if _, ok := reg[target.Provider]; !ok {
		fmt.Fprintf(a.stderr, "error: %v\n", missing[target.Provider])
		return exitUsage
	}
	if *out == "" {
		stamp := time.Now().UTC().Format("20060102T150405Z")
		*out = filepath.Join(*registry, "runs", strings.NewReplacer(":", "_", "/", "_", "@", "_").Replace(
			fmt.Sprintf("%s--%s--%s", suite.ID, target, stamp)))
	}
	return a.drive(ctx, *out, reqs, reg, lf, func(st *delphi.FileStore) int {
		c := capability.Calibration{Suite: suite.ID, SuiteSHA256: suite.SHA256, Family: suite.Family, Target: target,
			RunDir: st.Dir, FinishedAt: time.Now().UTC().Format(time.RFC3339)}
		priced := true
		var cost float64
		for i, it := range suite.Items {
			r, _, err := st.Result(reqs[i].ID)
			if err != nil {
				return a.fail(err)
			}
			if r.Usage != nil {
				c.InputTokens += r.Usage.InputTokens
				c.OutputToks += r.Usage.OutputTokens
				if in, outp, ok := reg[target.Provider].Price(reqs[i].Model); ok { // priced as requested, like `run`
					cost += (float64(r.Usage.InputTokens)*in + float64(r.Usage.OutputTokens)*outp) / 1e6
				} else {
					priced = false
				}
			}
			switch r.Status {
			case delphi.Succeeded:
			case delphi.Refused:
				c.Refused++
				c.Graded++
				c.Grades = append(c.Grades, capability.Grade{Item: it.ID, Passed: false, Reason: "refused"})
				continue
			default:
				// Never answered (errored, expired, missing): the infrastructure's failure,
				// not the model's, so it is reported and left out of the pass rate.
				c.NotAnswered++
				continue
			}
			g, err := suite.GradeAnswer(ctx, it, r.Text)
			if err != nil {
				return a.fail(fmt.Errorf("grading %s: %w (no calibration record written)", it.ID, err))
			}
			if r.Truncated && g.Passed {
				g.Reason = "passed, though the answer stopped at max_tokens"
			}
			c.Graded++
			if g.Passed {
				c.Passed++
			}
			c.Grades = append(c.Grades, g)
		}
		if priced {
			c.CostUSD = &cost
		}
		r, err := capability.Open(*registry)
		if err != nil {
			return a.fail(err)
		}
		path, err := r.AddCalibration(c)
		if err != nil {
			return a.fail(err)
		}
		lo, hi := capability.Wilson(c.Passed, c.Graded)
		fmt.Fprintf(a.stdout, "%s on %s (%s): %d/%d passed, 95%% %.3f..%.3f; %d refused, %d not answered\n%s\n",
			target, suite.ID, suite.Family, c.Passed, c.Graded, lo, hi, c.Refused, c.NotAnswered, path)
		if c.NotAnswered > 0 {
			return exitFailures
		}
		return exitOK
	})
}

// cmdRegistry imports public evidence and prints per-family tables.
func cmdRegistry(_ context.Context, args []string, a *app) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: delphi registry import -file F | table [-family F] [-json]")
		return exitUsage
	}
	sub, args := args[0], args[1:]
	fs, _ := newFS("registry "+sub, a)
	registry := fs.String("registry", defaultRegistry(), "registry directory ($DELPHI_REGISTRY)")
	file := fs.String("file", "", "import: a JSON array of conductor.capability.evidence.v1 records")
	family := fs.String("family", "", "table: one family (default: every family)")
	asJSON := fs.Bool("json", false, "table: machine-readable output")
	if !parse(fs, args) {
		return exitUsage
	}
	r, err := capability.Open(*registry)
	if err != nil {
		return a.fail(err)
	}
	switch sub {
	case "import":
		data, err := os.ReadFile(*file)
		if err != nil {
			return a.fail(err)
		}
		var recs []capability.Evidence
		if err := json.Unmarshal(data, &recs); err != nil {
			return a.fail(fmt.Errorf("%s: %w", *file, err))
		}
		n, err := r.ImportEvidence(recs, time.Now())
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.stdout, "imported %d new record(s), %d already present\n", n, len(recs)-n)
		return exitOK
	case "table":
		families := r.Families()
		if *family != "" {
			families = []string{*family}
		}
		var tables []capability.Table
		for _, f := range families {
			tables = append(tables, r.Table(f))
		}
		if *asJSON {
			b, _ := json.MarshalIndent(tables, "", "  ")
			fmt.Fprintln(a.stdout, string(b))
			return exitOK
		}
		for _, t := range tables {
			printTable(a, t)
		}
		return exitOK
	}
	fmt.Fprintf(a.stderr, "unknown registry command %q\n", sub)
	return exitUsage
}

func printTable(a *app, t capability.Table) {
	level := "none"
	if t.Level > 0 {
		level = fmt.Sprintf("%.2f", t.Level)
	}
	fmt.Fprintf(a.stdout, "\n%s  (margin %.2f, level %s, reference %s)\n", t.Family, t.Delta, level, orNone(t.Reference))
	w := tabwriter.NewWriter(a.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "target\tpassed\trate\t95% interval\t$/accepted\tprior\tvs reference\tlabel")
	for _, r := range t.Rows {
		cost, prior, rate, interval := "-", "-", "-", "-"
		if r.CostPer != nil {
			cost = fmt.Sprintf("%.4f", *r.CostPer)
		}
		if r.Prior != nil {
			prior = fmt.Sprintf("%.2f", *r.Prior)
		}
		if r.Graded > 0 {
			rate, interval = fmt.Sprintf("%.3f", r.Rate), fmt.Sprintf("%.3f..%.3f", r.Low, r.High)
		}
		verdict := string(r.Verdict)
		if r.Target == t.Reference {
			verdict = "reference"
		} else if verdict == "" {
			verdict = "-"
		}
		label := r.Label
		if r.MeetsLevel != nil && !*r.MeetsLevel {
			label += ", below level"
		}
		fmt.Fprintf(w, "%s\t%d/%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Target, r.Passed, r.Graded, rate, interval, cost, prior, verdict, label)
	}
	w.Flush()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// cmdRoute prints the registry's delegation decision for a task family.
func cmdRoute(_ context.Context, args []string, a *app) int {
	fs, _ := newFS("route", a)
	family := fs.String("family", "", "task family (as in the suites, e.g. card-compile)")
	registry := fs.String("registry", defaultRegistry(), "registry directory ($DELPHI_REGISTRY)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if !parse(fs, args) {
		return exitUsage
	}
	if *family == "" {
		fmt.Fprintln(a.stderr, "usage: delphi route -family F [-json]")
		return exitUsage
	}
	route, err := routeFamily(*family, *registry, a.free)
	if *asJSON {
		out := map[string]any{"route": route}
		if err != nil {
			out["error"] = err.Error()
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Fprintln(a.stdout, string(b))
		if err != nil {
			return exitFailures
		}
		return exitOK
	}
	if err != nil {
		return a.fail(err)
	}
	fmt.Fprintf(a.stdout, "%s -> %s\n  %s\n", route.Family, route.Choice, route.Basis)
	if len(route.Escalation) > 0 {
		fmt.Fprintf(a.stdout, "  escalates to: %s\n", strings.Join(route.Escalation, ", "))
	}
	w := tabwriter.NewWriter(a.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "  considered\teligible\twhy")
	for _, c := range route.Considered {
		fmt.Fprintf(w, "  %s\t%v\t%s\n", c.Target, c.Eligible, c.Why)
	}
	w.Flush()
	return exitOK
}
