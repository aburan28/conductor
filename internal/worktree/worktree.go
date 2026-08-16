// Package worktree manages the isolated git workspaces that autonomous attempts run in
// (DESIGN.md §7.10, §11).
//
// Worktree isolation solves file-level corruption: two agents editing the same repository
// cannot clobber each other's working tree. It does not solve logical conflicts — that is
// what reservations and the merge-risk graph are for. Both layers are needed, and neither
// substitutes for the other.
package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Manager creates and reclaims worktrees under a root directory.
type Manager struct {
	RepoPath string
	Root     string
	// GitTimeout bounds every git invocation. A hung git process must not stall the
	// scheduler loop that called it.
	GitTimeout time.Duration
}

func New(repoPath, root string) *Manager {
	if root == "" {
		root = filepath.Join(repoPath, ".conductor", "runtime", "worktrees")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(repoPath, root)
	}
	return &Manager{RepoPath: repoPath, Root: root, GitTimeout: 60 * time.Second}
}

// Workspace is one prepared worktree.
type Workspace struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
}

// branchSafe strips characters git refuses in a ref name.
var branchSafe = regexp.MustCompile(`[^a-zA-Z0-9._/-]+`)

// BranchName builds the conventional branch for an attempt: agent/T-42/attempt-7.
func BranchName(taskRef string, attemptNumber int) string {
	ref := branchSafe.ReplaceAllString(taskRef, "-")
	ref = strings.Trim(ref, "-/.")
	if ref == "" {
		ref = "task"
	}
	return fmt.Sprintf("agent/%s/attempt-%d", ref, attemptNumber)
}

// Prepare creates (or reuses) an isolated worktree for an attempt.
//
// Reuse matters for recovery: after a crash the branch and its work are still on disk, and a
// replacement attempt can adopt them rather than starting from nothing (DESIGN.md §27.2).
func (m *Manager) Prepare(ctx context.Context, taskRef string, attemptNumber int, baseBranch string) (Workspace, error) {
	if m.RepoPath == "" {
		return Workspace{}, fmt.Errorf("worktree: no repository path configured")
	}
	if baseBranch == "" {
		baseBranch = "main"
	}

	branch := BranchName(taskRef, attemptNumber)
	dir := filepath.Join(m.Root, fmt.Sprintf("%s-a%d", sanitizeDir(taskRef), attemptNumber))

	if err := os.MkdirAll(m.Root, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("worktree root: %w", err)
	}

	if info, err := os.Stat(filepath.Join(dir, ".git")); err == nil && !info.IsDir() || err == nil {
		// Already present — adopt it.
		sha, err := m.git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return Workspace{}, err
		}
		return Workspace{Path: dir, Branch: branch, BaseSHA: strings.TrimSpace(sha)}, nil
	}

	baseSHA, err := m.git(ctx, m.RepoPath, "rev-parse", baseBranch)
	if err != nil {
		// A repository with no commits yet, or an unknown base branch. Fall back to HEAD so
		// a fresh project still works.
		baseSHA, err = m.git(ctx, m.RepoPath, "rev-parse", "HEAD")
		if err != nil {
			return Workspace{}, fmt.Errorf("resolve base branch %q: %w", baseBranch, err)
		}
	}
	baseSHA = strings.TrimSpace(baseSHA)

	args := []string{"worktree", "add", "-b", branch, dir, baseSHA}
	if _, err := m.git(ctx, m.RepoPath, args...); err != nil {
		// The branch may already exist from a previous attempt; attach to it instead.
		if _, retryErr := m.git(ctx, m.RepoPath, "worktree", "add", dir, branch); retryErr != nil {
			return Workspace{}, fmt.Errorf("create worktree: %w", err)
		}
	}
	return Workspace{Path: dir, Branch: branch, BaseSHA: baseSHA}, nil
}

