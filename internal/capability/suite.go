// Package capability is Delphi's capability registry (docs/DELPHI_OBJECTIVES.md §3): frozen
// calibration suites with deterministic graders, the calibration records their runs produce,
// public benchmark evidence kept with its source and date, and the statistics that turn them
// into per-family tables of pass rates and equivalence verdicts.
package capability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Schema names, one per record kind.
const (
	SuiteSchema       = "conductor.capability.suite.v1"
	CalibrationSchema = "conductor.capability.calibration.v1"
	EvidenceSchema    = "conductor.capability.evidence.v1"
)

// Grader decides whether one answer passes. Kinds:
//
//	exact    the trimmed answer equals the item's expect
//	regex    the item's expect is a regular expression the answer must match
//	command  argv runs in the suite's directory with the item and answer as JSON on stdin
//	         ({suite, item, prompt, expect, answer}) and prints {"passed": bool, "reason": "..."}
type Grader struct {
	Kind           string   `json:"kind"`
	Argv           []string `json:"argv,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// Item is one calibration question.
type Item struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Expect string `json:"expect,omitempty"`
}

// Suite is a frozen set of items for one task family. Its sha256 is part of every
// calibration record, so a changed suite is a different measurement.
type Suite struct {
	Schema    string `json:"schema"`
	ID        string `json:"id"`
	Family    string `json:"family"`
	System    string `json:"system,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Grader    Grader `json:"grader"`
	Items     []Item `json:"items"`

	Path   string `json:"-"`
	SHA256 string `json:"-"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// LoadSuite reads and validates a suite file.
func LoadSuite(path string) (Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, err
	}
	var s Suite
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Suite{}, fmt.Errorf("%s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	s.Path, s.SHA256 = path, hex.EncodeToString(sum[:])
	if problems := s.Validate(); len(problems) > 0 {
		return Suite{}, fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	return s, nil
}

// Validate lists what is wrong with s.
func (s Suite) Validate() []string {
	var out []string
	if s.Schema != SuiteSchema {
		out = append(out, fmt.Sprintf("schema must be %s", SuiteSchema))
	}
	if !idPattern.MatchString(s.ID) {
		out = append(out, "id must be lowercase letters, digits, '.', '_' or '-'")
	}
	if !idPattern.MatchString(s.Family) {
		out = append(out, "family must be lowercase letters, digits, '.', '_' or '-'")
	}
	switch s.Grader.Kind {
	case "exact", "regex":
		if len(s.Grader.Argv) > 0 {
			out = append(out, "grader.argv is only for kind command")
		}
	case "command":
		if len(s.Grader.Argv) == 0 {
			out = append(out, "grader kind command needs argv")
		}
	default:
		out = append(out, "grader.kind must be exact, regex or command")
	}
	if len(s.Items) == 0 {
		out = append(out, "a suite needs items")
	}
	seen := map[string]bool{}
	for i, it := range s.Items {
		switch {
		case !idPattern.MatchString(it.ID):
			out = append(out, fmt.Sprintf("items[%d].id is not a valid id", i))
		case seen[it.ID]:
			out = append(out, fmt.Sprintf("items[%d].id %q repeats", i, it.ID))
		}
		seen[it.ID] = true
		if strings.TrimSpace(it.Prompt) == "" {
			out = append(out, fmt.Sprintf("item %s has no prompt", it.ID))
		}
		if s.Grader.Kind == "regex" {
			if _, err := regexp.Compile(it.Expect); err != nil {
				out = append(out, fmt.Sprintf("item %s: expect is not a regular expression: %v", it.ID, err))
			}
		}
		if (s.Grader.Kind == "exact" || s.Grader.Kind == "regex") && it.Expect == "" {
			out = append(out, fmt.Sprintf("item %s needs expect for a %s grader", it.ID, s.Grader.Kind))
		}
	}
	return out
}

// Grade is one graded answer.
type Grade struct {
	Item   string `json:"item"`
	Passed bool   `json:"passed"`
	Reason string `json:"reason,omitempty"`
	// AnswerSHA256 binds the grade to the exact answer text.
	AnswerSHA256 string `json:"answer_sha256"`
}

// GradeAnswer grades answer against item with the suite's grader.
func (s Suite) GradeAnswer(ctx context.Context, it Item, answer string) (Grade, error) {
	sum := sha256.Sum256([]byte(answer))
	g := Grade{Item: it.ID, AnswerSHA256: hex.EncodeToString(sum[:])}
	switch s.Grader.Kind {
	case "exact":
		g.Passed = strings.TrimSpace(answer) == strings.TrimSpace(it.Expect)
		if !g.Passed {
			g.Reason = "answer differs from expect"
		}
		return g, nil
	case "regex":
		g.Passed = regexp.MustCompile(it.Expect).MatchString(answer)
		if !g.Passed {
			g.Reason = "answer does not match expect"
		}
		return g, nil
	}
	return s.command(ctx, it, answer, g)
}

func (s Suite) command(ctx context.Context, it Item, answer string, g Grade) (Grade, error) {
	timeout := time.Duration(s.Grader.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	in, _ := json.Marshal(map[string]string{"suite": s.ID, "item": it.ID, "prompt": it.Prompt, "expect": it.Expect, "answer": answer})
	cmd := exec.CommandContext(ctx, s.Grader.Argv[0], s.Grader.Argv[1:]...)
	cmd.Dir = filepath.Dir(s.Path)
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return g, fmt.Errorf("grader %s: %w", s.Grader.Argv[0], err) // could not run: not a verdict
		}
		if ctx.Err() != nil {
			return g, fmt.Errorf("grader timed out after %s on item %s", timeout, it.ID)
		}
	}
	var verdict struct {
		Passed *bool  `json:"passed"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &verdict); err != nil || verdict.Passed == nil {
		return g, fmt.Errorf("grader printed no {\"passed\": bool} for item %s (stderr: %s)", it.ID, strings.TrimSpace(errb.String()))
	}
	g.Passed, g.Reason = *verdict.Passed, verdict.Reason
	return g, nil
}
