// Package bounds reads the sealed documents of the ecbench bounds protocol -- a frontier of
// measured ECDLP method costs and the verdicts of challenges against it -- for routing
// policies that want to spend model capacity where a bound is beatable and escalate where a
// challenge has stalled.
//
// Read-only, from files. A frontier (`ecbench.frontier/v1`) is built from committed bound
// records and never edited; a verdict (`ecbench.verdict/v1`) is one audited paired session
// judged against a frozen challenge, and judging it again yields the same id. Both are
// content-addressed: the id is the SHA-256 of the document's own bytes with the id field
// blank, so a changed byte is a different record. The seal is checked here before anything is
// read from a document, exactly as the harness's own `read_sealed` does.
//
// Everything here is therefore a fact anyone holding the records can re-derive. That is what
// makes it admissible in a routing decision: conductor's policies read deterministic facts,
// never a model's account of its own work. The protocol is aburan28/crypto
// docs/bounds/README.md; how the facts are used is docs/bounds-integration.md here.
package bounds

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aburan28/conductor/internal/policy"
)

// Schemas this reader understands. A document carrying any other schema is an error, not a
// smaller set of facts: the fields read here are the fields of these versions.
const (
	FrontierSchema = "ecbench.frontier/v1"
	VerdictSchema  = "ecbench.verdict/v1"

	frontierIDField, frontierIDPrefix = "frontier_id", "ECFR1h"
	verdictIDField, verdictIDPrefix   = "verdict_id", "ECVD1h"
)

// Outcomes a verdict may carry. The set is closed within the schema version.
var outcomes = map[string]bool{
	"advances": true, "trade": true, "matches": true, "regresses": true, "inadmissible": true,
}

// Config is the optional `bounds:` block of .conductor/project.yaml.
type Config struct {
	// Frontier is the path of a frontier.json, relative to the repository root unless
	// absolute. Empty means the project reads no frontier.
	Frontier string
	// Verdicts is a glob of verdict files in filepath.Match syntax (one `*` per path element;
	// there is no `**`), relative to the repository root unless absolute. Empty means the
	// project reads no verdicts.
	Verdicts string
}

// Empty reports whether the block names nothing.
func (c Config) Empty() bool { return c.Frontier == "" && c.Verdicts == "" }

// Store is what Load read: at most one frontier, and every challenge with a verdict.
type Store struct {
	// Frontier is nil when no frontier document was read.
	Frontier *Frontier
	// Challenges holds every challenge at least one verdict was read for, by challenge id.
	Challenges map[string]Challenge
}

// Frontier is the part of an `ecbench.frontier/v1` document a routing decision can use.
type Frontier struct {
	ID   string
	Axes []string
	// BuiltFrom counts the bound records the frontier was built from, admissible or not.
	BuiltFrom int
	// Inadmissible counts the bounds listed as never eligible for a frontier.
	Inadmissible int
	Domains      []Domain
}

// Entries counts every bound the frontier lists, on the frontier or dominated.
func (f *Frontier) Entries() int {
	n := 0
	for _, d := range f.Domains {
		n += d.Entries
	}
	return n
}

// FrontierEntries counts the bounds on the frontier across every domain: those nothing
// dominates. Ties stand, so a domain may have several.
func (f *Frontier) FrontierEntries() int {
	n := 0
	for _, d := range f.Domains {
		n += d.FrontierEntries
	}
	return n
}

// Domain finds a domain by id.
func (f *Frontier) Domain(id string) (Domain, bool) {
	for _, d := range f.Domains {
		if d.ID == id {
			return d, true
		}
	}
	return Domain{}, false
}

// Domain is one domain of a frontier: bounds compare only inside one.
type Domain struct {
	ID      string
	Problem string
	Family  string
	Tier    string
	Unit    string
	// Entries counts the bounds listed in the domain; FrontierEntries those on its frontier.
	Entries         int
	FrontierEntries int
	// OpsLeader is the frontier entry with the least ops value (the ratio of its measured
	// constant to the generic floor); MemoryLeader the one storing the fewest table entries
	// per √r. Known is false when the domain names none.
	OpsLeader    Leader
	MemoryLeader Leader
}

// Leader is the bound leading a domain on one axis, and its value on that axis.
type Leader struct {
	Known    bool
	BoundID  string
	Method   string
	MethodID string
	Value    float64
}

