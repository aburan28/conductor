package bounds

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/policy"
)

// testdata/frontier.json is a copy of a real `ecbench frontier build` output
// (aburan28/crypto docs/bounds/frontier.json, taken 2026-10-06), seal intact. The verdicts
// under testdata/verdicts/ are hand-written fixtures following the `Verdict` field set, sealed
// the same way, and say so in their statements; the challenge they name is a real one.
const (
	fixtureFrontierID = "ECFR1h8b45b672c283"
	fixtureChallenge  = "ECCH1h91fd655dfb54"
	fixtureDomain     = "ECDOM1ha1f283af3a5d"
)

func load(t *testing.T, root string, cfg Config) *Store {
	t.Helper()
	s, err := Load(root, cfg)
	if err != nil {
		t.Fatalf("Load(%q, %+v): %v", root, cfg, err)
	}
	return s
}

func TestFrontierDocumentReads(t *testing.T) {
	s := load(t, "testdata", Config{Frontier: "frontier.json"})
	f := s.Frontier
	if f == nil {
		t.Fatal("frontier.json is present and sealed, but no frontier was read")
	}
	if f.ID != fixtureFrontierID {
		t.Errorf("frontier id = %s, want %s", f.ID, fixtureFrontierID)
	}
	if len(f.Domains) != 3 || f.Entries() != 43 || f.FrontierEntries() != 13 {
		t.Errorf("domains=%d entries=%d frontier=%d; want 3, 43, 13", len(f.Domains), f.Entries(), f.FrontierEntries())
	}
	if f.BuiltFrom != 46 || f.Inadmissible != 3 || strings.Join(f.Axes, ",") != "ops,memory" {
		t.Errorf("built_from=%d inadmissible=%d axes=%v; want 46, 3, [ops memory]", f.BuiltFrom, f.Inadmissible, f.Axes)
	}

	// Prime, toy: bsgs.negation leads on operations at 1.10x the floor, and the frozen
	// reference walk on memory -- the two leaders of a domain need not be one bound.
	prime, ok := f.Domain(fixtureDomain)
	if !ok {
		t.Fatalf("domain %s not read", fixtureDomain)
	}
	if prime.Family != "prime" || prime.Tier != "toy" || prime.Unit != "ecbench.gae" || prime.Problem != "ecdlp.single_target" {
		t.Errorf("prime domain = %+v", prime)
	}
	if prime.Entries != 17 || prime.FrontierEntries != 8 {
		t.Errorf("prime entries=%d frontier=%d; want 17, 8", prime.Entries, prime.FrontierEntries)
	}
	if ol := prime.OpsLeader; !ol.Known || ol.BoundID != "ECBND1he8163c29cc39" || ol.Method != "bsgs.negation" ||
		ol.MethodID != "ECM1h87f6341629c3" || fmt.Sprintf("%.4f", ol.Value) != "1.1000" {
		t.Errorf("prime ops leader = %+v", ol)
	}
	if ml := prime.MemoryLeader; !ml.Known || ml.BoundID != "ECBND1h412ef80d6c1a" || ml.Method != "rho.frozen_reference" ||
		fmt.Sprintf("%.4f", ml.Value) != "0.0118" {
		t.Errorf("prime memory leader = %+v", ml)
	}

	// Koblitz, toy: one bound leads on both axes.
	koblitz, ok := f.Domain("ECDOM1h080dd3dc639e")
	if !ok {
		t.Fatal("koblitz toy domain not read")
	}
	if koblitz.OpsLeader.BoundID != "ECBND1hc2936a598d20" || koblitz.MemoryLeader.BoundID != "ECBND1hc2936a598d20" ||
		koblitz.OpsLeader.Method != "rho.signed_frobenius" {
		t.Errorf("koblitz leaders = %+v / %+v", koblitz.OpsLeader, koblitz.MemoryLeader)
	}

	if len(s.Challenges) != 0 {
		t.Errorf("no verdicts were configured, yet %d challenges were read", len(s.Challenges))
	}
	facts := s.Facts("")
	if !facts.Known || facts.FrontierID != fixtureFrontierID || facts.DomainCount != 3 || facts.FrontierEntries != 13 || facts.ChallengeKnown {
		t.Errorf("facts = %+v", facts)
	}
}

