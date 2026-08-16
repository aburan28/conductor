package resource

import (
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

func TestParseNormalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"path:src/a.go", "path:src/a.go"},
		{"path:./src/a.go", "path:src/a.go"},
		{"path:/src/a.go", "path:src/a.go"},
		{"path:src/./a.go", "path:src/a.go"},
		{"path:src/x/../a.go", "path:src/a.go"},
		{"path:src\\a.go", "path:src/a.go"},
		{"dir:packages/router/", "dir:packages/router"},
		{"dir:/", "dir:."},
		{"src/a.go", "path:src/a.go"},
		{"src/router/", "dir:src/router"},
		{"api:get /v1/tasks", "api:GET /v1/tasks"},
		{"api:GET v1/tasks", "api:GET /v1/tasks"},
		{"migration:primary", "migration:primary"},
		{"  path:src/a.go  ", "path:src/a.go"},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		if got.String() != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

// Traversal outside the repository root must be refused rather than normalized into
// something inside it (DESIGN.md §11.4).
func TestParseRejectsTraversal(t *testing.T) {
	for _, in := range []string{"path:../outside.go", "dir:../../etc", "path:a/../../b"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should have failed", in)
		}
	}
}

// An unrecognized prefix must not be silently treated as a resource type; it is a path.
func TestParseUnknownPrefixIsPath(t *testing.T) {
	got, err := Parse("C:/Users/x/file.go")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Type != domain.ResourcePath {
		t.Errorf("type = %q, want path", got.Type)
	}
}

func TestOverlapsPaths(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Identity and directory containment.
		{"path:src/a.go", "path:src/a.go", true},
		{"path:src/a.go", "path:src/b.go", false},
		{"dir:src", "path:src/a.go", true},
		{"path:src/a.go", "dir:src", true},
		{"dir:src", "path:srcfoo/a.go", false}, // prefix must be a path boundary
		{"dir:src/api", "dir:src", true},
		{"dir:src/api", "dir:src/web", false},
		{"dir:.", "path:anything/at/all.go", true},

		// Lockfiles and tests are path-like.
		{"lockfile:go.sum", "path:go.sum", true},
		{"test:internal/api", "dir:internal", true},

		// Globs.
		{"path:internal/**/*_test.go", "path:internal/api/api_test.go", true},
		{"path:internal/**/*_test.go", "path:internal/api/api.go", false},
		{"path:internal/*.go", "path:internal/api/api.go", false},
		{"path:internal/**", "path:internal/a/b/c.go", true},
		{"path:internal/**/*.go", "dir:internal/api", true},
		{"path:migrations/*.sql", "dir:cmd", false},

		// Repo covers everything.
		{"repo:conductor", "path:anything.go", true},

		// Migrations serialize; named lanes do not collide with each other.
		{"migration:primary", "migration:primary", true},
		{"migration:*", "migration:primary", true},
		{"migration:primary", "migration:analytics", false},
		{"migration:primary", "dir:migrations", true},
		{"migration:primary", "dir:internal/api", false},
		{"migration:primary", "schema:public", true},

		// Schema equality only.
		{"schema:public", "schema:public", true},
		{"schema:public", "schema:audit", false},

		// API routes: params match any single segment, method must line up.
		{"api:GET /v1/tasks/:id", "api:GET /v1/tasks/abc", true},
		{"api:GET /v1/tasks/{id}", "api:GET /v1/tasks/abc", true},
		{"api:GET /v1/tasks/:id", "api:POST /v1/tasks/abc", false},
		{"api:GET /v1/tasks", "api:GET /v1/tasks/abc", false},
		{"api:GET /v1/tasks", "path:internal/api/tasks.go", false},

		// Symbols nest, and resolve to their file when compared with a path.
		{"symbol:go:pkg/api", "symbol:go:pkg/api.Handler", true},
		{"symbol:go:pkg/api.Handler", "symbol:go:pkg/api.Other", false},
		{"symbol:internal/ledger.Claim", "dir:internal", true},
	}

	for _, tc := range cases {
		a, err := Parse(tc.a)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.a, err)
		}
		b, err := Parse(tc.b)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.b, err)
		}
		if got := Overlaps(a, b); got != tc.want {
			t.Errorf("Overlaps(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		// Overlap is a symmetric relation; asymmetry here would make conflict detection
		// depend on which side happened to be inserted first.
		if got := Overlaps(b, a); got != tc.want {
			t.Errorf("Overlaps(%q, %q) [reversed] = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestGlobVsGlobIsConservative(t *testing.T) {
	// Two patterns over the same subtree must be reported as overlapping even though
	// deciding pattern intersection exactly is not tractable.
	a := MustParse("path:internal/**/*.go")
	b := MustParse("path:internal/api/*.go")
	if !Overlaps(a, b) {
		t.Error("overlapping glob subtrees should conflict")
	}

	// internal/** and cmd/** share nothing; a false positive here would make every glob
	// reservation block every other one.
	c := MustParse("path:cmd/**/*.go")
	if Overlaps(a, c) {
		t.Error("disjoint glob subtrees should not conflict")
	}

	// Different file kinds in the same subtree cannot select the same file.
	d := MustParse("path:internal/**/*.md")
	if Overlaps(a, d) {
		t.Error("*.go and *.md patterns should not conflict")
	}

	// ...but a narrower suffix of the same kind still can.
	e := MustParse("path:internal/**/*_test.go")
	if !Overlaps(a, e) {
		t.Error("*.go and *_test.go patterns should conflict")
	}
}

func TestSetDeduplicates(t *testing.T) {
	s := NewSet()
	if !s.Add(MustParse("path:a.go")) {
		t.Fatal("first add should report new")
	}
	if s.Add(MustParse("path:./a.go")) {
		t.Error("normalized duplicate should not be reported as new")
	}
	if s.Len() != 1 {
		t.Errorf("len = %d, want 1", s.Len())
	}
	if got := s.Strings(); len(got) != 1 || got[0] != "path:a.go" {
		t.Errorf("Strings() = %v", got)
	}
}
