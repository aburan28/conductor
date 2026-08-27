package policy

import "testing"

func TestExpressions(t *testing.T) {
	env := MapEnv{
		"task.estimated_files":    2,
		"task.security_sensitive": false,
		"task.risk":               "high",
		"task.labels":             []string{"docs", "backend"},
		"task.paths":              []string{"docs/guide.md", "docs/api/index.md"},
		"task.title":              "Write the Migration Guide",
		"attempt.failures":        int64(2),
		"budget.fraction":         0.8,
		"session.tier":            "T3",
		"session.effort":          "high",
		"empty":                   []string{},
	}
	cases := []struct {
		src  string
		want bool
	}{
		{"", true},
		{"task.estimated_files <= 2 && !task.security_sensitive", true},
		{"task.estimated_files < 2", false},
		{"task.risk == \"high\"", true},
		{"task.risk == 'HIGH'", true},
		{"task.risk >= \"medium\"", true},
		{"task.risk > \"critical\"", false},
		{"task.labels has \"docs\"", true},
		{"task.labels has \"frontend\"", false},
		{"\"docs\" in task.labels", true},
		{"task.risk in [\"high\", \"critical\"]", true},
		{"task.paths all \"docs/**\"", true},
		{"task.paths any \"**/*.md\"", true},
		{"task.paths any \"internal/**\"", false},
		{"empty any \"**\"", false},
		{"task.title contains \"migration\"", true},
		{"task.title matches \"re:(?i)guide$\"", true},
		{"task.title startswith \"Write\"", true},
		{"attempt.failures >= 2 || task.risk == \"low\"", true},
		{"attempt.failures >= 2 and task.risk == 'low'", false},
		{"not task.security_sensitive", true},
		{"budget.fraction > 0.75", true},
		{"session.tier >= \"T2\"", true},
		{"session.tier >= \"T4\"", false},
		{"session.effort >= \"medium\"", true},
		{"len(task.labels) == 2", true},
		{"count(task.paths) * 2 > 3", true},
		{"lower(task.risk) == \"high\"", true},
		{"undefined.fact == null", true},
		{"undefined.fact > 1", false},
		{"undefined.fact", false},
		{"(task.estimated_files + 1) * 2 == 6", true},
		{"-task.estimated_files < 0", true},
		{"true", true},
		{"false || true", true},
	}
	for _, c := range cases {
		e, err := Compile(c.src)
		if err != nil {
			t.Fatalf("compile %q: %v", c.src, err)
		}
		got, err := e.Bool(env)
		if err != nil {
			t.Fatalf("eval %q: %v", c.src, err)
		}
		if got != c.want {
			t.Errorf("%q = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	for _, src := range []string{
		"task.risk ==", "(a && b", "a has", "[1, 2", "\"unterminated", "frob(1)", "a ?? b", "1 2",
	} {
		if _, err := Compile(src); err == nil {
			t.Errorf("expected %q to fail to compile", src)
		}
	}
}

func TestVars(t *testing.T) {
	e := MustCompile("task.risk == \"high\" && (attempt.failures > 1 || task.labels has \"x\")")
	got := e.Vars()
	want := []string{"attempt.failures", "task.labels", "task.risk"}
	if len(got) != len(want) {
		t.Fatalf("vars = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("vars = %v, want %v", got, want)
		}
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docs", true},
		{"docs/**", "src/docs/a.md", false},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "c.md", true},
		{"**/*.go", "c.md", false},
		{"internal/*/api", "internal/x/api", true},
		{"internal/*/api", "internal/x/y/api", false},
		{"internal/**/api", "internal/x/y/api", true},
		{"*.sql", "0001.sql", true},
		{"migrations/**/*.sql", "migrations/0001.sql", true},
		{"**", "anything/at/all", true},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestEvalErrors(t *testing.T) {
	env := MapEnv{"s": "x"}
	for _, src := range []string{"s - 1", "1 / 0", "s matches \"re:(\""} {
		e := MustCompile(src)
		if _, err := e.Eval(env); err == nil {
			t.Errorf("expected %q to fail to evaluate", src)
		}
	}
}