func TestVerdictFixturesSummarise(t *testing.T) {
	s := load(t, "testdata", Config{Verdicts: "verdicts/*.json"})
	if s.Frontier != nil {
		t.Error("no frontier was configured, yet one was read")
	}
	c, ok := s.Challenges[fixtureChallenge]
	if !ok || len(s.Challenges) != 1 {
		t.Fatalf("challenges = %v, want exactly %s", keys(s.Challenges), fixtureChallenge)
	}
	if c.DomainID != fixtureDomain || len(c.Verdicts) != 3 {
		t.Errorf("challenge = %+v", c)
	}
	// Epochs 1 (regresses), 2 (advances at the constant level), 4 (inadmissible): three
	// epochs seen, the latest 4, one since the advance, and an inadmissible latest outcome --
	// an epoch spent is an epoch counted, whatever the session turned out to be worth.
	if fmt.Sprint(c.Epochs) != "[1 2 4]" || c.LatestEpoch != 4 || c.LatestOutcome != "inadmissible" {
		t.Errorf("epochs=%v latest=%d outcome=%s", c.Epochs, c.LatestEpoch, c.LatestOutcome)
	}
	if !c.Advanced || c.LastAdvance != 2 || c.EpochsWithoutAdvance != 1 {
		t.Errorf("advanced=%v last=%d without=%d; want true, 2, 1", c.Advanced, c.LastAdvance, c.EpochsWithoutAdvance)
	}
	adv := c.Verdicts[1]
	if adv.Epoch != 2 || adv.Outcome != "advances" || adv.LevelMoved != "constant" || fmt.Sprint(adv.AdvancesOn) != "[ops memory]" {
		t.Errorf("epoch-2 verdict = %+v", adv)
	}
	if c.Verdicts[0].LevelMoved != "" || c.Verdicts[2].LevelMoved != "" {
		t.Error("a verdict that is not an advance on ops names no level")
	}
	for _, v := range c.Verdicts {
		if !strings.HasPrefix(v.ID, verdictIDPrefix) || v.Path == "" || v.ChallengeID != fixtureChallenge {
			t.Errorf("verdict = %+v", v)
		}
	}

	facts := s.Facts(fixtureChallenge)
	if facts.Known {
		t.Error("frontier facts known with no frontier configured")
	}
	if !facts.ChallengeKnown || facts.ChallengeEpochs != 3 || facts.EpochsWithoutAdvance != 1 || facts.LastOutcome != "inadmissible" {
		t.Errorf("challenge facts = %+v", facts)
	}
}

func TestFactsForATask(t *testing.T) {
	s := load(t, "testdata", Config{Frontier: "frontier.json", Verdicts: "verdicts/*.json"})

	// The verdicts' domain is one of the frontier's: the two documents describe one world.
	if _, ok := s.Frontier.Domain(s.Challenges[fixtureChallenge].DomainID); !ok {
		t.Errorf("verdict domain %s is not a frontier domain", s.Challenges[fixtureChallenge].DomainID)
	}

	working := s.Facts(fixtureChallenge)
	if !working.Known || !working.ChallengeKnown || working.FrontierEntries != 13 || working.ChallengeEpochs != 3 {
		t.Errorf("task working the challenge: %+v", working)
	}
	// A task that names no challenge still sees the frontier; one naming a challenge nobody
	// has run sees the frontier and no challenge -- never zero epochs.
	for _, ref := range []string{"", "ECCH1h000000000000"} {
		f := s.Facts(ref)
		if !f.Known || f.ChallengeKnown || f.ChallengeEpochs != 0 || f.LastOutcome != "" {
			t.Errorf("Facts(%q) = %+v", ref, f)
		}
	}
}

