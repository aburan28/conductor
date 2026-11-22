package transfer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sessionA = "11111111-1111-4111-8111-111111111111"
	sessionB = "22222222-2222-4222-8222-222222222222"
	project  = "/tmp/demo-app"
)

func claudeLine(t *testing.T, typ, id, cwd, text string) string {
	t.Helper()
	rec := map[string]any{
		"type":      typ,
		"sessionId": id,
		"cwd":       cwd,
		"timestamp": "2026-10-01T10:00:00Z",
	}
	if typ == "user" {
		rec["message"] = map[string]any{"role": "user", "content": text}
	} else {
		rec["message"] = map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": text},
		}}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// fakeClaudeHome writes one transcript for id under the project's Claude folder.
func fakeClaudeHome(t *testing.T, cwd string, sessions ...struct{ id, text string }) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "projects", claudeSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		body := claudeLine(t, "user", s.id, cwd, "hello") + claudeLine(t, "assistant", s.id, cwd, s.text)
		if err := os.WriteFile(filepath.Join(dir, s.id+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestExportAndInspect(t *testing.T) {
	home := fakeClaudeHome(t, project,
		struct{ id, text string }{sessionA, "first answer"},
		struct{ id, text string }{sessionB, "second answer"})

	var buf bytes.Buffer
	rep, err := Export(&buf, ExportOptions{Tool: ToolClaude, ClaudeHome: home, Project: project})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if rep.Sessions != 2 {
		t.Fatalf("exported %d sessions, want 2", rep.Sessions)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if b.Manifest.Tool != ToolClaude || b.Manifest.SourceProjectPath != project {
		t.Logf("manifest: %+v", b.Manifest)
	}
	if len(b.Manifest.Sessions) != 2 {
		t.Fatalf("manifest has %d sessions", len(b.Manifest.Sessions))
	}
	for _, s := range b.Manifest.Sessions {
		if s.SHA256 == "" || s.SizeBytes == 0 {
			t.Errorf("session %s has no digest/size", s.BundlePath)
		}
		if _, err := b.Read(s.BundlePath); err != nil {
			t.Errorf("read %s: %v", s.BundlePath, err)
		}
	}
	ir, err := Inspect(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if ir.SecretsFound != 0 {
		t.Errorf("unexpected secrets: %d", ir.SecretsFound)
	}
	if len(b.Entries()) < 4 {
		t.Errorf("entries = %v", b.Entries())
	}
}

func TestExportProjectFilterAndNothing(t *testing.T) {
	home := fakeClaudeHome(t, project, struct{ id, text string }{sessionA, "hi"})
	home2 := fakeClaudeHome(t, "/tmp/other", struct{ id, text string }{sessionB, "hi"})
	_ = home2

	var buf bytes.Buffer
	_, err := Export(&buf, ExportOptions{Tool: ToolClaude, ClaudeHome: home, Project: "/tmp/does-not-exist"})
	if !errors.Is(err, ErrNothingSelected) {
		t.Fatalf("err = %v, want ErrNothingSelected", err)
	}
}

// TestChecksumTamper builds a valid bundle, rewrites checksums.json to a wrong value, and
// expects Open to refuse it.
func TestChecksumTamper(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, ToolClaude, "/h", project)
	if _, err := w.AddSession(Session{ThreadID: sessionA, BundlePath: "projects/-tmp-demo-app/" + sessionA + ".jsonl", OriginalCWD: project}, strings.NewReader("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	tampered := rewriteZip(t, buf.Bytes(), func(files map[string][]byte) {
		var sums Checksums
		if err := json.Unmarshal(files[ChecksumsName], &sums); err != nil {
			t.Fatal(err)
		}
		sums["projects/-tmp-demo-app/"+sessionA+".jsonl"] = strings.Repeat("0", 64)
		files[ChecksumsName], _ = json.Marshal(sums)
	})
	if _, err := OpenBytes(tampered); err == nil {
		t.Fatal("tampered bundle opened without error")
	}
}

func TestUnsafeBundlePathRejected(t *testing.T) {
	m := Manifest{FormatVersion: FormatVersion, Tool: ToolClaude, CreatedAt: time.Now().Format(time.RFC3339),
		Sessions: []Session{{BundlePath: "../evil.jsonl"}}}
	if err := m.Validate(); err == nil {
		t.Fatal("path traversal was accepted")
	}
}

func TestImportNewIdenticalGrownDiverged(t *testing.T) {
	home := t.TempDir()
	rel := "projects/-tmp-demo-app/" + sessionA + ".jsonl"
	base := claudeLine(t, "user", sessionA, project, "hello")

	writeBundle := func(body string) *Bundle {
		var buf bytes.Buffer
		w := NewWriter(&buf, ToolClaude, "/src", project)
		if _, err := w.AddSession(Session{ThreadID: sessionA, BundlePath: rel, OriginalCWD: project, OriginalPath: "/src/" + rel}, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		b, err := OpenBytes(buf.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// new
	res, err := Import(writeBundle(base), ImportOptions{Tool: ToolClaude, ClaudeHome: home})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].Action != ActionImport {
		t.Fatalf("action = %s, want import", res.Items[0].Action)
	}
	// identical
	res, _ = Import(writeBundle(base), ImportOptions{Tool: ToolClaude, ClaudeHome: home})
	if res.Items[0].Action != ActionSkipIdentical {
		t.Fatalf("action = %s, want skip-identical", res.Items[0].Action)
	}
	// grown without merge -> conflict
	grown := base + claudeLine(t, "assistant", sessionA, project, "more")
	res, _ = Import(writeBundle(grown), ImportOptions{Tool: ToolClaude, ClaudeHome: home})
	if res.Items[0].Action != ActionConflict {
		t.Fatalf("action = %s, want conflict", res.Items[0].Action)
	}
	// grown with merge -> append
	res, err = Import(writeBundle(grown), ImportOptions{Tool: ToolClaude, ClaudeHome: home, Merge: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].Action != ActionUpdate || res.Items[0].LinesAdded != 1 {
		t.Fatalf("item = %+v, want update +1 line", res.Items[0])
	}
	got, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != grown {
		t.Fatalf("merged file = %q, want %q", got, grown)
	}
	// merge is idempotent
	res, _ = Import(writeBundle(grown), ImportOptions{Tool: ToolClaude, ClaudeHome: home, Merge: true})
	if res.Items[0].Action != ActionSkipIdentical {
		t.Fatalf("second merge action = %s", res.Items[0].Action)
	}
	// local ahead -> skip-ahead
	ahead := grown + claudeLine(t, "assistant", sessionA, project, "local only")
	if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(rel)), []byte(ahead), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ = Import(writeBundle(grown), ImportOptions{Tool: ToolClaude, ClaudeHome: home, Merge: true})
	if res.Items[0].Action != ActionSkipAhead {
		t.Fatalf("action = %s, want skip-ahead", res.Items[0].Action)
	}
}

func TestImportDivergedPolicies(t *testing.T) {
	body := claudeLine(t, "user", sessionA, project, "bundle side")
	rel := "projects/-tmp-demo-app/" + sessionA + ".jsonl"
	var buf bytes.Buffer
	w := NewWriter(&buf, ToolClaude, "/src", project)
	if _, err := w.AddSession(Session{ThreadID: sessionA, BundlePath: rel, OriginalCWD: project}, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	// conflict by default
	home := t.TempDir()
	dest := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("local side\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := Import(b, ImportOptions{Tool: ToolClaude, ClaudeHome: home})
	if res.Items[0].Action != ActionConflict {
		t.Fatalf("action = %s", res.Items[0].Action)
	}
	// replace with backup
	res, err = Import(b, ImportOptions{Tool: ToolClaude, ClaudeHome: home, ReplaceWithBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].Action != ActionReplace {
		t.Fatalf("action = %s", res.Items[0].Action)
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 2 {
		t.Fatalf("expected the file and one backup, got %d entries", len(entries))
	}
	// import as copy
	if err := os.WriteFile(dest, []byte("local side\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = Import(b, ImportOptions{Tool: ToolClaude, ClaudeHome: home, ImportAsCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].Action != ActionImportCopy {
		t.Fatalf("action = %s", res.Items[0].Action)
	}
	entries, _ = os.ReadDir(filepath.Dir(dest))
	if len(entries) < 3 {
		t.Fatalf("expected a copy beside the local file, got %d entries", len(entries))
	}
}

func TestImportMapCwd(t *testing.T) {
	body := claudeLine(t, "user", sessionA, project, "hello")
	rel := "projects/-tmp-demo-app/" + sessionA + ".jsonl"
	var buf bytes.Buffer
	w := NewWriter(&buf, ToolClaude, "/src", project)
	if _, err := w.AddSession(Session{ThreadID: sessionA, BundlePath: rel, OriginalCWD: project}, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := OpenBytes(buf.Bytes())

	home := t.TempDir()
	newProject := "/tmp/new-place"
	res, err := Import(b, ImportOptions{Tool: ToolClaude, ClaudeHome: home, MapCwdHere: true, Here: newProject})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].Action != ActionImport {
		t.Fatalf("action = %s", res.Items[0].Action)
	}
	want := filepath.Join(home, "projects", claudeSlug(newProject), sessionA+".jsonl")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected %s: %v", want, err)
	}
	if !strings.Contains(string(data), newProject) || strings.Contains(string(data), `"`+project+`"`) {
		t.Fatalf("cwd was not rewritten: %s", data)
	}
}

func TestSecretGateAndRedact(t *testing.T) {
	home := fakeClaudeHome(t, project, struct{ id, text string }{sessionA, "use AKIAIOSFODNN7EXAMPLE for the deploy"})
	var buf bytes.Buffer
	_, err := Export(&buf, ExportOptions{Tool: ToolClaude, ClaudeHome: home, Project: project})
	var sec *SecretsError
	if !errors.As(err, &sec) {
		t.Fatalf("err = %v, want SecretsError", err)
	}
	if !strings.Contains(sec.Error(), "AKIA") {
		t.Errorf("masked finding leaked nothing identifiable? %v", sec)
	}
	buf.Reset()
	if _, err := Export(&buf, ExportOptions{Tool: ToolClaude, ClaudeHome: home, Project: project, Redact: true}); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := b.Read(b.Manifest.Sessions[0].BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte("AKIAIOSFODNN7EXAMPLE")) {
		t.Fatal("secret survived redaction inside the bundle")
	}
	if !bytes.Contains(plain, []byte("REDACTED")) {
		t.Fatal("no placeholder written")
	}
}

func TestCodexExport(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "10", "01")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	rollout := filepath.Join(dir, "rollout-2026-10-01T10-00-00-"+id+".jsonl")
	meta := `{"timestamp":"2026-10-01T10:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"` + project + `","model_provider":"openai"}}` + "\n" +
		`{"timestamp":"2026-10-01T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello"}}` + "\n"
	if err := os.WriteFile(rollout, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	rep, err := Export(&buf, ExportOptions{Tool: ToolCodex, CodexHome: home, Project: project})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Sessions != 1 {
		t.Fatalf("sessions = %d", rep.Sessions)
	}
	b, err := OpenBytes(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if b.Manifest.Sessions[0].ThreadID != id {
		t.Errorf("thread id = %q", b.Manifest.Sessions[0].ThreadID)
	}
}

func TestSyncExchange(t *testing.T) {
	srcHome := fakeClaudeHome(t, project, struct{ id, text string }{sessionA, "from the desktop"})
	dstHome := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	addrCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		_, err := ServeSync(ctx, "127.0.0.1:0", SyncOptions{
			Tool: ToolClaude, ClaudeHome: srcHome, Project: project,
			Code: "TEST-CODE-1234", Ready: func(a string) { addrCh <- a },
		}, io.Discard)
		errCh <- err
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case <-ctx.Done():
		t.Fatal("server did not start")
	}
	rep, err := ConnectSync(ctx, addr, "TEST-CODE-1234", SyncOptions{Tool: ToolClaude, ClaudeHome: dstHome})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if rep.Received != 1 {
		t.Fatalf("received %d, want 1", rep.Received)
	}
	got, err := os.ReadFile(filepath.Join(dstHome, "projects", claudeSlug(project), sessionA+".jsonl"))
	if err != nil {
		t.Fatalf("imported transcript missing: %v", err)
	}
	if !strings.Contains(string(got), "from the desktop") {
		t.Errorf("unexpected content: %s", got)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("server did not finish")
	}
}

// rewriteZip rebuilds a ZIP with the given mutation applied to its members.
func rewriteZip(t *testing.T, raw []byte, mutate func(map[string][]byte)) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := b.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		rc.Close()
		files[f.Name] = b.Bytes()
	}
	mutate(files)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