// Remove reclaims a worktree.
//
// Callers pass keep=true after a failure. A crashed run's tree is evidence — deleting it
// destroys the only record of what the agent actually did (DESIGN.md §10.4, §27.2).
func (m *Manager) Remove(ctx context.Context, path string, keep bool) error {
	if keep || path == "" {
		return nil
	}
	if _, err := m.git(ctx, m.RepoPath, "worktree", "remove", "--force", path); err != nil {
		// Fall back to a plain delete plus prune, so a half-registered worktree does not
		// wedge the runner permanently.
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
		_, _ = m.git(ctx, m.RepoPath, "worktree", "prune")
	}
	return nil
}

// Status reports the paths an attempt has changed, relative to the repository root.
//
// This is the input to scope-drift detection and the merge-risk graph. It reads git, not the
// agent's account of itself, which is the difference between evidence and a claim.
func (m *Manager) Status(ctx context.Context, path, baseSHA string) (changed []string, head string, err error) {
	if path == "" {
		return nil, "", nil
	}
	if out, err := m.git(ctx, path, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(out)
	}

	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			changed = append(changed, p)
		}
	}

	// Committed changes since the base.
	if baseSHA != "" {
		if out, err := m.git(ctx, path, "diff", "--name-only", baseSHA+"...HEAD"); err == nil {
			for _, line := range strings.Split(out, "\n") {
				add(line)
			}
		}
	}
	// Uncommitted changes, including untracked files, so drift is caught before a commit
	// rather than after.
	if out, err := m.git(ctx, path, "status", "--porcelain"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if len(line) < 4 {
				continue
			}
			entry := strings.TrimSpace(line[2:])
			// Renames appear as "old -> new"; both sides are touched.
			if old, new_, found := strings.Cut(entry, " -> "); found {
				add(old)
				add(new_)
				continue
			}
			add(entry)
		}
	}

	sort.Strings(changed)
	return changed, head, nil
}

// Commit stages everything and commits, returning the new SHA. Returns an empty SHA when
// there was nothing to commit.
func (m *Manager) Commit(ctx context.Context, path, message string) (string, error) {
	if _, err := m.git(ctx, path, "add", "-A"); err != nil {
		return "", err
	}
	if out, err := m.git(ctx, path, "status", "--porcelain"); err == nil && strings.TrimSpace(out) == "" {
		return "", nil
	}
	if _, err := m.git(ctx, path, "commit", "-m", message); err != nil {
		return "", err
	}
	sha, err := m.git(ctx, path, "rev-parse", "HEAD")
	return strings.TrimSpace(sha), err
}

// Drift reports how far the base branch has moved since an attempt started, which gates
// automatic rebase and integration review (DESIGN.md §27.5).
func (m *Manager) Drift(ctx context.Context, baseBranch, baseSHA string) (commits int, changed []string, err error) {
	if baseSHA == "" {
		return 0, nil, nil
	}
	out, err := m.git(ctx, m.RepoPath, "rev-list", "--count", baseSHA+".."+baseBranch)
	if err != nil {
		return 0, nil, err
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &commits); err != nil {
		return 0, nil, nil
	}
	if commits == 0 {
		return 0, nil, nil
	}
	names, err := m.git(ctx, m.RepoPath, "diff", "--name-only", baseSHA, baseBranch)
	if err != nil {
		return commits, nil, nil
	}
	for _, line := range strings.Split(names, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			changed = append(changed, s)
		}
	}
	return commits, changed, nil
}

// HeadSHA returns the repository's current HEAD.
func (m *Manager) HeadSHA(ctx context.Context) (string, error) {
	out, err := m.git(ctx, m.RepoPath, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// IsRepo reports whether the configured path is a git working tree.
func (m *Manager) IsRepo(ctx context.Context) bool {
	out, err := m.git(ctx, m.RepoPath, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	timeout := m.GitTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A deterministic, minimal environment: no user hooks, no interactive prompts, no
	// credential helpers reaching for a keychain in the middle of a scheduler tick.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func sanitizeDir(s string) string {
	s = branchSafe.ReplaceAllString(s, "-")
	s = strings.ReplaceAll(s, "/", "-")
	return strings.Trim(s, "-.")
}
