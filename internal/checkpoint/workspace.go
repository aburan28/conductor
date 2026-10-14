package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The working tree travels with the conversation, because a transcript that says "I edited
// router.go" is worthless next to a router.go that was never edited. Three layers cover
// every state a tree can be in:
//
//   - commits not on any remote ref go in a git bundle, so unpushed work is not lost and a
//     resume on another machine fetches it with ordinary git;
//   - tracked changes (staged or not) go in one binary patch against HEAD;
//   - untracked, non-ignored files go in verbatim.
//
// Ignored files never travel: that is where node_modules and build output live, and also
// where .env files live.

// Limits that keep a checkpoint a checkpoint rather than a backup of the disk.
const (
	maxUntrackedFileBytes = 8 << 20  // one untracked file larger than this is skipped, and named
	maxUntrackedTotal     = 64 << 20 // across all untracked files
	maxPatchBytes         = 64 << 20
)

// Git runs git in a directory. A variable so tests can observe or replace it.
var Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

func gitLine(ctx context.Context, dir string, args ...string) string {
	out, err := Git(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// InspectRepo describes the repository cwd is in. A cwd outside any repository yields a
// Repo with an empty Root and no error: a checkpoint of a conversation that was not about
// a repository is still worth taking.
func InspectRepo(ctx context.Context, cwd string) (Repo, error) {
	root := gitLine(ctx, cwd, "rev-parse", "--show-toplevel")
	if root == "" {
		return Repo{}, nil
	}
	r := Repo{Root: root}
	if rel, err := filepath.Rel(root, cwd); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		r.Subdir = filepath.ToSlash(rel)
	}
	r.Head = gitLine(ctx, root, "rev-parse", "HEAD")
	if b := gitLine(ctx, root, "symbolic-ref", "--short", "-q", "HEAD"); b != "" {
		r.Branch = b
	}
	if remote := gitLine(ctx, root, "remote"); remote != "" {
		first := strings.Fields(remote)
		name := first[0]
		for _, n := range first {
			if n == "origin" {
				name = n
			}
		}
		r.Remote = gitLine(ctx, root, "remote", "get-url", name)
	}
	if out, err := Git(ctx, root, "status", "--porcelain", "--untracked-files=all"); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if len(line) < 3 {
				continue
			}
			if strings.HasPrefix(line, "??") {
				r.Untracked++
			} else {
				r.Modified++
			}
		}
		r.Dirty = r.Untracked+r.Modified > 0
	}
	if r.Head != "" {
		if out, err := Git(ctx, root, "rev-list", "--count", "HEAD", "--not", "--remotes"); err == nil {
			r.Ahead, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		}
	}
	return r, nil
}