// Verdict is the part of an `ecbench.verdict/v1` document a routing decision can use.
type Verdict struct {
	ID          string
	ChallengeID string
	DomainID    string
	Epoch       uint64
	// Outcome is advances, trade, matches, regresses or inadmissible.
	Outcome string
	// AdvancesOn lists the deciding axes the candidate was clearly better on.
	AdvancesOn []string
	// LevelMoved is `exponent` or `constant` when the advance includes ops, else empty: a
	// level is a statement about operations.
	LevelMoved string
	// Path is the file the verdict was read from.
	Path string
}

// Challenge summarises every verdict read for one challenge.
//
// Epochs are counted, never subtracted. A challenge's epochs are whatever integers nobody has
// used for it before, so the gap between two epoch numbers says nothing about how many
// sessions were run; the number of distinct epochs holding a verdict does.
type Challenge struct {
	ID       string
	DomainID string
	// Verdicts is every verdict read, ordered by epoch and then by id.
	Verdicts []Verdict
	// Epochs is every distinct epoch holding a verdict, ascending.
	Epochs      []uint64
	LatestEpoch uint64
	// LatestOutcome is the outcome of the last verdict in Verdicts order. Two verdicts in one
	// epoch are ordered by id, which is deterministic for a given set of files and nothing
	// more; an `advances` among them still resets EpochsWithoutAdvance to zero.
	LatestOutcome string
	// Advanced reports that some verdict's outcome is `advances`; LastAdvance is the greatest
	// epoch holding one.
	Advanced    bool
	LastAdvance uint64
	// EpochsWithoutAdvance is the number of distinct epochs later than LastAdvance -- every
	// epoch, when nothing has advanced yet.
	EpochsWithoutAdvance int
}

// Load reads the documents cfg names, resolving relative paths against root.
//
// Absence is not an error and yields nothing known: an empty Config, a frontier path with no
// file behind it, or a verdict glob matching nothing each produce a Store whose Facts render
// every bounds name as nil -- the same shape as "this project reads no bounds", and
// deliberately so, because routing while a document is missing should fall back to the
// ordinary ladder rather than act on a guess about a frontier.
//
// A document that exists but cannot be read, does not seal, carries another schema, or says
// something this reader does not understand is an error, because that is a misconfiguration
// somebody has to fix; the Store is then nil, so no half-read set of verdicts can make a
// challenge look stalled or fresh. One broken file under the glob fails the whole set for the
// reason the harness's own loader gives: a frontier built from a directory with a broken
// record is wrong, not smaller.
func Load(root string, cfg Config) (*Store, error) {
	s := &Store{Challenges: map[string]Challenge{}}
	if cfg.Frontier != "" {
		f, err := readFrontier(resolve(root, cfg.Frontier))
		if err != nil {
			return nil, err
		}
		s.Frontier = f
	}
	if cfg.Verdicts != "" {
		verdicts, err := readVerdicts(resolve(root, cfg.Verdicts))
		if err != nil {
			return nil, err
		}
		challenges, err := summarise(verdicts)
		if err != nil {
			return nil, err
		}
		s.Challenges = challenges
	}
	return s, nil
}

// Facts renders the Store for one task. challengeID is the task's external_ref: the
// `ECCH1h…` id of the challenge it works, or empty for a task that works none.
//
// The frontier names are known whenever a frontier was read, whatever the task; the challenge
// names only when the task names a challenge that has at least one verdict. A nil Store
// knows nothing, which is what a caller holding a Load error should pass on.
func (s *Store) Facts(challengeID string) policy.BoundsFacts {
	var f policy.BoundsFacts
	if s == nil {
		return f
	}
	if s.Frontier != nil {
		f.Known = true
		f.FrontierID = s.Frontier.ID
		f.DomainCount = len(s.Frontier.Domains)
		f.FrontierEntries = s.Frontier.FrontierEntries()
	}
	if c, ok := s.Challenges[challengeID]; ok && challengeID != "" {
		f.ChallengeKnown = true
		f.ChallengeEpochs = len(c.Epochs)
		f.EpochsWithoutAdvance = c.EpochsWithoutAdvance
		f.LastOutcome = c.LatestOutcome
	}
	return f
}

func resolve(root, path string) string {
	if root == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}

// ---------------------------------------------------------------------------
// Frontier
// ---------------------------------------------------------------------------

type frontierDoc struct {
	Schema       string            `json:"schema"`
	FrontierID   string            `json:"frontier_id"`
	Axes         []string          `json:"axes"`
	BuiltFrom    []string          `json:"built_from"`
	Domains      []domainDoc       `json:"domains"`
	Inadmissible []json.RawMessage `json:"inadmissible"`
}

