// Package privacy implements the intent envelope of DESIGN.md §12 and the field-level
// visibility rules of §24.3.
//
// The problem this package solves: two people on a team can be building the same thing, and
// the system should say so — without either of them being able to read the other's prompts,
// and without the server storing anyone's plaintext for private work.
//
// The mechanism is HMAC-under-a-per-tenant-key plus MinHash. An HMAC is one-way, so a stored
// fingerprint reveals nothing about its input. MinHash over HMAC'd shingles preserves enough
// structure to estimate Jaccard similarity between two token sets while each individual
// hashed shingle stays opaque. The result is fuzzy duplicate detection with no plaintext and
// no model call.
package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// FingerprintVersion is part of every fingerprint string. Canonicalization is versioned
// (DESIGN.md §12.2) so that changing the algorithm cannot silently make old fingerprints
// compare unequal to new ones without anybody noticing — they simply stop matching, visibly.
const FingerprintVersion = "v1"

// MinHashLanes is the signature width. 64 lanes gives a standard error of roughly
// 1/sqrt(64) = 12.5% on the similarity estimate, which is comfortably precise enough to
// separate "the same work" (>0.6) from "adjacent work" (<0.3) while fitting in one
// Postgres bigint[] of modest size.
const MinHashLanes = 64

// The similarity basis is the set of normalized single tokens, not multi-token windows.
//
// Word-order-sensitive shingles were tried first and are wrong for this problem: "Implement
// the team invite flow with email invitations" and "Build team invitation flow: send invite
// emails" describe the same work and share zero two-token windows. Two people independently
// starting the same task almost never phrase it the same way, so anything that depends on
// phrasing fails exactly when it matters most.

// NewDedupeKey generates a per-organization HMAC key (DESIGN.md §25.6: per-tenant dedupe
// keys, so fingerprints are not comparable across tenants).
func NewDedupeKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// Envelope is the sanitized description of intended work. It deliberately has no field for a
// prompt: the caller supplies a summary it has chosen to share, and the scopes it intends to
// touch. Nothing here is derived from a conversation by the server.
type Envelope struct {
	Summary     string
	Scopes      []string
	ExternalRef string
}

// Canonical renders the envelope into the versioned canonical form that is hashed.
//
// Canonicalization is order-insensitive over both tokens and scopes: "add retry-aware model
// routing" and "model routing: retry aware, added" produce the same canonical string, so
// they produce the same fingerprint. That is the point — two people describing the same work
// in different words should collide.
func (e Envelope) Canonical() string {
	tokens := uniqueSorted(Tokenize(e.Summary))

	scopes := make([]string, 0, len(e.Scopes))
	for _, s := range e.Scopes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			scopes = append(scopes, s)
		}
	}
	scopes = uniqueSorted(scopes)

	var b strings.Builder
	b.WriteString(FingerprintVersion)
	b.WriteString("|s:")
	b.WriteString(strings.Join(tokens, " "))
	b.WriteString("|r:")
	b.WriteString(strings.Join(scopes, ","))
	if e.ExternalRef != "" {
		b.WriteString("|x:")
		b.WriteString(strings.ToLower(strings.TrimSpace(e.ExternalRef)))
	}
	return b.String()
}

// Fingerprint returns the exact-match fingerprint of the envelope under the tenant key.
//
// Equality of two fingerprints means the canonical intents were identical. Inequality means
// nothing about how similar they were — that is what the MinHash signature is for.
func (e Envelope) Fingerprint(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(e.Canonical()))
	return "hmac-sha256:" + FingerprintVersion + ":" + hex.EncodeToString(mac.Sum(nil))
}

// Signature computes the MinHash signature over HMAC'd shingles of the envelope.
//
// Returns nil when the summary carries too little content to shingle. A nil signature never
// matches anything, which is the safe default: a task with no description should not be
// reported as a duplicate of every other undescribed task.
func (e Envelope) Signature(key []byte) []int64 {
	shingles := Shingles(e.Summary)
	// Scopes participate in similarity too. Two tasks reserving the same files are similar
	// work even when their prose differs.
	for _, s := range e.Scopes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" {
			shingles = append(shingles, "scope:"+s)
		}
	}
	if len(shingles) == 0 {
		return nil
	}
	return minhash(key, shingles)
}