// CaptureWorkspace stages the working-tree layers into the bundle and fills the manifest's
// Workspace section. It never fails the checkpoint over the tree: a layer that cannot be
// captured is recorded as skipped, because the transcript is the part that cannot be
// recreated.
func CaptureWorkspace(ctx context.Context, repo Repo, b *Builder) Workspace {
	var ws Workspace
	if repo.Root == "" {
		ws.Skipped = "not a git repository"
		return ws
	}
	root := repo.Root
	// Commits not on any remote.
	if repo.Ahead > 0 {
		tmp, err := os.CreateTemp("", "conductor-commits-*.bundle")
		if err == nil {
			name := tmp.Name()
			tmp.Close()
			os.Remove(name)
			if _, err := Git(ctx, root, "bundle", "create", name, "HEAD", "--not", "--remotes"); err == nil {
				if data, err := os.ReadFile(name); err == nil {
					if b.Add(CommitsBundle, data) == nil {
						ws.CommitsBundle = true
					}
				}
			}
			os.Remove(name)
		}
	}
	// Tracked changes, staged and unstaged, as one patch against HEAD.
	if repo.Head != "" && repo.Modified > 0 {
		if patch, err := Git(ctx, root, "diff", "--binary", "--no-color", "--no-ext-diff", "HEAD", "--"); err == nil && len(patch) > 0 {
			if len(patch) > maxPatchBytes {
				ws.Skipped = joinSkipped(ws.Skipped, fmt.Sprintf("tracked changes exceed %d MiB", maxPatchBytes>>20))
			} else if b.Add(TrackedPatchPath, patch) == nil {
				ws.PatchBytes = int64(len(patch))
			}
		}
	}
	// Untracked, non-ignored files.
	if repo.Untracked > 0 {
		out, err := Git(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
		if err == nil {
			var total int64
			for _, rel := range strings.Split(string(out), "\x00") {
				if rel == "" {
					continue
				}
				p := filepath.Join(root, filepath.FromSlash(rel))
				info, err := os.Lstat(p)
				if err != nil || !info.Mode().IsRegular() {
					continue
				}
				if info.Size() > maxUntrackedFileBytes {
					ws.Skipped = joinSkipped(ws.Skipped, "untracked "+rel+" is too large")
					continue
				}
				if total+info.Size() > maxUntrackedTotal {
					ws.Skipped = joinSkipped(ws.Skipped, "untracked files exceed the size limit from "+rel)
					break
				}
				data, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				if b.Add(UntrackedDir+"/"+rel, data) == nil {
					ws.UntrackedFiles = append(ws.UntrackedFiles, rel)
					total += info.Size()
				}
			}
		}
	}
	sort.Strings(ws.UntrackedFiles)
	return ws
}

func joinSkipped(have, add string) string {
	if have == "" {
		return add
	}
	return have + "; " + add
}

// RestoreOptions controls how a workspace is put back.
type RestoreOptions struct {
	// Dir is the checkout to restore into. It must exist and be a git repository unless
	// Clone is set, in which case it is created by cloning the checkpoint's remote.
	Dir   string
	Clone bool
	// Force restores into a dirty tree. Without it a dirty tree is refused, because a
	// patch applied over unrelated edits is how work gets lost.
	Force bool
	// SkipCheckout leaves HEAD alone and applies only the patch and untracked files.
	SkipCheckout bool
}

// RestoreReport says what happened.
type RestoreReport struct {
	Dir           string   `json:"dir"`
	Cloned        bool     `json:"cloned,omitempty"`
	FetchedBundle bool     `json:"fetched_bundle,omitempty"`
	CheckedOut    string   `json:"checked_out,omitempty"`
	PatchApplied  bool     `json:"patch_applied,omitempty"`
	Untracked     int      `json:"untracked_written,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// RestoreWorkspace puts a bundle's working tree back into a checkout. The order matters:
// commits first (so the base is reachable), then the checkout, then the patch, then the
// loose files.
func RestoreWorkspace(ctx context.Context, b *Bundle, opts RestoreOptions) (RestoreReport, error) {
	rep := RestoreReport{Dir: opts.Dir}
	repo := b.Manifest.Repo
	if repo.Root == "" {
		rep.Warnings = append(rep.Warnings, "the checkpoint carries no repository state")
		return rep, nil
	}
	if _, err := os.Stat(opts.Dir); err != nil {
		if !opts.Clone {
			return rep, fmt.Errorf("%s does not exist (pass --clone to create it from %s)", opts.Dir, orNone(repo.Remote))
		}
		if repo.Remote == "" {
			return rep, errors.New("the checkpoint records no remote to clone from")
		}
		if err := os.MkdirAll(filepath.Dir(opts.Dir), 0o755); err != nil {
			return rep, err
		}
		if _, err := Git(ctx, filepath.Dir(opts.Dir), "clone", "--quiet", repo.Remote, opts.Dir); err != nil {
			return rep, err
		}
		rep.Cloned = true
	}
	root := gitLine(ctx, opts.Dir, "rev-parse", "--show-toplevel")
	if root == "" {
		return rep, fmt.Errorf("%s is not a git repository", opts.Dir)
	}
	if !opts.Force {
		if st := gitLine(ctx, root, "status", "--porcelain"); st != "" {
			return rep, fmt.Errorf("%s has uncommitted changes; commit or stash them, or pass --force", root)
		}
	}
	if data, ok := b.File(CommitsBundle); ok {
		tmp, err := os.CreateTemp("", "conductor-commits-*.bundle")
		if err != nil {
			return rep, err
		}
		name := tmp.Name()
		_, werr := tmp.Write(data)
		tmp.Close()
		defer os.Remove(name)
		if werr != nil {
			return rep, werr
		}
		if _, err := Git(ctx, root, "fetch", "--quiet", name, "HEAD"); err != nil {
			return rep, fmt.Errorf("fetching the checkpoint's commits: %w", err)
		}
		rep.FetchedBundle = true
	}
	if !opts.SkipCheckout && repo.Head != "" {
		if _, err := Git(ctx, root, "cat-file", "-e", repo.Head+"^{commit}"); err != nil {
			// The commit may only exist on the remote: one fetch, then give up gracefully.
			_, _ = Git(ctx, root, "fetch", "--quiet", "--all")
			if _, err := Git(ctx, root, "cat-file", "-e", repo.Head+"^{commit}"); err != nil {
				rep.Warnings = append(rep.Warnings, "commit "+short(repo.Head)+" is not reachable here; HEAD was left as is and the patch may not apply cleanly")
				goto patch
			}
		}
		if repo.Branch != "" {
			// Put the branch at the recorded commit. A branch that already exists elsewhere
			// is moved only if the recorded commit is ahead of it; otherwise it is left and a
			// detached checkout is used, so a resume never rewinds someone's branch.
			cur := gitLine(ctx, root, "rev-parse", "--verify", "-q", "refs/heads/"+repo.Branch)
			switch {
			case cur == "":
				if _, err := Git(ctx, root, "checkout", "--quiet", "-b", repo.Branch, repo.Head); err != nil {
					return rep, err
				}
				rep.CheckedOut = repo.Branch + " @ " + short(repo.Head)
			case cur == repo.Head || isAncestor(ctx, root, cur, repo.Head):
				if _, err := Git(ctx, root, "checkout", "--quiet", repo.Branch); err != nil {
					return rep, err
				}
				if cur != repo.Head {
					if _, err := Git(ctx, root, "merge", "--quiet", "--ff-only", repo.Head); err != nil {
						return rep, err
					}
				}
				rep.CheckedOut = repo.Branch + " @ " + short(repo.Head)
			default:
				if _, err := Git(ctx, root, "checkout", "--quiet", "--detach", repo.Head); err != nil {
					return rep, err
				}
				rep.CheckedOut = "detached @ " + short(repo.Head)
				rep.Warnings = append(rep.Warnings, "branch "+repo.Branch+" already exists here and has diverged; checked out the commit detached instead")
			}
		} else {
			if _, err := Git(ctx, root, "checkout", "--quiet", "--detach", repo.Head); err != nil {
				return rep, err
			}
			rep.CheckedOut = "detached @ " + short(repo.Head)
		}
	}
patch:
	if data, ok := b.File(TrackedPatchPath); ok {
		cmd := exec.CommandContext(ctx, "git", "apply", "--3way", "--whitespace=nowarn", "-")
		cmd.Dir = root
		cmd.Stdin = bytes.NewReader(data)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return rep, fmt.Errorf("applying the checkpoint's tracked changes: %s", strings.TrimSpace(stderr.String()))
		}
		rep.PatchApplied = true
	}
	for _, p := range b.Files(UntrackedDir + "/") {
		rel := strings.TrimPrefix(p, UntrackedDir+"/")
		if rel == "" {
			continue
		}
		data, _ := b.File(p)
		// Never through a symlink, never outside the checkout, never into .git: the patch
		// applied above may have just created a link for exactly this (writeInside).
		if err := writeInside(root, rel, data, 0o644, 0o755); err != nil {
			return rep, fmt.Errorf("restoring untracked files: %w", err)
		}
		rep.Untracked++
	}
	return rep, nil
}

func isAncestor(ctx context.Context, root, a, b string) bool {
	_, err := Git(ctx, root, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orNone(s string) string {
	if s == "" {
		return "(no remote recorded)"
	}
	return s
}