// Every path that cannot produce an answer must produce what a project reading no bounds
// produces: nothing known, no error, so that no threshold in a policy is satisfied. A missing
// document is a worktree without a generated file, not a fact about a frontier.
func TestAbsenceIsUnknownNotZero(t *testing.T) {
	empty := t.TempDir()
	cases := []struct {
		name string
		root string
		cfg  Config
	}{
		{"empty config", "testdata", Config{}},
		{"frontier file missing", empty, Config{Frontier: "frontier.json"}},
		{"verdict glob matches nothing", empty, Config{Verdicts: "verdicts/*.json"}},
		{"both missing", empty, Config{Frontier: "nope/frontier.json", Verdicts: "nope/*.json"}},
		{"absolute paths missing, no root", "", Config{Frontier: filepath.Join(empty, "f.json"), Verdicts: filepath.Join(empty, "*.json")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := load(t, tc.root, tc.cfg)
			if s.Frontier != nil || len(s.Challenges) != 0 {
				t.Errorf("store = %+v, want nothing known", s)
			}
			if f := s.Facts(fixtureChallenge); f.Known || f.ChallengeKnown {
				t.Errorf("facts = %+v, want nothing known", f)
			}
		})
	}
	if f := (*Store)(nil).Facts(fixtureChallenge); f != (policy.BoundsFacts{}) {
		t.Errorf("a nil store knows %+v", f)
	}
	if !(Config{}).Empty() || (Config{Verdicts: "x"}).Empty() {
		t.Error("Config.Empty")
	}
}

// A challenge nothing has advanced has gone every one of its epochs without an advance, and
// that is what the escalation rule reads -- end to end, from documents to a `when` expression.
func TestNeverAdvancedCountsEveryEpoch(t *testing.T) {
	dir := t.TempDir()
	for i, outcome := range []string{"regresses", "matches", "trade"} {
		sealVerdict(t, filepath.Join(dir, fmt.Sprintf("v%d.json", i)), verdict(fixtureChallenge, uint64(i+1), outcome))
	}
	s := load(t, dir, Config{Verdicts: "*.json"})
	c := s.Challenges[fixtureChallenge]
	if c.Advanced || c.EpochsWithoutAdvance != 3 || c.LatestOutcome != "trade" || c.LatestEpoch != 3 {
		t.Errorf("challenge = %+v", c)
	}

	stalled, err := policy.Compile("bounds.challenge_known && bounds.epochs_without_advance >= 3")
	if err != nil {
		t.Fatal(err)
	}
	fire, err := stalled.Bool(policy.Facts{Bounds: s.Facts(fixtureChallenge)}.Env())
	if err != nil || !fire {
		t.Errorf("escalation rule on a never-advanced three-epoch challenge: fire=%v err=%v", fire, err)
	}
	quiet, err := stalled.Bool(policy.Facts{Bounds: s.Facts("ECCH1h000000000000")}.Env())
	if err != nil || quiet {
		t.Errorf("escalation rule on a challenge with no verdicts: fire=%v err=%v", quiet, err)
	}
}

// Epochs are whatever integers nobody has used for the challenge before, so the count of
// epochs after the last advance is what is read, never the difference of two epoch numbers.
func TestEpochsAreCountedNotSubtracted(t *testing.T) {
	dir := t.TempDir()
	sealVerdict(t, filepath.Join(dir, "a.json"), verdict(fixtureChallenge, 2, "advances"))
	sealVerdict(t, filepath.Join(dir, "b.json"), verdict(fixtureChallenge, 7, "regresses"))
	sealVerdict(t, filepath.Join(dir, "c.json"), verdict(fixtureChallenge, 30, "regresses"))
	c := load(t, dir, Config{Verdicts: "*.json"}).Challenges[fixtureChallenge]
	if fmt.Sprint(c.Epochs) != "[2 7 30]" || !c.Advanced || c.LastAdvance != 2 || c.EpochsWithoutAdvance != 2 {
		t.Errorf("challenge = %+v; want epochs [2 7 30], last advance 2, two epochs since", c)
	}
}

