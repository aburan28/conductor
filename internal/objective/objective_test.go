package objective

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRefusesWhatCannotBeCheckedOrBudgeted(t *testing.T) {
	parent := &Node{ID: "n0", Depth: 1, BudgetUSD: 1, SpentUSD: 0.2}
	lim := Limits{MaxDepth: 2, MaxFanout: 2, MaxAttempts: 3}
	routable := func(f string) error {
		if f == "summarize" {
			return nil
		}
		return os.ErrNotExist
	}
	d := Decomposition{Decomposable: true, Children: []Child{
		{Kind: KindWork, Statement: "a", Criteria: []string{" "}, Family: "summarize", BudgetUSD: 0.5},
		{Kind: KindObjective, Statement: "b", Criteria: []string{"c"}, Family: "x", BudgetUSD: 0.5},
		{Kind: "task", Statement: "", Criteria: []string{"c"}, BudgetUSD: 0},
	}}
	got := strings.Join(Validate(parent, d, lim, time.Now(), routable), "\n")
	for _, want := range []string{"3 children, more than the limit of 2", "child 1: no acceptance criteria",
		"child 2: at depth 2 it must be a work item", "child 2: an objective takes no family",
		`child 3: kind "task"`, "child 3: no statement", "child 3: budget_usd must be positive",
		"sum to $1.0000, more than the $0.8000 left"} {
		if !strings.Contains(got, want) {
			t.Errorf("problems lack %q:\n%s", want, got)
		}
	}
	if p := Validate(parent, Decomposition{Decomposable: false, Reason: "too vague"}, lim, time.Now(), routable); len(p) != 1 || !strings.Contains(p[0], "too vague") {
		t.Errorf("not decomposable: %v", p)
	}
	ok := Decomposition{Decomposable: true, Children: []Child{{Kind: KindWork, Statement: "a", Criteria: []string{"c"}, Family: "summarize", BudgetUSD: 0.8}}}
	if p := Validate(parent, ok, lim, time.Now(), routable); len(p) != 0 {
		t.Errorf("valid decomposition refused: %v", p)
	}
}

func TestParseDecompositionTakesOneObjectAndRefusesExtras(t *testing.T) {
	if _, err := ParseDecomposition("```json\n{\"decomposable\": false, \"reason\": \"r\"}\n```"); err != nil {
		t.Errorf("fenced: %v", err)
	}
	for _, bad := range []string{"{}{}", `{"decomposable": true, "extra": 1}`, "prose"} {
		if _, err := ParseDecomposition(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestVendorSeparatesModelFamilies(t *testing.T) {
	for _, c := range [][3]string{
		{"anthropic", "claude-haiku-5-5", "anthropic"}, {"openai", "gpt-test", "openai"},
		{"openrouter", "deepseek/deepseek-v4:free", "deepseek"}, {"vllm", "glm-5.3", "zhipu"},
		{"bedrock", "claude-sonnet", "anthropic"}, {"lmstudio", "local-thing", "lmstudio"},
	} {
		if got := Vendor(c[0], c[1]); got != c[2] {
			t.Errorf("Vendor(%s, %s) = %s, want %s", c[0], c[1], got, c[2])
		}
	}
}

func TestLogIsAppendOnlyAndSurvivesATornLastLine(t *testing.T) {
	dir := t.TempDir()
	l, err := Create(dir, InitData{Statement: "s", Criteria: []string{"c"}, BudgetUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(dir, InitData{}); err == nil {
		t.Fatal("a second Create overwrote the run")
	}
	if err := l.Append("n0", EvRouted, RoutedData{Chain: []string{"p:m"}, Basis: "b"}); err != nil {
		t.Fatal(err)
	}
	// An event that does not fold is refused and never written.
	if err := l.Append("n9", EvFailed, ReasonData{Reason: "x"}); err == nil {
		t.Fatal("event for an unknown node accepted")
	}
	f, _ := os.OpenFile(filepath.Join(dir, "objective.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"seq":3,"type":"fai`)
	f.Close()
	l2, err := Load(dir)
	if err != nil {
		t.Fatalf("torn last line: %v", err)
	}
	if n := l2.Tree().Get("n0"); n.Route[0] != "p:m" || n.State != Open {
		t.Fatalf("fold: %+v", n)
	}
}

func TestGrantsAndUnallocatedKeepEverySubtreeCovered(t *testing.T) {
	dir := t.TempDir()
	l, err := Create(dir, InitData{Statement: "s", Criteria: []string{"c"}, BudgetUSD: 10})
	if err != nil {
		t.Fatal(err)
	}
	must := func(node, typ string, data any) {
		t.Helper()
		if err := l.Append(node, typ, data); err != nil {
			t.Fatal(err)
		}
	}
	must("n0", EvDelegated, Attempt{Target: "p:m", RequestID: "n0-plan-a0"})
	must("n0", EvAnswered, AnsweredData{RequestID: "n0-plan-a0", Status: "succeeded", CostUSD: 1, Priced: true, Outcome: "decomposed"})
	must("n0", EvDecomposed, DecomposedData{Children: []Child{
		{Kind: KindObjective, Statement: "a", Criteria: []string{"c"}, BudgetUSD: 4},
		{Kind: KindWork, Statement: "b", Criteria: []string{"c"}, Family: "f", BudgetUSD: 2}}})
	tr := l.Tree()
	if got := Unallocated(tr.Get("n0"), tr); got != 3 { // 10 - 1 spent - 6 allocated
		t.Fatalf("unallocated = %v", got)
	}
	must("n0.1", EvDelegated, Attempt{Target: "p:m", RequestID: "n0_1-plan-a0"})
	must("n0.1", EvAnswered, AnsweredData{RequestID: "n0_1-plan-a0", Status: "succeeded", CostUSD: 0.5, Priced: true, Outcome: "decomposed"})
	must("n0.1", EvDecomposed, DecomposedData{Children: []Child{{Kind: KindWork, Statement: "c", Criteria: []string{"c"}, Family: "f", BudgetUSD: 3.5}}})
	// A grant from the root to n0.1.1 raises n0.1 too, so n0.1 still covers its child.
	must("n0.1.1", EvGrant, Grant{From: "n0", AmountUSD: 2, Reason: "r"})
	if a, b := tr.Get("n0.1.1").BudgetUSD, tr.Get("n0.1").BudgetUSD; a != 5.5 || b != 6 {
		t.Fatalf("budgets after grant: n0.1.1 %v n0.1 %v", a, b)
	}
	if got := Unallocated(tr.Get("n0"), tr); got != 1 {
		t.Fatalf("root unallocated after grant = %v", got)
	}
	if err := l.Append("n0.2", EvGrant, Grant{From: "n0.1", AmountUSD: 1}); err == nil {
		t.Fatal("a grant from a non-ancestor was accepted")
	}
}
