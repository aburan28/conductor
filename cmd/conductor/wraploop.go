package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/adamburan/conductor/internal/client"
)

// The claim loop's half that lives in the wrap sidecar: adopting a claim made before the
// session started, and telling the control plane which paths the working tree has touched.

// worktreeRoot is the repository root the CLI is running in, or the working directory when
// it is not in a repository. A claim records it so the session that later works the claim —
// started in the same checkout — can find and adopt it.
func worktreeRoot(ctx context.Context) string {
	if root, err := gitOutput(ctx, "rev-parse", "--show-toplevel"); err == nil && root != "" {
		return root
	}
	cwd, _ := os.Getwd()
	return cwd
}

// adoptClaims binds claims made in this checkout before the session started (`conductor task
// claim`, then `conductor wrap`) to the new session, whose heartbeat keeps them alive from
// here on. A failure is reported and ignored: the session is still useful without it.
func adoptClaims(ctx context.Context, api *client.Client, sessionID, worktree string) {
	var out struct {
		Adopted []struct {
			TaskRef string `json:"task_ref"`
		} `json:"adopted"`
	}
	if err := api.Post(ctx, "/v1/sessions/"+sessionID+"/adopt",
		map[string]any{"worktree_path": worktree}, &out); err != nil {
		fmt.Fprintf(os.Stderr, "conductor: could not attach earlier claims to this session: %v\n", err)
		return
	}
	for _, a := range out.Adopted {
		fmt.Fprintf(os.Stderr, "Conductor: this session now holds %s; its claim stays alive while the session runs.\n", a.TaskRef)
	}
}

// maxHeartbeatPaths mirrors the server's cap; there is no point sending more.
const maxHeartbeatPaths = 500

// observedPaths lists the paths the working tree at root differs in from base: tracked
// changes (committed since base, staged, or not) plus untracked files that are not ignored,
// all relative to the repository root. Paths only — no content, no diff — which is all the
// merge-risk graph needs. It returns nil when git cannot answer, so the heartbeat leaves the
// recorded set alone rather than wiping it.
func observedPaths(ctx context.Context, root, base string) []string {
	diffArgs := []string{"diff", "--name-only", "--no-renames"}
	if base != "" {
		diffArgs = append(diffArgs, base)
	} else {
		diffArgs = append(diffArgs, "HEAD")
	}
	tracked, err := gitLines(ctx, root, diffArgs...)
	if err != nil {
		return nil
	}
	// Run at the root: ls-files only lists what is under its working directory, and the
	// wrapped tool may have been started in a subdirectory.
	untracked, err := gitLines(ctx, root, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range append(tracked, untracked...) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > maxHeartbeatPaths {
		out = out[:maxHeartbeatPaths]
	}
	return out
}

func gitLines(ctx context.Context, root string, args ...string) ([]string, error) {
	// -c core.quotepath=off keeps non-ASCII names readable instead of octal-escaped.
	prefix := []string{"-c", "core.quotepath=off"}
	if root != "" {
		prefix = append(prefix, "-C", root)
	}
	cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	raw, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}
