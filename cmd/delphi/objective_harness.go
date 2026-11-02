package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/capability"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/harness"
	"github.com/aburan28/conductor/internal/objective"
	"github.com/aburan28/conductor/internal/worktree"
)

// Coding-agent delegates (docs/DELPHI_OBJECTIVES.md, phase 5): a work item routed to
// claude-code:<model> or codex:<model> is done by that agent in a worktree of the objective's
// repository, on its own branch, and the attempt's answer is the diff it committed. The
// branch stays for a person to review or merge; nothing is merged or pushed.
//
// The agent runs as its CLI normally does (claude -p, codex exec) with the operator's own
// credentials, in the worktree; DELPHI_CLAUDE_CODE_PERMISSION (default acceptEdits) is passed
// to Claude Code. The run happens in the background; its result is written atomically to
// harness/<request>.json, so a restarted runner finds a finished run and reports one it lost.

// harnessTargets are the coding-agent providers a route may name.
var harnessTargets = map[string]struct{ driver, envCommand, command string }{
	"claude-code": {"claude", "DELPHI_CLAUDE_CODE_COMMAND", "claude"},
	"codex":       {"codex", "DELPHI_CODEX_COMMAND", "codex"},
}

func isHarness(provider string) bool {
	_, ok := harnessTargets[provider]
	return ok
}

func harnessDriver(provider string) harness.Driver {
	h := harnessTargets[provider]
	cmd := os.Getenv(h.envCommand)
	if cmd == "" {
		cmd = h.command
	}
	cfg := harness.HarnessConfig{Enabled: true, Command: cmd}
	if h.driver == "codex" {
		return harness.NewCodexDriver(cfg)
	}
	return harness.NewClaudeDriver(cfg)
}

// harnessResult is a finished coding-agent run, as written to harness/<request>.json.
type harnessResult struct {
	Succeeded    bool     `json:"succeeded"`
	ExitCode     int      `json:"exit_code"`
	FailureClass string   `json:"failure_class,omitempty"`
	Error        string   `json:"error,omitempty"`
	Turns        int      `json:"turns"`
	TokensIn     int64    `json:"tokens_in"`
	TokensOut    int64    `json:"tokens_out"`
	CostUSD      float64  `json:"cost_usd"`
	Worktree     string   `json:"worktree"`
	Branch       string   `json:"branch"`
	BaseSHA      string   `json:"base_sha"`
	Commit       string   `json:"commit,omitempty"`
	Changed      []string `json:"changed,omitempty"`
	Diff         string   `json:"diff,omitempty"`
	DiffTrimmed  bool     `json:"diff_trimmed,omitempty"`
}

const maxDiff = 256 << 10

var runningHarness sync.Map // request id -> struct{}, for runs this process started

func (r *objRunner) harnessPath(requestID string) string {
	return filepath.Join(r.dir, "harness", requestID+".json")
}

// startHarness runs a coding agent for n's attempt in the background.
func (r *objRunner) startHarness(n *objective.Node, att objective.Attempt, instruction string) {
	cfg := r.log.Tree().Config
	tg, _ := capability.ParseTarget(att.Target)
	runningHarness.Store(att.RequestID, struct{}{})
	go func() {
		defer runningHarness.Delete(att.RequestID)
		timeout := time.Duration(cfg.HarnessTimeoutSecs) * time.Second
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout+time.Minute)
		defer cancel()
		res := runHarness(ctx, cfg, r.dir, att.RequestID, n.ID, tg, instruction, timeout)
		if err := writeJSONAtomic(r.harnessPath(att.RequestID), res); err != nil {
			r.a.logf("harness %s: result not written: %v", att.RequestID, err)
		}
	}()
}

