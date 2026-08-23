// Package tokencost estimates what a file costs in LLM tokens.
//
// Agent harnesses silently load standing context on every session: AGENTS.md, CLAUDE.md,
// workflow rules, READMEs — and then whatever code the agent opens. Each of those reads is
// billed against a token budget, and until now nothing in Conductor could answer "how much?".
// This package answers it two ways:
//
//   - Estimate: a local, deterministic approximation. Content never leaves the machine,
//     no credentials are needed, and the same input always yields the same number — which
//     is what a coordination tool should prefer (DESIGN.md's rule against false precision).
//   - exact.go: the real count from a provider's tokenizer endpoint, opt-in, using the
//     caller's own credentials from their own machine. Content goes to the model provider —
//     exactly where the harness already sends it — and never to the Conductor server.
package tokencost

import (
	"bytes"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Estimate approximates how many tokens a BPE-family tokenizer (Claude, GPT, and kin)
// produces for text.
//
// It is a character-class mixture rather than a vocabulary: letters compress well
// (~3.9 chars/token in English), digits less (~2.7), punctuation barely (~1.6, since only
// common operators merge), spaces mostly ride along with the next word (~8), and newlines
// usually merge with indentation (~1.2). Because the mixture drives the answer, prose lands
// near the familiar 4 chars/token and dense code or JSON near 3 — with no per-language
// tables to maintain. Non-ASCII is charged conservatively at one token per rune.
//
// Expect ±15-20% against any specific model's tokenizer. That is accurate enough to compare
// files, catch a 40k-token AGENTS.md, or size a read against a member budget; use the exact
// counter when the precise number matters.
func Estimate(text string) int64 {
	if text == "" {
		return 0
	}
	var score float64
	for _, r := range text {
		switch {
		case r == '\n':
			score += 1.0 / 1.2
		case r == ' ' || r == '\t' || r == '\r':
			score += 1.0 / 8.0
		case r < 128 && unicode.IsLetter(r):
			score += 1.0 / 3.9
		case r < 128 && unicode.IsDigit(r):
			score += 1.0 / 2.7
		case r < 128:
			score += 1.0 / 1.6
		default:
			score += 1.0
		}
	}
	return int64(math.Max(1, math.Ceil(score)))
}

// FileCost is one file's position in the context bill.
type FileCost struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Tokens int64  `json:"tokens"`
}

// File estimates one file by path. Binary content is refused rather than mis-counted: a
// tokenizer never sees raw bytes, so a number for them would be fiction.
func File(path string) (FileCost, bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return FileCost{}, false, err
	}
	if isBinary(body) {
		return FileCost{Path: path, Bytes: int64(len(body))}, false, nil
	}
	return FileCost{Path: path, Bytes: int64(len(body)), Tokens: Estimate(string(body))}, true, nil
}

// isBinary sniffs the way git does: a NUL byte in the first block means not text.
func isBinary(body []byte) bool {
	head := body
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// skipDirs are trees no harness reads as context: VCS internals and dependency caches.
// Conductor's own runtime worktrees (.conductor/runtime) are skipped by path in Walk —
// counting those would count every live branch twice.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"target": true, "dist": true, ".next": true, "__pycache__": true,
}

// walkMaxFileBytes bounds what a directory sweep will read per file. Anything larger is not
// context an agent reads whole; explicit single-file requests via File have no cap.
const walkMaxFileBytes = 4 << 20

// Walk estimates every text file under root, returning costs sorted most-expensive-first
// plus the number of files skipped as binary or oversized. Paths are root-relative.
func Walk(root string) ([]FileCost, int, error) {
	var out []FileCost
	skipped := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			dotted := strings.HasPrefix(d.Name(), ".") && d.Name() != ".conductor" && d.Name() != ".github"
			if rel, err := filepath.Rel(root, path); err == nil && rel == filepath.Join(".conductor", "runtime") {
				return filepath.SkipDir
			}
			if skipDirs[d.Name()] || dotted {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > walkMaxFileBytes {
			skipped++
			return nil
		}
		cost, ok, err := File(path)
		if err != nil {
			return err
		}
		if !ok {
			skipped++
			return nil
		}
		if rel, err := filepath.Rel(root, path); err == nil {
			cost.Path = rel
		}
		out = append(out, cost)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return out[i].Path < out[j].Path
	})
	return out, skipped, nil
}

// rootInstructionFiles are the standing-context files harnesses load from the repository
// root, in the order a person expects to see them. Only files that exist are reported.
var rootInstructionFiles = []string{
	"AGENTS.md",
	"CLAUDE.md",
	"CLAUDE.local.md",
	"GEMINI.md",
	".cursorrules",
	".windsurfrules",
	".github/copilot-instructions.md",
	".conductor/WORKFLOW.md",
	"README.md",
}

// nestedInstructionNames are instruction files harnesses also pick up from subdirectories
// as the agent moves through the tree.
var nestedInstructionNames = map[string]bool{"AGENTS.md": true, "CLAUDE.md": true}

// InstructionFiles finds the files an agent harness loads as standing context: the known
// root-level instruction files, plus nested AGENTS.md/CLAUDE.md anywhere below. This is the
// recurring cost — paid on every session, before any useful work happens.
func InstructionFiles(root string) ([]FileCost, error) {
	seen := map[string]bool{}
	var out []FileCost

	add := func(rel string) error {
		if seen[rel] {
			return nil
		}
		cost, ok, err := File(filepath.Join(root, rel))
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !ok {
			return nil
		}
		cost.Path = rel
		seen[rel] = true
		out = append(out, cost)
		return nil
	}

	for _, rel := range rootInstructionFiles {
		if err := add(rel); err != nil {
			return nil, err
		}
	}

	var nested []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == d.Name() {
			return nil // root-level files were handled by the ordered list above
		}
		if nestedInstructionNames[d.Name()] {
			nested = append(nested, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(nested)
	for _, rel := range nested {
		if err := add(rel); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Total sums the token column.
func Total(files []FileCost) int64 {
	var total int64
	for _, f := range files {
		total += f.Tokens
	}
	return total
}