type domainDoc struct {
	Domain struct {
		Problem string `json:"problem"`
		Family  string `json:"family"`
		Tier    string `json:"tier"`
		Unit    string `json:"unit"`
	} `json:"domain"`
	DomainID     string     `json:"domain_id"`
	Entries      []entryDoc `json:"entries"`
	OpsLeader    *string    `json:"ops_leader"`
	MemoryLeader *string    `json:"memory_leader"`
}

type entryDoc struct {
	BoundID    string `json:"bound_id"`
	Method     string `json:"method"`
	MethodID   string `json:"method_id"`
	IsFrontier bool   `json:"is_frontier"`
	Axes       map[string]struct {
		Known bool     `json:"known"`
		Value *float64 `json:"value"`
	} `json:"axes"`
}

// readFrontier reads one frontier document. A missing file is (nil, nil): the frontier is
// generated, and a worktree may simply not hold it.
func readFrontier(path string) (*Frontier, error) {
	text, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bounds frontier: %w", err)
	}
	var doc frontierDoc
	if err := json.Unmarshal(text, &doc); err != nil {
		return nil, fmt.Errorf("bounds frontier %s: %w", path, err)
	}
	if doc.Schema != FrontierSchema {
		return nil, fmt.Errorf("bounds frontier %s: schema %q is not %s", path, doc.Schema, FrontierSchema)
	}
	if _, err := checkSeal(text, frontierIDField, frontierIDPrefix); err != nil {
		return nil, fmt.Errorf("bounds frontier %s: %w", path, err)
	}
	f := &Frontier{
		ID:           doc.FrontierID,
		Axes:         doc.Axes,
		BuiltFrom:    len(doc.BuiltFrom),
		Inadmissible: len(doc.Inadmissible),
	}
	for _, dd := range doc.Domains {
		d := Domain{
			ID:      dd.DomainID,
			Problem: dd.Domain.Problem,
			Family:  dd.Domain.Family,
			Tier:    dd.Domain.Tier,
			Unit:    dd.Domain.Unit,
			Entries: len(dd.Entries),
		}
		for _, e := range dd.Entries {
			if e.IsFrontier {
				d.FrontierEntries++
			}
		}
		if d.OpsLeader, err = leader(dd, dd.OpsLeader, "ops"); err != nil {
			return nil, fmt.Errorf("bounds frontier %s: %w", path, err)
		}
		if d.MemoryLeader, err = leader(dd, dd.MemoryLeader, "memory"); err != nil {
			return nil, fmt.Errorf("bounds frontier %s: %w", path, err)
		}
		f.Domains = append(f.Domains, d)
	}
	return f, nil
}

// leader resolves a domain's leader on one axis. A domain naming no leader is a domain with
// nothing on that axis (Known false); a domain naming a bound it does not list, or one whose
// value on the axis is unknown, is a broken document, because the builder chose the leader
// among its own entries by that very value.
func leader(d domainDoc, id *string, axis string) (Leader, error) {
	if id == nil || *id == "" {
		return Leader{}, nil
	}
	for _, e := range d.Entries {
		if e.BoundID != *id {
			continue
		}
		a, ok := e.Axes[axis]
		if !ok || !a.Known || a.Value == nil {
			return Leader{}, fmt.Errorf("domain %s: %s leader %s has no known %s value", d.DomainID, axis, *id, axis)
		}
		return Leader{Known: true, BoundID: e.BoundID, Method: e.Method, MethodID: e.MethodID, Value: *a.Value}, nil
	}
	return Leader{}, fmt.Errorf("domain %s: %s leader %s is not among its entries", d.DomainID, axis, *id)
}

// ---------------------------------------------------------------------------
// Verdicts
// ---------------------------------------------------------------------------

type verdictDoc struct {
	Schema      string   `json:"schema"`
	VerdictID   string   `json:"verdict_id"`
	ChallengeID string   `json:"challenge_id"`
	Epoch       uint64   `json:"epoch"`
	DomainID    string   `json:"domain_id"`
	Outcome     string   `json:"outcome"`
	AdvancesOn  []string `json:"advances_on"`
	LevelMoved  *string  `json:"level_moved"`
}

// readVerdicts reads every file the glob matches, in path order. A glob matching nothing is
// no verdicts. The same verdict under two paths is read once: equal ids are equal bytes.
func readVerdicts(pattern string) ([]Verdict, error) {
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("bounds verdicts %q: %w", pattern, err)
	}
	sort.Strings(paths)
	var out []Verdict
	seen := map[string]bool{}
	for _, p := range paths {
		v, err := readVerdict(p)
		if err != nil {
			return nil, err
		}
		if seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		out = append(out, v)
	}
	return out, nil
}

