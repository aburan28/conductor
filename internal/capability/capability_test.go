package capability_test

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/capability"
)

func near(a, b float64) bool { return math.Abs(a-b) < 5e-4 }

func TestWilsonMatchesPublishedValues(t *testing.T) {
	for _, c := range []struct {
		k, n   int
		lo, hi float64
	}{
		{0, 10, 0, 0.2775},
		{10, 10, 0.7225, 1},
		{50, 100, 0.4038, 0.5962},
	} {
		lo, hi := capability.Wilson(c.k, c.n)
		if !near(lo, c.lo) || !near(hi, c.hi) {
			t.Errorf("Wilson(%d,%d) = %.4f..%.4f, want %.4f..%.4f", c.k, c.n, lo, hi, c.lo, c.hi)
		}
	}
	if lo, hi := capability.Wilson(0, 0); lo != 0 || hi != 1 {
		t.Errorf("Wilson(0,0) = %v..%v, want the whole interval", lo, hi)
	}
}

func TestCompareNeedsEvidenceToCallTwoModelsEquivalent(t *testing.T) {
	for _, c := range []struct {
		name           string
		kA, nA, kB, nB int
		want           capability.Verdict
	}{
		{"same rate, many items", 950, 1000, 950, 1000, capability.Equivalent},
		{"perfect on both, few items", 10, 10, 10, 10, capability.Inconclusive},
		{"clearly ahead", 95, 100, 50, 100, capability.Better},
		{"clearly behind", 50, 100, 95, 100, capability.Worse},
		{"ahead by less than the margin, many items", 960, 1000, 950, 1000, capability.Equivalent},
	} {
		if got := capability.Compare(c.kA, c.nA, c.kB, c.nB, 0.05); got != c.want {
			lo, hi := capability.DiffInterval(c.kA, c.nA, c.kB, c.nB)
			t.Errorf("%s: %s (interval %.3f..%.3f), want %s", c.name, got, lo, hi, c.want)
		}
	}
}

func writeSuite(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "suite.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSuiteRejectsWhatCannotBeGraded(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{
		`{"schema":"x","id":"s","family":"math","grader":{"kind":"exact"},"items":[{"id":"a","prompt":"p","expect":"1"}]}`,
		`{"schema":"conductor.capability.suite.v1","id":"s","family":"math","grader":{"kind":"exact"},"items":[]}`,
		`{"schema":"conductor.capability.suite.v1","id":"s","family":"math","grader":{"kind":"exact"},"items":[{"id":"a","prompt":"p"}]}`,
		`{"schema":"conductor.capability.suite.v1","id":"s","family":"math","grader":{"kind":"command"},"items":[{"id":"a","prompt":"p"}]}`,
		`{"schema":"conductor.capability.suite.v1","id":"s","family":"math","grader":{"kind":"regex"},"items":[{"id":"a","prompt":"p","expect":"("}]}`,
		`{"schema":"conductor.capability.suite.v1","id":"s","family":"math","grader":{"kind":"exact"},"items":[{"id":"a","prompt":"p","expect":"1"},{"id":"a","prompt":"q","expect":"2"}]}`,
		`{"schema":"conductor.capability.suite.v1","id":"s","family":"math","grader":{"kind":"exact"},"items":[{"id":"a","prompt":"p","expect":"1"}],"surprise":1}`,
	} {
		if _, err := capability.LoadSuite(writeSuite(t, dir, bad)); err == nil {
			t.Errorf("LoadSuite accepted %s", bad)
		}
	}
}

