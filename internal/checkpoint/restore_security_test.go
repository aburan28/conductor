package checkpoint

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// craftedBundle seals a bundle whose tracked patch and untracked files are chosen by the test,
// the way an attacker would build one.
func craftedBundle(t *testing.T, patch []byte, untracked map[string]string) *Bundle {
	t.Helper()
	var b Builder
	if patch != nil {
		if err := b.Add(TrackedPatchPath, patch); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range untracked {
		if err := b.Add(UntrackedDir+"/"+rel, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{Schema: Schema, ID: "ck-evil", CreatedAt: time.Now().UTC(), Harness: "claude",
		SessionID: "s", Cwd: "/x", Repo: Repo{Root: "/x"}}
	var buf bytes.Buffer
	if err := b.Seal(&m, &buf); err != nil {
		t.Fatal(err)
	}
	bundle, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

// The tracked patch can create a symlink; the untracked files are then written after it. A
// patch that plants `evil -> <somewhere outside>` followed by an untracked `evil/payload` used
// to write the payload outside the checkout.
func TestRestoreRefusesToWriteThroughASymlinkThePatchCreated(t *testing.T) {
	_, work := newRepoPair(t)
	outside := t.TempDir()

	// Produce a genuine git patch that adds the symlink.
	scratch := filepath.Join(t.TempDir(), "scratch")
	gitT(t, filepath.Dir(scratch), "clone", "-q", filepath.Join(filepath.Dir(work), "origin.git"), scratch)
	if err := os.Symlink(outside, filepath.Join(scratch, "evil")); err != nil {
		t.Fatal(err)
	}
	gitT(t, scratch, "add", "-N", "evil")
	patch := gitT(t, scratch, "diff") + "\n"

	bundle := craftedBundle(t, []byte(patch), map[string]string{"evil/payload": "owned\n"})
	_, err := RestoreWorkspace(context.Background(), bundle, RestoreOptions{Dir: work, SkipCheckout: true})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("restore through a planted symlink = %v, want a refusal naming the link", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(err) {
		t.Fatalf("the payload was written outside the checkout (stat err %v)", err)
	}
}

// A file under .git is code execution the next time anyone runs git in that checkout.
func TestRestoreRefusesToWriteIntoGitMetadata(t *testing.T) {
	_, work := newRepoPair(t)
	for _, rel := range []string{".git/hooks/post-checkout", ".GIT/config",
		// A nested repository's config is as live as the top one (core.fsmonitor), and every
		// spelling a filesystem folds onto .git counts.
		"sub/.git/config", "sub/.Git./config", "sub/.git /config", "git~1/config",
		".git::$INDEX_ALLOCATION/config", "sub/.g\u200cit/config"} {
		bundle := craftedBundle(t, nil, map[string]string{rel: "#!/bin/sh\necho owned\n"})
		if _, err := RestoreWorkspace(context.Background(), bundle,
			RestoreOptions{Dir: work, SkipCheckout: true, Force: true}); err == nil {
			t.Errorf("restore wrote %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(work, ".git", "hooks", "post-checkout")); !os.IsNotExist(err) {
		t.Error("a hook was planted in .git")
	}
	if _, err := os.Stat(filepath.Join(work, "sub", ".git", "config")); !os.IsNotExist(err) {
		t.Error("a nested .git/config was planted")
	}
}

func TestIsGitDirName(t *testing.T) {
	for _, name := range []string{".git", ".GIT", ".git.", ".git . ", "GIT~1", ".git::$DATA", ".\u200dgi\ufefft"} {
		if !IsGitDirName(name) {
			t.Errorf("IsGitDirName(%q) = false", name)
		}
	}
	for _, name := range []string{"git", ".github", ".gitignore", "x.git", ".git~1", "git~2", "..."} {
		if IsGitDirName(name) {
			t.Errorf("IsGitDirName(%q) = true", name)
		}
	}
}

// Identifiers from a manifest become file names and command-line arguments.
func TestManifestRejectsUnsafeIdentifiers(t *testing.T) {
	ok := Manifest{Schema: Schema, ID: "ck-20260101T000000Z-abcdef", Harness: "codex",
		SessionID: "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a normal manifest was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"session traversal":    func(m *Manifest) { m.SessionID = "../../.ssh/authorized_keys" },
		"session separator":    func(m *Manifest) { m.SessionID = "a/b" },
		"session option":       func(m *Manifest) { m.SessionID = "--dangerously-bypass-approvals" },
		"id traversal":         func(m *Manifest) { m.ID = "../x" },
		"harness separator":    func(m *Manifest) { m.Harness = "claude/../../x" },
		"absolute native path": func(m *Manifest) { m.Transcript.NativeRelPath = "/etc/passwd" },
		"native traversal":     func(m *Manifest) { m.Transcript.NativeRelPath = "sessions/../../x" },
		"subdir traversal":     func(m *Manifest) { m.Repo.Subdir = "../.." },
		"subdir absolute":      func(m *Manifest) { m.Repo.Subdir = "/etc" },
		"subdir inner climb":   func(m *Manifest) { m.Repo.Subdir = "a/../../b" },
		"subdir into .git":     func(m *Manifest) { m.Repo.Subdir = "a/.git/hooks" },
	} {
		m := ok
		mutate(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: manifest accepted", name)
		}
	}
}

// Members are held in memory, so their size is bounded before they are read.
func TestOpenRefusesOversizedMembers(t *testing.T) {
	defer func(old int64) { MaxMemberBytes = old }(MaxMemberBytes)
	MaxMemberBytes = 1024

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	big := bytes.Repeat([]byte("a"), 4096)
	if err := tw.WriteHeader(&tar.Header{Name: "native/big", Mode: 0o600, Size: int64(len(big)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(big); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := Open(bytes.NewReader(buf.Bytes())); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized member = %v, want a size refusal", err)
	}
}

// The continuation is written into the checkout the bundle just restored, so a link the patch
// planted there must not carry it anywhere else.
func TestWriteContinuationRefusesSymlinks(t *testing.T) {
	_, work := newRepoPair(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(work, ".conductor")); err != nil {
		t.Fatal(err)
	}
	var b Builder
	if err := b.Add(ContinuationPath, []byte("# continue\n")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Schema: Schema, ID: "ck-evil", CreatedAt: time.Now().UTC(), Harness: "claude", SessionID: "s"}
	var buf bytes.Buffer
	if err := b.Seal(&m, &buf); err != nil {
		t.Fatal(err)
	}
	bundle, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteContinuation(bundle, work); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("WriteContinuation through a link = %v, want a refusal naming the link", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("the continuation was written outside the checkout: %v", entries)
	}

	// Without the link it lands where it always did.
	if err := os.Remove(filepath.Join(work, ".conductor")); err != nil {
		t.Fatal(err)
	}
	p, err := WriteContinuation(bundle, work)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(".conductor", "generated", "continuations", "ck-evil.md"); !strings.HasSuffix(p, want) {
		t.Errorf("continuation at %s, want it under the checkout's %s", p, want)
	} else if _, err := os.Stat(filepath.Join(work, want)); err != nil {
		t.Errorf("continuation not in the checkout: %v", err)
	}

	bundle.Manifest.ID = "../../x"
	if _, err := WriteContinuation(bundle, work); err == nil {
		t.Error("an unsafe id named the continuation file")
	}
}
