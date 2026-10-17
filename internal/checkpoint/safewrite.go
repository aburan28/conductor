package checkpoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// writeInside writes data to rel beneath dir, and refuses anything that would land outside it.
//
// A bundle's member paths are attacker-supplied as far as a restore is concerned: a
// checkpoint is a file someone sent, or an object in a bucket someone else can write. Two
// checks together keep a restore inside its directory:
//
//   - The write goes through os.Root, which resolves every component within dir and fails
//     on any path — "..", absolute, or a symbolic link — that resolves outside it.
//   - No component may be a symbolic link at all, even one pointing inside dir. The
//     tracked-changes patch is applied first and may itself create a link (`evil -> .git`,
//     or `evil -> ~/.ssh`); a later untracked file `evil/hooks/post-checkout` would then be
//     written through it. Refusing links outright is simpler than reasoning about targets.
//
// The repository's own metadata directory is off limits too, at any depth: a file written
// under .git is code execution the next time anyone runs git there, and a nested
// `sub/.git/config` (core.fsmonitor) works as well as a top-level one (IsGitDirName).
func writeInside(dir, rel string, data []byte, perm, dirPerm os.FileMode) error {
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if !SafeRelPath(rel) {
		return fmt.Errorf("refusing to write %q: it leaves %s", rel, dir)
	}
	if hasGitDirComponent(rel) {
		return fmt.Errorf("refusing to write %q: a checkpoint never writes into .git", rel)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		info, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			break // nothing further down exists yet, so nothing further down is a link
		}
		if err != nil {
			return fmt.Errorf("refusing to write %q: %w", rel, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write %q: %s is a symbolic link", rel, prefix)
		}
		if i < len(parts) && !info.IsDir() {
			return fmt.Errorf("refusing to write %q: %s is not a directory", rel, prefix)
		}
	}
	if parent := path.Dir(rel); parent != "." {
		if err := root.MkdirAll(parent, dirPerm); err != nil {
			return err
		}
	}
	return root.WriteFile(rel, data, perm)
}

// hasGitDirComponent reports whether any component of the slash-separated path p names a
// git metadata directory.
func hasGitDirComponent(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if IsGitDirName(part) {
			return true
		}
	}
	return false
}

// IsGitDirName reports whether a single path component would be taken for ".git" by some
// filesystem a checkout can live on, following git's own verify_path: case-insensitively,
// with NTFS's trailing dots and spaces and alternate data streams (".git. ", ".git::$DATA"),
// its 8.3 short name ("git~1"), and the code points HFS+ ignores (".g\u200cit").
func IsGitDirName(name string) bool {
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i] // NTFS: ".git::$INDEX_ALLOCATION" is the directory itself
	}
	name = strings.TrimRight(name, ". ")
	name = strings.Map(func(r rune) rune {
		if hfsIgnorable(r) {
			return -1
		}
		return r
	}, name)
	return strings.EqualFold(name, ".git") || strings.EqualFold(name, "git~1")
}

// hfsIgnorable lists the code points HFS+ drops when comparing names (git's next_hfs_char).
func hfsIgnorable(r rune) bool {
	switch {
	case r == 0x200c, r == 0x200d, r == 0x200e, r == 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x206a && r <= 0x206f,
		r == 0xfeff:
		return true
	}
	return false
}