// Two verdicts in one epoch are ordered by id, so the latest outcome is deterministic for a
// given set of files; an advance among them still means zero epochs without one. And the same
// verdict under two paths is one verdict: equal ids are equal bytes.
func TestOneEpochTwoVerdictsAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	idA := sealVerdict(t, filepath.Join(dir, "a.json"), verdict(fixtureChallenge, 3, "advances"))
	idB := sealVerdict(t, filepath.Join(dir, "b.json"), verdict(fixtureChallenge, 3, "regresses"))
	sealVerdict(t, filepath.Join(dir, "earlier.json"), verdict(fixtureChallenge, 1, "matches"))
	copyFile(t, filepath.Join(dir, "a.json"), filepath.Join(dir, "a-again.json"))

	c := load(t, dir, Config{Verdicts: "*.json"}).Challenges[fixtureChallenge]
	if len(c.Verdicts) != 3 || fmt.Sprint(c.Epochs) != "[1 3]" || c.EpochsWithoutAdvance != 0 {
		t.Errorf("challenge = %+v; want 3 verdicts over epochs [1 3] with none since the advance", c)
	}
	want := "advances"
	if idB > idA {
		want = "regresses"
	}
	if c.LatestOutcome != want {
		t.Errorf("latest outcome = %s, want %s (the greater id of %s / %s)", c.LatestOutcome, want, idA, idB)
	}
	if c.Verdicts[0].Epoch != 1 || c.Verdicts[1].ID > c.Verdicts[2].ID {
		t.Errorf("verdicts are not ordered by epoch then id: %+v", c.Verdicts)
	}
}

