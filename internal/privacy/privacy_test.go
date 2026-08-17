package privacy

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := NewDedupeKey()
	if err != nil {
		t.Fatalf("NewDedupeKey: %v", err)
	}
	return k
}

func TestFingerprintIsDeterministicAndOrderInsensitive(t *testing.T) {
	key := testKey(t)

	a := Envelope{Summary: "Add retry-aware model routing", Scopes: []string{"dir:internal/router"}}
	b := Envelope{Summary: "model routing, retry aware — added", Scopes: []string{"dir:internal/router"}}

	if a.Fingerprint(key) != a.Fingerprint(key) {
		t.Fatal("fingerprint is not deterministic")
	}
	if a.Fingerprint(key) != b.Fingerprint(key) {
		t.Errorf("reordered/reworded intent should share a fingerprint:\n a=%s\n b=%s",
			a.Canonical(), b.Canonical())
	}
}

func TestFingerprintIsTenantScoped(t *testing.T) {
	e := Envelope{Summary: "Add retry-aware model routing"}
	if e.Fingerprint(testKey(t)) == e.Fingerprint(testKey(t)) {
		t.Error("identical text under different tenant keys must not produce the same fingerprint")
	}
}

func TestFingerprintCarriesVersion(t *testing.T) {
	fp := Envelope{Summary: "anything"}.Fingerprint(testKey(t))
	if !strings.HasPrefix(fp, "hmac-sha256:"+FingerprintVersion+":") {
		t.Errorf("fingerprint %q missing algorithm/version prefix", fp)
	}
}

func TestSimilaritySeparatesDuplicateFromUnrelated(t *testing.T) {
	key := testKey(t)

	alice := Envelope{
		Summary: "Implement the team invite flow with email invitations and acceptance",
		Scopes:  []string{"dir:internal/invites"},
	}
	bob := Envelope{
		Summary: "Build team invitation flow: send invite emails, accept invitations",
		Scopes:  []string{"dir:internal/invites"},
	}
	unrelated := Envelope{
		Summary: "Upgrade the Postgres connection pool and add query timeouts",
		Scopes:  []string{"dir:internal/db"},
	}

	dup := Similarity(alice.Signature(key), bob.Signature(key))
	far := Similarity(alice.Signature(key), unrelated.Signature(key))

	if dup <= far {
		t.Fatalf("duplicate similarity %.2f should exceed unrelated %.2f", dup, far)
	}
	if dup < 0.30 {
		t.Errorf("near-duplicate similarity %.2f is too low to be useful", dup)
	}
	if far > 0.20 {
		t.Errorf("unrelated similarity %.2f is too high; would produce false conflicts", far)
	}
}

func TestSimilarityOfIdenticalIsOne(t *testing.T) {
	key := testKey(t)
	e := Envelope{Summary: "Rework the scheduler backoff strategy", Scopes: []string{"path:internal/scheduler/scheduler.go"}}
	if got := Similarity(e.Signature(key), e.Signature(key)); got != 1.0 {
		t.Errorf("self-similarity = %.3f, want 1.0", got)
	}
}

// An empty or contentless summary must never look like a duplicate of anything, or every
// undescribed task would collide with every other one.
func TestEmptySignatureNeverMatches(t *testing.T) {
	key := testKey(t)
	empty := Envelope{}
	if sig := empty.Signature(key); sig != nil {
		t.Errorf("empty envelope signature = %v, want nil", sig)
	}
	other := Envelope{Summary: "Rework the scheduler backoff strategy"}
	if got := Similarity(empty.Signature(key), other.Signature(key)); got != 0 {
		t.Errorf("similarity with empty = %.3f, want 0", got)
	}
}

func TestSignatureWidth(t *testing.T) {
	sig := Envelope{Summary: "add routing policy evaluator"}.Signature(testKey(t))
	if len(sig) != MinHashLanes {
		t.Errorf("signature width = %d, want %d", len(sig), MinHashLanes)
	}
}

// ---------------------------------------------------------------------------
// Visibility projection
// ---------------------------------------------------------------------------