// Tokenize normalizes text into comparable lowercase word tokens, dropping punctuation and
// stopwords. It is the only place text is inspected, and it never leaves this package.
func Tokenize(s string) []string {
	s = norm.NFC.String(strings.ToLower(s))

	fields := strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 2 || stopwords[f] {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

// Shingles returns the distinct normalized tokens that form the similarity basis.
func Shingles(s string) []string {
	return uniqueSorted(Tokenize(s))
}

// minhash computes the signature. Each shingle is HMAC'd once to a 64-bit base value, then
// permuted per lane by an odd multiplier and an additive constant, both derived from the
// tenant key. Deriving the permutations from the key means two tenants produce different
// signatures for identical text, so signatures cannot be correlated across tenants.
func minhash(key []byte, shingles []string) []int64 {
	mulA, addB := lanes(key)

	sig := make([]uint64, MinHashLanes)
	for i := range sig {
		sig[i] = ^uint64(0)
	}

	mac := hmac.New(sha256.New, key)
	for _, sh := range shingles {
		mac.Reset()
		mac.Write([]byte(sh))
		base := binary.BigEndian.Uint64(mac.Sum(nil)[:8])
		for i := 0; i < MinHashLanes; i++ {
			// Multiply-shift permutation; the multiplier is forced odd so the map is a
			// bijection over uint64 and no lane collapses.
			v := base*mulA[i] + addB[i]
			if v < sig[i] {
				sig[i] = v
			}
		}
	}

	out := make([]int64, MinHashLanes)
	for i, v := range sig {
		out[i] = int64(v) // bit-preserving; Postgres bigint is signed
	}
	return out
}

// lanes derives the per-lane permutation constants from the tenant key.
func lanes(key []byte) (mul, add [MinHashLanes]uint64) {
	mac := hmac.New(sha256.New, key)
	for i := 0; i < MinHashLanes; i++ {
		mac.Reset()
		mac.Write([]byte("conductor/minhash/" + FingerprintVersion + "/lane"))
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(i))
		mac.Write(b[:])
		sum := mac.Sum(nil)
		mul[i] = binary.BigEndian.Uint64(sum[0:8]) | 1 // odd
		add[i] = binary.BigEndian.Uint64(sum[8:16])
	}
	return mul, add
}

// Similarity estimates the Jaccard similarity of the two underlying shingle sets as the
// fraction of lanes that agree. Signatures of different widths, or either being empty,
// yield 0.
func Similarity(a, b []int64) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	matches := 0
	for i := range a {
		if a[i] == b[i] {
			matches++
		}
	}
	return float64(matches) / float64(len(a))
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// suffixRules is an ordered longest-first list of derivational and inflectional endings to
// strip, each with the minimum stem length that must remain.
//
// This is not a Porter stemmer and does not try to be. It exists to collapse the specific
// variation that shows up when two engineers describe the same task: invite / invitation /
// invitations, accept / acceptance, route / router / routing. A full stemmer would be
// language-specific, a dependency, and no more accurate on the vocabulary that actually
// appears in task titles.
var suffixRules = []struct {
	suffix  string
	replace string
	minStem int
}{
	{"ations", "", 4}, {"ation", "", 4},
	{"ances", "", 4}, {"ance", "", 4},
	{"ences", "", 4}, {"ence", "", 4},
	{"ments", "", 4}, {"ment", "", 4},
	{"ings", "", 4}, {"ing", "", 4},
	{"ions", "", 4}, {"ion", "", 4},
	{"ies", "y", 3},
	{"ers", "", 4}, {"er", "", 4},
	{"ed", "", 3},
	{"es", "", 3},
	{"s", "", 3},
}

func stem(w string) string {
	for _, r := range suffixRules {
		if !strings.HasSuffix(w, r.suffix) {
			continue
		}
		// "ss" is not a plural; "class" must not become "clas".
		if r.suffix == "s" && strings.HasSuffix(w, "ss") {
			continue
		}
		stemmed := w[:len(w)-len(r.suffix)] + r.replace
		if len(stemmed) >= r.minStem {
			w = stemmed
		}
		break
	}
	// A trailing silent "e" is what keeps "invite" from meeting "invitation" at "invit".
	if len(w) > 4 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}

// stopwords removes filler that would otherwise inflate similarity between unrelated tasks.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"from": true, "into": true, "onto": true, "our": true, "its": true, "was": true,
	"are": true, "were": true, "will": true, "would": true, "should": true, "could": true,
	"have": true, "has": true, "had": true, "not": true, "but": true, "all": true,
	"any": true, "can": true, "when": true, "then": true, "than": true, "them": true,
	"they": true, "there": true, "these": true, "those": true, "some": true, "such": true,
	"only": true, "also": true, "just": true, "make": true, "made": true, "use": true,
	"using": true, "via": true, "per": true, "out": true, "off": true, "over": true,
	"under": true, "about": true, "after": true, "before": true, "while": true,
}