func TestGradersExactRegexAndCommand(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	exact, err := capability.LoadSuite(writeSuite(t, dir, `{"schema":"conductor.capability.suite.v1","id":"s","family":"math",
		"grader":{"kind":"exact"},"items":[{"id":"a","prompt":"6*7","expect":"42"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := exact.GradeAnswer(ctx, exact.Items[0], " 42\n"); !g.Passed || len(g.AnswerSHA256) != 64 {
		t.Errorf("exact pass = %+v", g)
	}
	if g, _ := exact.GradeAnswer(ctx, exact.Items[0], "The answer is 42"); g.Passed {
		t.Error("exact accepted extra text")
	}
	re, _ := capability.LoadSuite(writeSuite(t, dir, `{"schema":"conductor.capability.suite.v1","id":"s","family":"math",
		"grader":{"kind":"regex"},"items":[{"id":"a","prompt":"6*7","expect":"\\b42\\b"}]}`))
	if g, _ := re.GradeAnswer(ctx, re.Items[0], "The answer is 42."); !g.Passed {
		t.Errorf("regex = %+v", g)
	}
	// The command grader runs in the suite's directory and reads the item as JSON on stdin.
	if err := os.WriteFile(filepath.Join(dir, "grade.sh"), []byte(`in=$(cat)
case "$in" in *'"answer":"42"'*) echo '{"passed":true}';; *) echo '{"passed":false,"reason":"not 42"}';; esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, err := capability.LoadSuite(writeSuite(t, dir, `{"schema":"conductor.capability.suite.v1","id":"s","family":"math",
		"grader":{"kind":"command","argv":["sh","grade.sh"]},"items":[{"id":"a","prompt":"6*7"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if g, err := cmd.GradeAnswer(ctx, cmd.Items[0], "42"); err != nil || !g.Passed {
		t.Errorf("command pass = %+v, %v", g, err)
	}
	if g, err := cmd.GradeAnswer(ctx, cmd.Items[0], "41"); err != nil || g.Passed || g.Reason != "not 42" {
		t.Errorf("command fail = %+v, %v", g, err)
	}
	broken, _ := capability.LoadSuite(writeSuite(t, dir, `{"schema":"conductor.capability.suite.v1","id":"s","family":"math",
		"grader":{"kind":"command","argv":["sh","-c","echo not json"]},"items":[{"id":"a","prompt":"6*7"}]}`))
	if _, err := broken.GradeAnswer(ctx, broken.Items[0], "42"); err == nil {
		t.Error("a grader that prints no verdict was taken as one")
	}
}

var now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func evidence(model, bench string, score float64, retrieved string) capability.Evidence {
	return capability.Evidence{Schema: capability.EvidenceSchema, Model: model, Benchmark: bench, Version: "v1",
		Score: score, Scale: "percent", SourceURL: "https://example.org/board", Retrieved: retrieved}
}

func TestEvidenceNeedsASourceAndADate(t *testing.T) {
	good := evidence("m", "b", 50, "2026-10-01")
	if p := good.Validate(now); len(p) != 0 {
		t.Fatalf("valid record rejected: %v", p)
	}
	for name, mutate := range map[string]func(*capability.Evidence){
		"no source":    func(e *capability.Evidence) { e.SourceURL = "" },
		"plain http":   func(e *capability.Evidence) { e.SourceURL = "http://example.org" },
		"no date":      func(e *capability.Evidence) { e.Retrieved = "" },
		"future date":  func(e *capability.Evidence) { e.Retrieved = "2027-01-01" },
		"bad scale":    func(e *capability.Evidence) { e.Scale = "stars" },
		"out of range": func(e *capability.Evidence) { e.Score = 120 },
		"no version":   func(e *capability.Evidence) { e.Version = "" },
	} {
		e := good
		mutate(&e)
		if len(e.Validate(now)) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestImportIsAllOrNothingAndIdempotent(t *testing.T) {
	r, _ := capability.Open(t.TempDir())
	bad := evidence("m", "b", 50, "")
	if n, err := r.ImportEvidence([]capability.Evidence{evidence("m", "b", 50, "2026-10-01"), bad}, now); err == nil || n != 0 {
		t.Fatalf("a batch with one bad record imported %d, %v", n, err)
	}
	recs := []capability.Evidence{evidence("m", "b", 50, "2026-10-01")}
	if n, err := r.ImportEvidence(recs, now); err != nil || n != 1 {
		t.Fatalf("import = %d, %v", n, err)
	}
	if n, err := r.ImportEvidence(recs, now); err != nil || n != 0 {
		t.Fatalf("re-import = %d, %v", n, err)
	}
	again, _ := capability.Open(r.Dir)
	if len(again.Evidence) != 1 {
		t.Fatalf("on disk: %d records", len(again.Evidence))
	}
}

func calib(target capability.Target, suite, sha, at string, passed, graded int, cost float64) capability.Calibration {
	return capability.Calibration{Suite: suite, SuiteSHA256: sha, Family: "card-compile", Target: target,
		Passed: passed, Graded: graded, CostUSD: &cost, FinishedAt: at}
}

func TestTableRanksPoolsAndLabels(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "families.json"), []byte(`{"families":{"card-compile":{"delta":0.05,"level":0.8}},
		"benchmarks":{"swe":{"card-compile":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := capability.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sonnet := capability.Target{Provider: "anthropic", Model: "claude-sonnet-5-5", Effort: "high"}
	haiku := capability.Target{Provider: "anthropic", Model: "claude-haiku-5-5"}
	glm := capability.Target{Provider: "vllm", Model: "glm-5.3"}
	for _, c := range []capability.Calibration{
		calib(sonnet, "cards-a", "sha-a", "2026-10-30T10:00:00Z", 190, 200, 4),
		calib(haiku, "cards-a", "sha-a", "2026-10-29T10:00:00Z", 20, 200, 1), // superseded below
		calib(haiku, "cards-a", "sha-a", "2026-10-30T11:00:00Z", 188, 200, 1),
		calib(glm, "cards-a", "sha-a", "2026-10-30T12:00:00Z", 120, 200, 0.5),
	} {
		if _, err := r.AddCalibration(c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.ImportEvidence([]capability.Evidence{evidence("deepseek-v4", "swe", 70, "2026-10-20"),
		evidence("claude-haiku-5-5", "swe", 60, "2026-10-20")}, now); err != nil {
		t.Fatal(err)
	}
	r, _ = capability.Open(dir) // read back from disk
	tab := r.Table("card-compile")
	if tab.Reference != sonnet.String() || len(tab.Rows) != 4 {
		t.Fatalf("table = %+v", tab)
	}
	byTarget := map[string]capability.Row{}
	for _, row := range tab.Rows {
		byTarget[row.Target] = row
	}
	h := byTarget[haiku.String()]
	if h.Graded != 200 || h.Passed != 188 {
		t.Errorf("haiku should use only its latest run of the suite: %+v", h)
	}
	// Adjusted rates 0.9356 and 0.9455: the 90% interval of the difference is about
	// -0.049..+0.029, inside the five-point margin, so the two are equivalent.
	if h.Verdict != capability.Equivalent {
		t.Errorf("haiku 188/200 vs sonnet 190/200: %s", h.Verdict)
	}
	if h.CostPer == nil || !near(*h.CostPer, 1.0/188) || h.Prior == nil || !near(*h.Prior, 0.6) {
		t.Errorf("haiku cost/prior = %v %v", h.CostPer, h.Prior)
	}
	if g := byTarget[glm.String()]; g.Verdict != capability.Worse || g.MeetsLevel == nil || *g.MeetsLevel {
		t.Errorf("glm 120/200 = %+v", g)
	}
	if d := byTarget["deepseek-v4"]; d.Label != "prior-only" || d.Prior == nil || !near(*d.Prior, 0.7) || d.Graded != 0 {
		t.Errorf("deepseek prior-only row = %+v", d)
	}
	if tab.Rows[len(tab.Rows)-1].Label != "prior-only" {
		t.Error("prior-only rows must come after calibrated ones")
	}
	// Calibrations are write-once.
	if _, err := r.AddCalibration(calib(glm, "cards-a", "sha-a", "2026-10-30T12:00:00Z", 1, 1, 0)); err == nil ||
		!strings.Contains(err.Error(), "exists") {
		t.Errorf("a second record with the same name was written: %v", err)
	}
}