// A document that is present but wrong is an error and nothing known -- not a smaller set of
// facts. A broken verdict under the glob fails the set, because a challenge summarised
// without one of its verdicts may be the one that advanced.
func TestBrokenDocumentsAreErrorsNotSmallerFacts(t *testing.T) {
	good, err := os.ReadFile("testdata/frontier.json")
	if err != nil {
		t.Fatal(err)
	}
	okVerdict, err := os.ReadFile("testdata/verdicts/epoch2-advances.json")
	if err != nil {
		t.Fatal(err)
	}
	minimalFrontier := func(domains []map[string]any) map[string]any {
		return map[string]any{"schema": FrontierSchema, "frontier_id": "", "axes": []string{"ops", "memory"},
			"built_from": []string{}, "domains": domains, "inadmissible": []any{}}
	}
	cases := []struct {
		name  string
		setup func(dir string) Config
		want  string
	}{
		{"frontier tampered", func(dir string) Config {
			write(t, filepath.Join(dir, "f.json"), bytes.Replace(good, []byte(`"family": "prime"`), []byte(`"family": "prime "`), 1))
			return Config{Frontier: "f.json"}
		}, "seal mismatch"},
		{"frontier not json", func(dir string) Config {
			write(t, filepath.Join(dir, "f.json"), []byte("{"))
			return Config{Frontier: "f.json"}
		}, "f.json"},
		{"frontier other schema", func(dir string) Config {
			doc := minimalFrontier(nil)
			doc["schema"] = "ecbench.frontier/v2"
			seal(t, filepath.Join(dir, "f.json"), doc, frontierIDField, frontierIDPrefix)
			return Config{Frontier: "f.json"}
		}, "schema"},
		{"frontier leader not among entries", func(dir string) Config {
			seal(t, filepath.Join(dir, "f.json"), minimalFrontier([]map[string]any{{
				"domain": map[string]any{"family": "prime", "tier": "toy"}, "domain_id": "ECDOM1h000000000000",
				"entries": []any{}, "ops_leader": "ECBND1h000000000000", "memory_leader": nil,
			}}), frontierIDField, frontierIDPrefix)
			return Config{Frontier: "f.json"}
		}, "not among its entries"},
		{"frontier unreadable", func(dir string) Config {
			if err := os.Mkdir(filepath.Join(dir, "f.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			return Config{Frontier: "f.json"}
		}, "f.json"},
		{"verdict tampered", func(dir string) Config {
			write(t, filepath.Join(dir, "v.json"), bytes.Replace(okVerdict, []byte(`"epoch": 2`), []byte(`"epoch": 3`), 1))
			return Config{Verdicts: "*.json"}
		}, "seal mismatch"},
		{"verdict other schema", func(dir string) Config {
			doc := verdict(fixtureChallenge, 1, "advances")
			doc["schema"] = "ecbench.verdict/v2"
			seal(t, filepath.Join(dir, "v.json"), doc, verdictIDField, verdictIDPrefix)
			return Config{Verdicts: "*.json"}
		}, "schema"},
		{"verdict unknown outcome", func(dir string) Config {
			sealVerdict(t, filepath.Join(dir, "v.json"), verdict(fixtureChallenge, 1, "wins"))
			return Config{Verdicts: "*.json"}
		}, `outcome "wins"`},
		{"verdict without challenge", func(dir string) Config {
			sealVerdict(t, filepath.Join(dir, "v.json"), verdict("", 1, "matches"))
			return Config{Verdicts: "*.json"}
		}, "no challenge_id"},
		{"verdict id with another prefix", func(dir string) Config {
			doc := verdict(fixtureChallenge, 1, "matches")
			seal(t, filepath.Join(dir, "v.json"), doc, verdictIDField, "ECVX1h")
			return Config{Verdicts: "*.json"}
		}, "does not start with"},
		{"one challenge, two domains", func(dir string) Config {
			sealVerdict(t, filepath.Join(dir, "a.json"), verdict(fixtureChallenge, 1, "matches"))
			other := verdict(fixtureChallenge, 2, "matches")
			other["domain_id"] = "ECDOM1h080dd3dc639e"
			sealVerdict(t, filepath.Join(dir, "b.json"), other)
			return Config{Verdicts: "*.json"}
		}, "two domains"},
		{"one broken verdict fails the set", func(dir string) Config {
			sealVerdict(t, filepath.Join(dir, "a.json"), verdict(fixtureChallenge, 1, "advances"))
			write(t, filepath.Join(dir, "b.json"), []byte(`{"schema":"ecbench.verdict/v1"}`))
			return Config{Verdicts: "*.json"}
		}, "b.json"},
		{"bad glob", func(string) Config { return Config{Verdicts: "["} }, "syntax error in pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := tc.setup(dir)
			s, err := Load(dir, cfg)
			if err == nil {
				t.Fatalf("Load succeeded with %+v; want an error mentioning %q", s, tc.want)
			}
			if s != nil {
				t.Errorf("store = %+v beside an error; want nil so nothing half-read is believed", s)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// --- helpers ---------------------------------------------------------------------------

// verdict is the part of an `ecbench.verdict/v1` document this reader reads, with the id
// blank for sealing. The on-disk fixtures carry the whole field set; these carry enough.
func verdict(challenge string, epoch uint64, outcome string) map[string]any {
	advancesOn := []string{}
	var level any
	if outcome == "advances" {
		advancesOn, level = []string{"ops"}, "constant"
	}
	return map[string]any{
		"schema": VerdictSchema, "verdict_id": "", "challenge_id": challenge, "epoch": epoch,
		"domain_id": fixtureDomain, "outcome": outcome, "advances_on": advancesOn, "regresses_on": []string{},
		"level_moved": level, "reasons": []string{}, "statement": "FIXTURE written by bounds_test.go",
	}
}

func sealVerdict(t *testing.T, path string, doc map[string]any) string {
	t.Helper()
	return seal(t, path, doc, verdictIDField, verdictIDPrefix)
}

// seal writes doc the way the harness's `seal_document` does: the id field blank, the bytes
// hashed, prefix + twelve hex digits written in. Returns the id.
func seal(t *testing.T, path string, doc map[string]any, field, prefix string) string {
	t.Helper()
	doc[field] = ""
	blank, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	blank = append(blank, '\n')
	needle := []byte(fmt.Sprintf("%q: \"\"", field))
	if bytes.Count(blank, needle) != 1 {
		t.Fatalf("fixture must carry %s exactly once", needle)
	}
	sum := sha256.Sum256(blank)
	id := prefix + hex.EncodeToString(sum[:])[:12]
	write(t, path, bytes.Replace(blank, needle, []byte(fmt.Sprintf("%q: %q", field, id)), 1))
	return id
}

func write(t *testing.T, path string, text []byte) {
	t.Helper()
	if err := os.WriteFile(path, text, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	write(t, to, b)
}

func keys(m map[string]Challenge) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
