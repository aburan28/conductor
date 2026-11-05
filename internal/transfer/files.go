package transfer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"
)

// Writes into a harness home are driven by paths that came out of an untrusted archive, so
// they go through os.Root: every component is resolved inside the home, ".." and absolute
// paths are refused, and a symbolic link at any component is refused outright (a link
// planted by an earlier session could otherwise redirect the next write).

// writeInside writes data to rel beneath dir, creating parent directories as needed.
func writeInside(dir, rel string, data []byte, perm, dirPerm fs.FileMode) error {
	rel, err := checkRel(dir, rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if parent := path.Dir(rel); parent != "." {
		if err := root.MkdirAll(parent, dirPerm); err != nil {
			return err
		}
	}
	return root.WriteFile(rel, data, perm)
}

// appendInside appends suffix to an existing file, refusing links and non-regular files.
func appendInside(dir, rel string, suffix []byte) error {
	rel, err := checkRel(dir, rel)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := noSymlinks(root, rel); err != nil {
		return err
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(suffix)
	return err
}

// readInside reads rel beneath dir. It reports missing files as (nil, false, nil) so the
// caller can treat a first import differently from a conflict.
func readInside(dir, rel string) ([]byte, bool, error) {
	rel, err := checkRel(dir, rel)
	if err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	if err := noSymlinks(root, rel); err != nil {
		return nil, false, err
	}
	f, err := root.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	data, err := readCapped(f, HardMaxSessionBytes, rel)
	return data, err == nil, err
}

func checkRel(dir, rel string) (string, error) {
	rel = path.Clean(strings.ReplaceAll(rel, "\\", "/"))
	if !SafeRel(rel) {
		return "", fmt.Errorf("refusing to write %q: it would leave %s", rel, dir)
	}
	return rel, nil
}

// noSymlinks refuses a path any of whose existing components is a symbolic link.
func noSymlinks(root *os.Root, rel string) error {
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		info, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // nothing further down exists yet, so nothing further down is a link
		}
		if err != nil {
			return fmt.Errorf("refusing %q: %w", rel, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing %q: %s is a symbolic link", rel, prefix)
		}
		if i < len(parts) && !info.IsDir() {
			return fmt.Errorf("refusing %q: %s is not a directory", rel, prefix)
		}
	}
	return nil
}

func runtimeIsCaseInsensitive() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}