func TestPrivateTaskHidesIntentButKeepsTerritory(t *testing.T) {
	alice := Owner{PrincipalID: "p-alice", Handle: "alice"}
	bob := Viewer{PrincipalID: "p-bob", Role: domain.RoleContributor}

	task := domain.Task{
		ID:                 "t1",
		Ref:                "T-42",
		Title:              "Migrate billing to the new provider",
		Objective:          "Sensitive commercial detail",
		ExternalRef:        "GH-812",
		Status:             domain.TaskRunning,
		Visibility:         domain.VisibilityPrivate,
		AcceptanceCriteria: []domain.AcceptanceCriterion{{Text: "secret criterion"}},
	}
	proj := TaskProjection{
		Owner:  alice,
		Scopes: []string{"dir:internal/billing"},
		Branch: "conductor/T-42",
	}

	view := ProjectTask(bob, task, proj)

	// What Bob must NOT learn.
	if view.Title != "" || view.Objective != "" || view.ExternalRef != "" {
		t.Errorf("private task leaked intent to another principal: %+v", view)
	}
	if len(view.AcceptanceCriteria) != 0 {
		t.Error("private task leaked acceptance criteria")
	}

	// What Bob MUST learn, or coordination fails.
	if view.Ref != "T-42" || view.Owner != "alice" || view.Status != domain.TaskRunning {
		t.Errorf("private task hid coordination state that prevents collisions: %+v", view)
	}
	if len(view.Scopes) != 1 || view.Scopes[0] != "dir:internal/billing" {
		t.Errorf("private task hid its reserved territory: %+v", view.Scopes)
	}
	if !view.Redacted {
		t.Error("redacted view should be marked as such")
	}

	// The owner sees everything.
	own := ProjectTask(Viewer{PrincipalID: "p-alice"}, task, proj)
	if own.Title != task.Title || own.Objective != task.Objective {
		t.Error("owner should see their own private task in full")
	}
	if own.Redacted {
		t.Error("owner's own view should not be marked redacted")
	}
}

func TestVisibilityLadder(t *testing.T) {
	owner := Owner{PrincipalID: "p-alice", Handle: "alice"}
	viewer := Viewer{PrincipalID: "p-bob", Role: domain.RoleContributor}
	proj := TaskProjection{
		Owner:      owner,
		CommitSHA:  "fed987",
		Artifacts:  []domain.Artifact{{ID: "a1", Kind: "commit"}},
		Validation: []domain.ValidationResult{{CommandID: "test"}},
	}

	summary := ProjectTask(viewer, domain.Task{
		Title: "x", Visibility: domain.VisibilityTeamSummary,
	}, proj)
	if summary.Title != "x" {
		t.Error("team_summary should expose the title")
	}
	if summary.CommitSHA != "" || len(summary.Artifacts) != 0 || len(summary.Validation) != 0 {
		t.Error("team_summary must not expose artifacts or validation evidence")
	}

	artifacts := ProjectTask(viewer, domain.Task{
		Title: "x", Visibility: domain.VisibilityTeamArtifacts,
	}, proj)
	if artifacts.CommitSHA == "" || len(artifacts.Artifacts) == 0 {
		t.Error("team_artifacts should expose artifacts")
	}
}

func TestPresenceRedactsPrivateTaskTitle(t *testing.T) {
	alice := Owner{PrincipalID: "p-alice", Handle: "alice"}
	bob := Viewer{PrincipalID: "p-bob"}
	task := domain.Task{Ref: "T-42", Title: "secret", Status: domain.TaskRunning, Visibility: domain.VisibilityPrivate}

	entry := ProjectPresence(bob,
		domain.Session{ID: "s1", Harness: "claude-code", State: domain.SessionWorking, HeartbeatAt: time.Now()},
		domain.Principal{ID: "p-alice", Handle: "alice"},
		&task, alice, []string{"dir:internal/billing"}, "")

	if entry.TaskTitle != "" {
		t.Errorf("presence leaked private task title: %q", entry.TaskTitle)
	}
	if entry.TaskRef != "T-42" || entry.State != domain.SessionWorking {
		t.Error("presence should still show the ref and state")
	}
	if len(entry.Scopes) != 1 {
		t.Error("presence should still show held scopes")
	}
}