func runHarness(ctx context.Context, cfg objective.Config, dir, requestID, node string, tg capability.Target, instruction string, timeout time.Duration) harnessResult {
	var out harnessResult
	trees := worktree.New(cfg.Repo, filepath.Join(dir, "worktrees"))
	attempt := 1
	if i := strings.LastIndex(requestID, "-a"); i >= 0 {
		fmt.Sscanf(requestID[i+2:], "%d", &attempt)
		attempt++
	}
	ws, err := trees.Prepare(ctx, "objective-"+objective.Key(node), attempt, cfg.BaseRef)
	if err != nil {
		out.FailureClass, out.Error = "worktree", err.Error()
		return out
	}
	out.Worktree, out.Branch, out.BaseSHA = ws.Path, ws.Branch, ws.BaseSHA
	spec := harness.RunSpec{TaskRef: node, Instruction: instruction, Cwd: ws.Path, Branch: ws.Branch,
		BaseSHA: ws.BaseSHA, Model: tg.Model, Effort: domain.Effort(tg.Effort), Timeout: timeout,
		PermissionMode: envOr("DELPHI_CLAUDE_CODE_PERMISSION", "acceptEdits")}
	h, err := harnessDriver(tg.Provider).Start(ctx, spec)
	if err != nil {
		out.FailureClass, out.Error = "start", err.Error()
		return out
	}
	go func() {
		for range h.Events() { // usage arrives in the result; the stream only needs draining
		}
	}()
	res, err := h.Wait(ctx)
	out.Succeeded, out.ExitCode, out.FailureClass = res.Succeeded, res.ExitCode, res.FailureClass
	out.Turns, out.TokensIn, out.TokensOut, out.CostUSD = res.Turns, res.TokensIn, res.TokensOut, res.CostUSD
	if err != nil {
		out.Error = err.Error()
	}
	changed, _, err := trees.Status(ctx, ws.Path, ws.BaseSHA)
	if err != nil {
		out.Error = strings.TrimSpace(out.Error + "; status: " + err.Error())
		return out
	}
	out.Changed = changed
	if len(changed) > 0 {
		// The agent may have committed some or all of its work itself; commit the rest.
		if commit, err := trees.Commit(ctx, ws.Path, fmt.Sprintf("delphi objective %s: %s", node, tg)); err == nil && commit != "" {
			out.Commit = commit
		}
		if out.Commit == "" {
			if head, err := git(ctx, ws.Path, "rev-parse", "HEAD"); err == nil {
				out.Commit = head
			}
		}
		diff, _ := git(ctx, ws.Path, "diff", ws.BaseSHA+".."+out.Commit)
		if len(diff) > maxDiff {
			diff, out.DiffTrimmed = diff[:maxDiff], true
		}
		out.Diff = diff
	}
	return out
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(b)), err
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func writeJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// collectHarness folds a finished coding-agent run into the node.
func (r *objRunner) collectHarness(n *objective.Node, a *objective.Attempt) (bool, error) {
	b, err := os.ReadFile(r.harnessPath(a.RequestID))
	if errors.Is(err, os.ErrNotExist) {
		if _, running := runningHarness.Load(a.RequestID); running {
			if objective.TimeLeft(n, r.now()) <= 0 {
				return true, r.log.Append(n.ID, objective.EvFailed, objective.ReasonData{Reason: fmt.Sprintf("deadline %s passed while %s was working", n.DeadlineAt, a.Target)})
			}
			return false, nil
		}
		// Started by a runner that has since stopped: the run is gone with it.
		return true, r.log.Append(n.ID, objective.EvAnswered, objective.AnsweredData{RequestID: a.RequestID,
			Status: "errored", Outcome: "interrupted (the runner stopped during the coding agent's run)"})
	}
	if err != nil {
		return false, err
	}
	var res harnessResult
	if err := json.Unmarshal(b, &res); err != nil {
		return false, fmt.Errorf("%s: %w", r.harnessPath(a.RequestID), err)
	}
	d := objective.AnsweredData{RequestID: a.RequestID, Status: "succeeded", Tokens: res.TokensIn + res.TokensOut,
		CostUSD: res.CostUSD, Priced: res.CostUSD > 0, Worktree: res.Worktree, Branch: res.Branch, Commit: res.Commit, Changed: res.Changed}
	switch {
	case !res.Succeeded:
		d.Status, d.Outcome = "errored", fmt.Sprintf("the coding agent failed (%s, exit %d)", orNone(res.FailureClass), res.ExitCode)
	case len(res.Changed) == 0:
		d.Status, d.Outcome = "errored", "the coding agent changed nothing"
	default:
		d.Answer = harnessAnswer(res)
		h := sha256.Sum256([]byte(d.Answer))
		d.AnswerSHA = hex.EncodeToString(h[:])
	}
	return true, r.log.Append(n.ID, objective.EvAnswered, d)
}

// harnessAnswer is what checks read: where the work is, and the diff itself.
func harnessAnswer(res harnessResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Branch %s, commit %s (base %s).\nChanged files:\n- %s\n\n", res.Branch, res.Commit, res.BaseSHA, strings.Join(res.Changed, "\n- "))
	if res.DiffTrimmed {
		fmt.Fprintf(&b, "The diff is longer than %d KiB; the first part follows. Read the whole of it on the branch.\n\n", maxDiff>>10)
	}
	b.WriteString("```diff\n")
	b.WriteString(res.Diff)
	b.WriteString("\n```\n")
	return b.String()
}

// harnessPrompt is the coding agent's instruction for a work item.
func (r *objRunner) harnessPrompt(n *objective.Node) string {
	return r.prompt(n, "work") + "\n\nYou are working in a git worktree of the repository, on your own branch. Make the change there and commit it. Do not push, merge or open a pull request; an independent reviewer and the operator's checks will judge the diff against the criteria."
}