func readVerdict(path string) (Verdict, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		return Verdict{}, fmt.Errorf("bounds verdict: %w", err)
	}
	var doc verdictDoc
	if err := json.Unmarshal(text, &doc); err != nil {
		return Verdict{}, fmt.Errorf("bounds verdict %s: %w", path, err)
	}
	if doc.Schema != VerdictSchema {
		return Verdict{}, fmt.Errorf("bounds verdict %s: schema %q is not %s", path, doc.Schema, VerdictSchema)
	}
	if _, err := checkSeal(text, verdictIDField, verdictIDPrefix); err != nil {
		return Verdict{}, fmt.Errorf("bounds verdict %s: %w", path, err)
	}
	if doc.ChallengeID == "" {
		return Verdict{}, fmt.Errorf("bounds verdict %s: no challenge_id", path)
	}
	if !outcomes[doc.Outcome] {
		return Verdict{}, fmt.Errorf("bounds verdict %s: outcome %q is not one this reader knows", path, doc.Outcome)
	}
	v := Verdict{
		ID:          doc.VerdictID,
		ChallengeID: doc.ChallengeID,
		DomainID:    doc.DomainID,
		Epoch:       doc.Epoch,
		Outcome:     doc.Outcome,
		AdvancesOn:  doc.AdvancesOn,
		Path:        path,
	}
	if doc.LevelMoved != nil {
		v.LevelMoved = *doc.LevelMoved
	}
	return v, nil
}

// summarise groups verdicts by challenge and derives each challenge's position.
func summarise(verdicts []Verdict) (map[string]Challenge, error) {
	byChallenge := map[string][]Verdict{}
	for _, v := range verdicts {
		byChallenge[v.ChallengeID] = append(byChallenge[v.ChallengeID], v)
	}
	out := make(map[string]Challenge, len(byChallenge))
	for id, vs := range byChallenge {
		sort.Slice(vs, func(i, j int) bool {
			if vs[i].Epoch != vs[j].Epoch {
				return vs[i].Epoch < vs[j].Epoch
			}
			return vs[i].ID < vs[j].ID
		})
		c := Challenge{ID: id, DomainID: vs[0].DomainID, Verdicts: vs}
		for _, v := range vs {
			// A challenge is in exactly one domain. Verdicts for one challenge naming two are
			// not both verdicts of that challenge, and neither can be trusted.
			if v.DomainID != c.DomainID {
				return nil, fmt.Errorf("bounds verdicts for challenge %s name two domains, %s (%s) and %s (%s)",
					id, c.DomainID, vs[0].Path, v.DomainID, v.Path)
			}
			if len(c.Epochs) == 0 || c.Epochs[len(c.Epochs)-1] != v.Epoch {
				c.Epochs = append(c.Epochs, v.Epoch)
			}
			if v.Outcome == "advances" {
				c.Advanced, c.LastAdvance = true, v.Epoch
			}
		}
		c.LatestEpoch = vs[len(vs)-1].Epoch
		c.LatestOutcome = vs[len(vs)-1].Outcome
		for _, e := range c.Epochs {
			if !c.Advanced || e > c.LastAdvance {
				c.EpochsWithoutAdvance++
			}
		}
		out[id] = c
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Seals
// ---------------------------------------------------------------------------

// checkSeal verifies a sealed document the way the harness's `check_document_seal` does: the
// id field must appear exactly once as written, `"field": "id"`, and the SHA-256 of the
// document's bytes with that value blank must give the id's twelve hex digits after its
// prefix. Bytes, not a parsed value: a float parsed and written again can differ in a last
// digit from what was sealed, and the seal is over what was written.
func checkSeal(text []byte, field, prefix string) (string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(text, &top); err != nil {
		return "", err
	}
	var id string
	if raw, ok := top[field]; !ok || json.Unmarshal(raw, &id) != nil {
		return "", fmt.Errorf("no %s", field)
	}
	if !strings.HasPrefix(id, prefix) {
		return "", fmt.Errorf("%s %q does not start with %s", field, id, prefix)
	}
	filled := []byte(fmt.Sprintf("%q: %q", field, id))
	if bytes.Count(text, filled) != 1 {
		return "", fmt.Errorf("%s must appear exactly once as written", field)
	}
	blank := bytes.Replace(text, filled, []byte(fmt.Sprintf("%q: \"\"", field)), 1)
	sum := sha256.Sum256(blank)
	want := prefix + hex.EncodeToString(sum[:])[:12]
	if want != id {
		return "", fmt.Errorf("seal mismatch: bytes hash to %s, document says %s", want, id)
	}
	return id, nil
}