// The conflict view must not carry the similarity score. A numeric oracle over someone
// else's private intent can be probed to reconstruct it.
func TestConflictViewOmitsSimilarityScore(t *testing.T) {
	v := ProjectConflict(
		Viewer{PrincipalID: "p-bob"},
		domain.ConflictEdge{
			Kind:       domain.ConflictDuplicateIntent,
			Severity:   domain.SeverityMedium,
			Suggestion: domain.OutcomeSuggestJoin,
			Detail:     domain.ConflictDetail{Similarity: 0.83, Reason: "overlapping in-flight work"},
		},
		domain.Task{Ref: "T-50", Title: "mine"},
		domain.Task{Ref: "T-42", Title: "secret", Visibility: domain.VisibilityPrivate},
		Owner{PrincipalID: "p-bob", Handle: "bob"},
		Owner{PrincipalID: "p-alice", Handle: "alice"},
	)

	if v.Other.Title != "" {
		t.Errorf("conflict view leaked private counterparty title: %q", v.Other.Title)
	}
	if v.Other.Owner != "alice" || v.Suggestion != domain.OutcomeSuggestJoin {
		t.Error("conflict view should still name the owner and the suggested action")
	}

	// Structural assertion: no field of ConflictView may carry the score.
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		if strings.Contains(strings.ToLower(rt.Field(i).Name), "similarity") {
			t.Errorf("ConflictView must not expose a similarity field (found %q)", rt.Field(i).Name)
		}
	}
}

func TestSanitizeEventPayload(t *testing.T) {
	clean, dropped := SanitizeEventPayload(map[string]any{
		"task_ref": "T-42",
		"status":   "running",
		"prompt":   "the user's actual prompt",
		"messages": []string{"..."},
	})
	if _, ok := clean["prompt"]; ok {
		t.Error("prompt survived sanitization")
	}
	if _, ok := clean["messages"]; ok {
		t.Error("messages survived sanitization")
	}
	if clean["task_ref"] != "T-42" || clean["status"] != "running" {
		t.Errorf("allowlisted keys were dropped: %+v", clean)
	}
	if len(dropped) != 2 {
		t.Errorf("dropped = %v, want 2 keys", dropped)
	}
}

func TestClampSummary(t *testing.T) {
	long := strings.Repeat("a", MaxSummaryLength+50)
	got := ClampSummary(long)
	if len([]rune(got)) > MaxSummaryLength+1 {
		t.Errorf("clamped length = %d, want <= %d", len([]rune(got)), MaxSummaryLength+1)
	}
	if ClampSummary("short") != "short" {
		t.Error("short summaries should pass through unchanged")
	}
}

// ---------------------------------------------------------------------------
// Structural privacy guarantees (DESIGN.md §32.4)
// ---------------------------------------------------------------------------

// forbidden matches identifiers that would indicate conversation content is being stored or
// transmitted. DESIGN.md §12.4 and §36.9 make this a hard invariant, so it is asserted
// mechanically rather than by review.
var forbidden = regexp.MustCompile(`(?i)(prompt|transcript|chain_?of_?thought|reasoning_?(text|content|trace)|completion_?text|chat_?(log|history|message)|conversation|assistant_?(text|message)|tool_?(input|arguments|result)|raw_?(output|text)|stdout|stderr|clipboard|env_?vars?)`)

// allowedExceptions are identifiers that match the pattern but are legitimate.
var allowedExceptions = map[string]bool{
	// "reasoning_effort" is a routing knob (how hard to think), not stored reasoning.
	"reasoning_effort":    true,
	"ReasoningEffort":     true,
	"MaxReasoningLevel":   true,
	"max_reasoning_level": true,
}

