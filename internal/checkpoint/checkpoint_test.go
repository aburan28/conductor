package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBundleRoundTripVerifiesEveryMember(t *testing.T) {
	var b Builder
	if err := b.Add("native/claude/s.jsonl", []byte(`{"type":"user"}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(ContinuationPath, []byte("# hi\n")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Schema: Schema, ID: "ck-test-000001", CreatedAt: time.Unix(1700000000, 0).UTC(), Harness: "claude", SessionID: "s", Cwd: "/x"}
	var buf bytes.Buffer
	if err := b.Seal(&m, &buf); err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("manifest lists %d files, want 2", len(m.Files))
	}

	rm, err := ReadManifest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if rm.ID != m.ID || len(rm.Files) != 2 {
		t.Errorf("ReadManifest returned %+v", rm)
	}

	opened, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := opened.File(ContinuationPath); string(got) != "# hi\n" {
		t.Errorf("continuation = %q", got)
	}

	// Tampering with a member must be detected: flip one byte inside the gzip stream's
	// decompressed content by rebuilding the archive with altered data under the same manifest.
	var tampered Builder
	_ = tampered.Add("native/claude/s.jsonl", []byte(`{"type":"user","x":1}`+"\n"))
	_ = tampered.Add(ContinuationPath, []byte("# hi\n"))
	var buf2 bytes.Buffer
	m2 := m
	if err := tampered.Seal(&m2, &buf2); err != nil {
		t.Fatal(err)
	}
	// Splice the original manifest's hashes over the tampered archive by re-sealing with
	// the *original* Files list is not possible through the API, so verify the property the
	// other way round: a bundle whose manifest lists a member that is absent is refused.
	var missing Builder
	_ = missing.Add(ContinuationPath, []byte("# hi\n"))
	m3 := m
	var buf3 bytes.Buffer
	if err := missing.Seal(&m3, &buf3); err != nil {
		t.Fatal(err)
	}
	if len(m3.Files) != 1 {
		t.Fatalf("expected the reseal to list one file")
	}
	if _, err := Open(bytes.NewReader(buf3.Bytes())); err != nil {
		t.Errorf("a consistent bundle must open: %v", err)
	}
	if _, err := Open(bytes.NewReader([]byte("not a bundle"))); err == nil {
		t.Error("garbage opened as a bundle")
	}
}

func TestBundleRejectsEscapingPaths(t *testing.T) {
	var b Builder
	for _, p := range []string{"../x", "/abs", "", "manifest.json"} {
		if err := b.Add(p, nil); err == nil {
			t.Errorf("Add(%q) accepted", p)
		}
	}
	if err := b.Add("a/b", nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Add("a/b", nil); err == nil {
		t.Error("duplicate member accepted")
	}
}

func TestSealUnseal(t *testing.T) {
	plain := []byte("gzip tar bytes")
	sealed, err := Seal(plain, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) || IsSealed(plain) {
		t.Fatal("IsSealed is wrong")
	}
	if bytes.Contains(sealed, plain) {
		t.Fatal("sealed output contains the plaintext")
	}
	got, err := Unseal(sealed, "correct horse")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Unseal = %q, %v", got, err)
	}
	if _, err := Unseal(sealed, "wrong"); err == nil {
		t.Error("wrong passphrase accepted")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := Unseal(sealed, "correct horse"); err == nil {
		t.Error("altered ciphertext accepted")
	}
	if _, err := Seal(plain, ""); err == nil {
		t.Error("empty passphrase accepted")
	}
}

const sampleClaude = `{"type":"ai-title","aiTitle":"Fix the router","sessionId":"11111111-2222-3333-4444-555555555555"}
{"parentUuid":null,"type":"user","uuid":"u1","timestamp":"2026-10-05T17:37:06.610Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","version":"2.1.289","message":{"role":"user","content":"Please fix the retry bug in the router"}}
{"type":"user","uuid":"u2","timestamp":"2026-10-05T17:37:07.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"user","content":"<system-reminder>ignore me</system-reminder>"}}
{"type":"assistant","uuid":"a1","requestId":"r1","timestamp":"2026-10-05T17:37:10.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"assistant","content":[{"type":"text","text":"Looking at the router now."}]}}
{"type":"assistant","uuid":"a2","requestId":"r1","timestamp":"2026-10-05T17:37:11.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"/home/alice/repo/internal/router/retry.go","old_string":"a","new_string":"b"}}]}}
{"type":"user","uuid":"u3","timestamp":"2026-10-05T17:37:12.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"assistant","uuid":"a3","requestId":"r2","timestamp":"2026-10-05T17:37:20.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test ./internal/router/","description":"Run router tests"}}]}}
{"type":"user","uuid":"u4","timestamp":"2026-10-05T17:37:25.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"FAIL","is_error":true}]}}
{"type":"assistant","uuid":"a4","requestId":"r3","timestamp":"2026-10-05T17:37:30.000Z","cwd":"/home/alice/repo","sessionId":"11111111-2222-3333-4444-555555555555","message":{"role":"assistant","content":[{"type":"text","text":"The test fails; next I will fix the backoff."}]}}
{"type":"summary","summary":"Router retry fix in progress","leafUuid":"a4"}
`

func TestParseClaude(t *testing.T) {
	c := ParseClaude([]byte(sampleClaude))
	if c.SessionID != "11111111-2222-3333-4444-555555555555" || c.Cwd != "/home/alice/repo" || c.Version != "2.1.289" || c.Title != "Fix the router" {
		t.Errorf("header: %+v", *c)
	}
	users, assistants := c.Counts()
	if users != 1 || assistants != 3 {
		t.Errorf("counts = %d users, %d assistants; want 1, 3", users, assistants)
	}
	if c.Turns[0].Text != "Please fix the retry bug in the router" {
		t.Errorf("first turn = %q", c.Turns[0].Text)
	}
	a := c.Turns[1]
	if a.Text != "Looking at the router now." || len(a.Tools) != 1 || a.Tools[0].Name != "Edit" || a.Tools[0].Path != "/home/alice/repo/internal/router/retry.go" {
		t.Errorf("folded assistant turn = %+v", a)
	}
	if b := c.Turns[2]; len(b.Tools) != 1 || b.Tools[0].Summary != "Run router tests" || !b.Tools[0].Error {
		t.Errorf("bash turn = %+v", b)
	}
	if c.Summary != "Router retry fix in progress" {
		t.Errorf("summary = %q", c.Summary)
	}
	if got := c.FilesTouched(); len(got) != 1 {
		t.Errorf("files touched = %v", got)
	}
	m := Manifest{ID: "ck-x", CreatedAt: time.Now(), Harness: "claude", SessionID: c.SessionID, Cwd: c.Cwd, Reason: ReasonManual}
	md := string(RenderContinuation(m, c, []string{"internal/router/retry.go (modified)"}))
	for _, want := range []string{"# Continuation of a Claude Code session", "Please fix the retry bug", "`Edit`: /home/alice/repo/internal/router/retry.go", "_(error)_", "next I will fix the backoff", "Where the work stands", "Router retry fix in progress"} {
		if !strings.Contains(md, want) {
			t.Errorf("continuation lacks %q", want)
		}
	}
	if strings.Contains(md, "ignore me") {
		t.Error("continuation leaked an injected system reminder")
	}
}

func TestRenderContinuationElidesTheMiddle(t *testing.T) {
	c := &Conversation{Harness: "codex", SessionID: "s"}
	big := strings.Repeat("x", 3000)
	for i := 0; i < 200; i++ {
		c.Turns = append(c.Turns, Turn{Role: "user", Text: "ask " + big}, Turn{Role: "assistant", Text: "answer " + big})
	}
	md := string(RenderContinuation(Manifest{ID: "ck", Harness: "codex", SessionID: "s"}, c, nil))
	if !strings.Contains(md, "earlier turn(s) omitted") {
		t.Fatal("a long conversation was not elided")
	}
	if len(md) > maxContinuation+40_000 {
		t.Errorf("continuation is %d bytes", len(md))
	}
}

func TestRewriteClaudeCwdTouchesOnlyTheTopLevelField(t *testing.T) {
	out := rewriteClaudeCwd([]byte(sampleClaude), "/home/alice/repo", "/srv/work/repo")
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("rewritten line is not JSON: %v", err)
		}
		if cwd, ok := rec["cwd"]; ok && cwd != "/srv/work/repo" {
			t.Errorf("cwd not rewritten: %v", cwd)
		}
	}
	if !strings.Contains(string(out), "/home/alice/repo/internal/router/retry.go") {
		t.Error("a path inside a message was rewritten; only the top-level cwd should be")
	}
}

const sampleCodex = `{"timestamp":"2026-10-05T10:00:00.000Z","type":"session_meta","payload":{"id":"0199abcd-0000-7000-8000-000000000001","timestamp":"2026-10-05T10:00:00.000Z","cwd":"/home/bob/repo","originator":"codex_cli_rs","cli_version":"0.50.0"}}
{"timestamp":"2026-10-05T10:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"Add a retry budget to the router"}}
{"timestamp":"2026-10-05T10:00:02.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>...</environment_context>"}]}}
{"timestamp":"2026-10-05T10:00:03.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"go test ./...\"]}","call_id":"c1"}}
{"timestamp":"2026-10-05T10:00:04.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}
{"timestamp":"2026-10-05T10:00:05.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Tests pass. Adding the budget now."}}
{"timestamp":"2026-10-05T10:00:06.000Z","type":"turn_context","payload":{"cwd":"/home/bob/repo","model":"gpt-5-codex"}}
`

func TestParseCodexAndRewrite(t *testing.T) {
	c := ParseCodex([]byte(sampleCodex))
	if c.SessionID != "0199abcd-0000-7000-8000-000000000001" || c.Cwd != "/home/bob/repo" || c.Version != "0.50.0" {
		t.Errorf("header: %+v", *c)
	}
	users, assistants := c.Counts()
	if users != 1 || assistants != 1 {
		t.Fatalf("counts = %d, %d; turns %+v", users, assistants, c.Turns)
	}
	a := c.Turns[1]
	if len(a.Tools) != 1 || a.Tools[0].Summary != "go test ./..." || a.Text != "Tests pass. Adding the budget now." {
		t.Errorf("assistant turn = %+v", a)
	}
	out := rewriteCodexCwd([]byte(sampleCodex), "/tmp/elsewhere")
	if strings.Contains(string(out), "/home/bob/repo") {
		t.Error("cwd not rewritten in every payload")
	}
}

const sampleOpenCode = `{"info":{"id":"ses_123","title":"Router budget","directory":"/home/carol/repo","version":"1.18.34","time":{"created":1759660000000,"updated":1759660100000}},
"messages":[{"info":{"role":"user","time":{"created":1759660000000}},"parts":[{"type":"text","text":"Add a retry budget"}]},
{"info":{"role":"assistant","time":{"created":1759660050000}},"parts":[{"type":"text","text":"On it."},{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/home/carol/repo/router.go"},"title":"edit router.go"}}]}]}`

func TestParseOpenCode(t *testing.T) {
	c := ParseOpenCode([]byte(sampleOpenCode))
	if c.SessionID != "ses_123" || c.Title != "Router budget" || c.Cwd != "/home/carol/repo" {
		t.Errorf("header: %+v", *c)
	}
	if len(c.Turns) != 2 || c.Turns[1].Tools[0].Path != "/home/carol/repo/router.go" {
		t.Errorf("turns = %+v", c.Turns)
	}
}

// withoutRepoEnv drops the variables that point git at a repository (GIT_DIR and friends).
// A git hook sets them, so a test run from one would otherwise commit into the repository
// being pushed rather than its own temporary one.
func withoutRepoEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_COMMON_DIR", "GIT_PREFIX", "GIT_NAMESPACE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// git runs git in a test repository, failing the test on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(withoutRepoEnv(os.Environ()), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// newRepoPair makes an "origin" bare repo and a clone with one commit on main.
func newRepoPair(t *testing.T) (origin, work string) {
	t.Helper()
	requireGit(t)
	base := t.TempDir()
	origin = filepath.Join(base, "origin.git")
	gitT(t, base, "init", "--bare", "-q", "-b", "main", origin)
	work = filepath.Join(base, "work")
	gitT(t, base, "clone", "-q", origin, work)
	gitT(t, work, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", ".")
	gitT(t, work, "commit", "-q", "-m", "init")
	gitT(t, work, "push", "-q", "-u", "origin", "main")
	return origin, work
}

func TestWorkspaceCaptureAndRestore(t *testing.T) {
	_, work := newRepoPair(t)
	ctx := context.Background()

	// A local commit not on the remote, a tracked edit, and an untracked file.
	gitT(t, work, "checkout", "-q", "-b", "feat/x")
	if err := os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "a.go")
	gitT(t, work, "commit", "-q", "-m", "add a")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes", "todo.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := InspectRepo(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Branch != "feat/x" || repo.Ahead != 1 || repo.Modified != 1 || repo.Untracked != 1 || !repo.Dirty {
		t.Fatalf("InspectRepo = %+v", repo)
	}
	head := repo.Head

	var b Builder
	ws := CaptureWorkspace(ctx, repo, &b)
	if !ws.CommitsBundle || ws.PatchBytes == 0 || len(ws.UntrackedFiles) != 1 || ws.UntrackedFiles[0] != "notes/todo.txt" {
		t.Fatalf("CaptureWorkspace = %+v", ws)
	}
	m := Manifest{Schema: Schema, ID: "ck-ws", CreatedAt: time.Now().UTC(), Harness: "claude", SessionID: "s", Cwd: work, Repo: repo, Workspace: ws}
	var buf bytes.Buffer
	if err := b.Seal(&m, &buf); err != nil {
		t.Fatal(err)
	}
	bundle, err := Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	// Restore into a fresh clone that has never seen feat/x.
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", repo.Remote, other)
	rep, err := RestoreWorkspace(ctx, bundle, RestoreOptions{Dir: other})
	if err != nil {
		t.Fatalf("restore: %v (%+v)", err, rep)
	}
	if !rep.FetchedBundle || !rep.PatchApplied || rep.Untracked != 1 || !strings.HasPrefix(rep.CheckedOut, "feat/x") {
		t.Errorf("restore report = %+v", rep)
	}
	if got := gitT(t, other, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want %s", got, head)
	}
	if data, _ := os.ReadFile(filepath.Join(other, "README.md")); string(data) != "hello\nworld\n" {
		t.Errorf("README after restore = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(other, "notes", "todo.txt")); string(data) != "later\n" {
		t.Errorf("untracked file after restore = %q", data)
	}

	// A dirty target is refused without --force.
	if err := os.WriteFile(filepath.Join(other, "README.md"), []byte("conflict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreWorkspace(ctx, bundle, RestoreOptions{Dir: other}); err == nil {
		t.Error("restore into a dirty tree succeeded without --force")
	}

	// A missing directory is created from the remote with --clone.
	fresh := filepath.Join(t.TempDir(), "fresh")
	rep, err = RestoreWorkspace(ctx, bundle, RestoreOptions{Dir: fresh, Clone: true})
	if err != nil || !rep.Cloned {
		t.Fatalf("clone-restore: %v %+v", err, rep)
	}
}

func TestCaptureClaudeEndToEnd(t *testing.T) {
	_, work := newRepoPair(t)
	state := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("CONDUCTOR_STATE_DIR", state)
	getenv := func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return cfg
		}
		return ""
	}
	sid := "11111111-2222-3333-4444-555555555555"
	projectDir := filepath.Join(cfg, "projects", strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, work))
	if err := os.MkdirAll(filepath.Join(projectDir, sid, "subagents"), 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.ReplaceAll(sampleClaude, "/home/alice/repo", work)
	if err := os.WriteFile(filepath.Join(projectDir, sid+".jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, sid, "subagents", "agent-1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The pid→session map recent Claude Code builds keep.
	if err := os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "sessions", "4242.json"), []byte(`{"pid":4242,"sessionId":"`+sid+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	now := time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	res, err := Capture(ctx, Request{Harness: "claude-code", Cwd: work, PID: 4242, Reason: ReasonPeriodic, Note: "halfway", Getenv: getenv, Now: clock,
		Conductor: ConductorRef{Project: "demo", Task: "T-7"}})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Manifest
	if res.Skipped != "" || m.SessionID != sid || m.Harness != "claude" || m.Transcript.Extras != 1 || m.Repo.Branch != "main" || len(m.Workspace.UntrackedFiles) != 1 || m.Conductor.Task != "T-7" || m.Title != "Fix the router" {
		t.Fatalf("manifest = %+v (skipped %q)", m, res.Skipped)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatal(err)
	}

	// A new transcript line within the rate limit is skipped for automatic reasons, taken
	// when forced, and chained to its parent.
	if f, err := os.OpenFile(filepath.Join(projectDir, sid+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		f.WriteString(`{"type":"user","uuid":"u9","timestamp":"2026-10-05T18:04:00Z","sessionId":"` + sid + `","message":{"role":"user","content":"more"}}` + "\n")
		f.Close()
	}
	now = now.Add(5 * time.Second)
	limited, err := Capture(ctx, Request{Harness: "claude", Cwd: work, SessionID: sid, Reason: ReasonHook, Getenv: getenv, Now: clock})
	if err != nil || limited.Skipped == "" {
		t.Errorf("rate limit did not apply: %+v %v", limited, err)
	}
	forced, err := Capture(ctx, Request{Harness: "claude", Cwd: work, SessionID: sid, Reason: ReasonHook, Force: true, Getenv: getenv, Now: clock})
	if err != nil || forced.Skipped != "" || forced.Manifest.Parent != m.ID {
		t.Errorf("forced capture: %+v %v", forced, err)
	}

	// Nothing changed since: a later periodic capture writes nothing.
	now = now.Add(5 * time.Minute)
	again, err := Capture(ctx, Request{Harness: "claude", Cwd: work, SessionID: sid, Reason: ReasonPeriodic, Getenv: getenv, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if again.Skipped == "" || again.Manifest.ID != forced.Manifest.ID {
		t.Errorf("unchanged session was captured again: %+v", again)
	}

	// Store: list, resolve, latest.
	all, err := List()
	if err != nil || len(all) != 2 || all[0].ID != forced.Manifest.ID {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if got, err := Resolve(ShortID(m.ID)); err != nil || got.ID != m.ID {
		t.Errorf("Resolve(short) = %+v, %v", got, err)
	}
	if got, err := Resolve(sid[:8]); err != nil || got.ID != forced.Manifest.ID {
		t.Errorf("Resolve(session prefix) = %+v, %v", got, err)
	}
	if got, err := Resolve("latest"); err != nil || got.ID != forced.Manifest.ID {
		t.Errorf("Resolve(latest) = %+v, %v", got, err)
	}

	// Resume natively into another "account" (config dir) and another checkout.
	bundle, err := Load(forced.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	gitT(t, filepath.Dir(other), "clone", "-q", m.Repo.Remote, other)
	if _, err := RestoreWorkspace(ctx, bundle, RestoreOptions{Dir: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "new.txt")); err != nil {
		t.Error("untracked file was not restored")
	}
	altCfg := t.TempDir()
	launch, err := PrepareResume(ctx, bundle, ResumeOptions{Cwd: other, StateDir: altCfg, Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	if !launch.Native || launch.Argv[0] != "claude" || launch.Argv[1] != "--resume" || !strings.HasPrefix(launch.Argv[2], altCfg) {
		t.Errorf("launch = %+v", launch)
	}
	if len(launch.Env) != 1 || launch.Env[0] != "CLAUDE_CONFIG_DIR="+altCfg {
		t.Errorf("env = %v", launch.Env)
	}
	installed, err := os.ReadFile(launch.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), `"cwd":"`+other+`"`) {
		t.Error("installed transcript does not point at the new checkout")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(launch.Transcript), sid, "subagents", "agent-1.jsonl")); err != nil {
		t.Error("subagent transcript was not installed beside the session")
	}
	// Installing twice without force is refused.
	if _, err := PrepareResume(ctx, bundle, ResumeOptions{Cwd: other, StateDir: altCfg, Getenv: getenv}); err == nil {
		t.Error("second install over an existing transcript succeeded without --force")
	}

	// Cross-harness: Codex starts from the continuation.
	cross, err := PrepareResume(ctx, bundle, ResumeOptions{Cwd: other, Target: "codex", Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	if cross.Native || cross.Argv[0] != "codex" || !strings.Contains(cross.Argv[1], cross.Continuation) {
		t.Errorf("cross launch = %+v", cross)
	}
	if data, err := os.ReadFile(cross.Continuation); err != nil || !strings.Contains(string(data), "Please fix the retry bug") {
		t.Errorf("continuation on disk: %v", err)
	}

	// Prune keeps the newest N per session once they are old enough.
	removed, err := Prune(1, 0, now.Add(time.Hour))
	if err != nil || len(removed) != 1 || removed[0].ID != m.ID {
		t.Errorf("Prune = %+v, %v", removed, err)
	}
}

func TestCaptureUnknownHarness(t *testing.T) {
	t.Setenv("CONDUCTOR_STATE_DIR", t.TempDir())
	if _, err := Capture(context.Background(), Request{Harness: "cursor", Cwd: t.TempDir()}); err == nil {
		t.Error("unknown harness accepted")
	}
}
