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
// The repository's own metadata directory is off limits too: a file written under .git is
// code execution the next time anyone runs git there.
func writeInside(dir, rel string, data []byte, perm, dirPerm os.FileMode) error {
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if !SafeRelPath(rel) {
		return fmt.Errorf("refusing to write %q: it leaves %s", rel, dir)
	}
	first, _, _ := strings.Cut(rel, "/")
	if strings.EqualFold(first, ".git") {
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