// TestNoTranscriptFieldsInSharedTypes walks every type that crosses the control-plane
// boundary and fails if any field could carry conversation content.
func TestNoTranscriptFieldsInSharedTypes(t *testing.T) {
	types := []any{
		domain.Task{}, domain.Attempt{}, domain.Session{}, domain.Lease{},
		domain.ScopeReservation{}, domain.Intent{}, domain.ConflictEdge{},
		domain.ConflictDetail{}, domain.Event{}, domain.HandoffBundle{},
		domain.EvidenceManifest{}, domain.PresenceEntry{}, domain.Artifact{},
		domain.ValidationResult{}, domain.ReviewFinding{}, domain.DecisionRecord{},
		domain.Principal{}, domain.Project{}, domain.Runner{}, domain.ModelProfile{},
		TaskView{}, ConflictView{}, ConflictParty{},
	}

	for _, v := range types {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			name := f.Name
			tag := strings.Split(f.Tag.Get("json"), ",")[0]

			for _, candidate := range []string{name, tag} {
				if candidate == "" || allowedExceptions[candidate] {
					continue
				}
				if forbidden.MatchString(candidate) {
					t.Errorf("%s.%s (json:%q) looks like it carries conversation content",
						rt.Name(), name, tag)
				}
			}
		}
	}
}

// TestNoTranscriptColumnsInSchema asserts the same invariant one layer down: there must be
// no place in the database to put a transcript even if application code tried.
func TestNoTranscriptColumnsInSchema(t *testing.T) {
	root := findRepoRoot(t)
	dir := filepath.Join(root, "internal", "db", "migrations")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}

	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		found = true
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for lineNo, line := range strings.Split(string(body), "\n") {
			code, _, _ := strings.Cut(line, "--") // ignore comments, which discuss these words
			code = strings.TrimSpace(code)
			if code == "" {
				continue
			}
			if allowedExceptions[firstWord(code)] {
				continue
			}
			if m := forbidden.FindString(code); m != "" {
				t.Errorf("%s:%d declares a column that could hold conversation content: %q",
					e.Name(), lineNo+1, strings.TrimSpace(line))
			}
		}
	}
	if !found {
		t.Fatal("no migrations found; the schema privacy check would silently pass")
	}
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repository root")
	return ""
}

// The similarity estimate must be stable across tenant keys, not merely correct on average.
//
// Each organization gets its own HMAC key, which reshuffles the MinHash permutations, so the
// same pair of intents produces a slightly different estimate per tenant. At 64 lanes the
// spread was wide enough that a genuine duplicate slipped under the threshold roughly 2.5% of
// the time — a miss nobody would ever notice, because the symptom is a warning that does not
// appear. This measures the spread instead of trusting it.
func TestSimilarityIsStableAcrossKeys(t *testing.T) {
	alice := Envelope{
		Summary: "Implement the team invite flow with email invitations and acceptance",
		Scopes:  []string{"dir:internal/invites"},
	}
	bob := Envelope{
		Summary: "Build team invitation flow: send invite emails, accept invitations",
		Scopes:  []string{"dir:internal/invites"},
	}
	unrelated := Envelope{
		Summary: "Upgrade the Postgres connection pool and add query timeouts",
		Scopes:  []string{"dir:internal/db"},
	}

	// The threshold the shipped policy files use.
	const threshold = 0.50
	const trials = 200

	missed, falsePositives := 0, 0
	var min, max float64 = 1, 0
	for i := 0; i < trials; i++ {
		key, err := NewDedupeKey()
		if err != nil {
			t.Fatalf("NewDedupeKey: %v", err)
		}
		dup := Similarity(alice.Signature(key), bob.Signature(key))
		if dup < min {
			min = dup
		}
		if dup > max {
			max = dup
		}
		if dup < threshold {
			missed++
		}
		if Similarity(alice.Signature(key), unrelated.Signature(key)) >= threshold {
			falsePositives++
		}
	}

	if missed > 0 {
		t.Errorf("a genuine duplicate fell below the %.2f threshold in %d/%d tenant keys "+
			"(range %.3f–%.3f); raise MinHashLanes or lower the threshold",
			threshold, missed, trials, min, max)
	}
	if falsePositives > 0 {
		t.Errorf("unrelated work exceeded the threshold in %d/%d tenant keys", falsePositives, trials)
	}
	// A duplicate that only just clears the bar is a warning sign even when it passes.
	if min < threshold+0.05 {
		t.Errorf("duplicate similarity bottomed out at %.3f, uncomfortably close to %.2f", min, threshold)
	}
}
