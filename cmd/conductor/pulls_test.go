package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/privacy"
)

// pullsFixture is a control plane's pull request endpoints with canned answers, recording what
// the CLI asked for.
type pullsFixture struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
}

func (f *pullsFixture) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// lastBody is the pull request the most recent link request named.
func (f *pullsFixture) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *pullsFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	now := time.Now()
	pulls := []privacy.PullRequestView{
		{Repository: "acme/widgets", Number: 118, URL: "https://github.com/acme/widgets/pull/118", State: "open", Title: "Retry backoff",
			Author: "octo", AuthorHandle: "olive", TaskRef: "T-42", TaskStatus: domain.TaskVerifying, LinkSource: "trailer", Primary: true,
			HeadRef: "feature/retry", HeadSHA: "0123456789abcdef", BaseRef: "main", Checks: &privacy.PullChecks{Total: 5, Passed: 4, Failed: 1},
			ReviewDecision: "approved", UnresolvedThreads: 2, Mergeable: "clean", Additions: 120, Deletions: 30, ChangedFiles: 5, UpdatedAt: now, CreatedAt: now},
		{Repository: "acme/widgets", Number: 119, URL: "https://github.com/acme/widgets/pull/119", State: "open", Draft: true, Title: "WIP: cache",
			Author: "mallory", Checks: &privacy.PullChecks{Total: 2, Passed: 0, Pending: 2}, UpdatedAt: now, CreatedAt: now},
		{Repository: "acme/widgets", Number: 120, URL: "https://github.com/acme/widgets/pull/120", State: "open", Redacted: true, UpdatedAt: now, CreatedAt: now},
	}
	m := http.NewServeMux()
	record := func(r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	}
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	m.HandleFunc("GET /v1/projects/proj/pulls", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.Header.Get("Authorization") != "Bearer tok" {
			write(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated", "code": "unauthenticated"})
			return
		}
		q := r.URL.Query()
		out := []privacy.PullRequestView{}
		for _, p := range pulls {
			if n := q.Get("number"); n != "" && !strings.HasSuffix(p.URL, "/"+n) {
				continue
			}
			if task := q.Get("task"); task != "" && p.TaskRef != task {
				continue
			}
			if q.Get("unlinked") == "1" && (p.TaskRef != "" || p.Redacted) {
				continue
			}
			if q.Get("state") == "open" && p.State != "open" {
				continue
			}
			out = append(out, p)
		}
		write(w, http.StatusOK, map[string]any{"pulls": out})
	})
	m.HandleFunc("GET /v1/projects/proj/pulls/acme/widgets/{n}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		for _, p := range pulls {
			if strings.HasSuffix(p.URL, "/"+r.PathValue("n")) {
				d := map[string]any{"pull": p}
				if p.TaskRef != "" {
					d["task"] = map[string]any{"ref": p.TaskRef, "status": "verifying"}
					d["attempts"] = []map[string]any{{"attempt_number": 1, "state": "succeeded", "harness": "claude", "branch": "agent/T-42/attempt-1"}}
				}
				write(w, http.StatusOK, d)
				return
			}
		}
		write(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "not_found"})
	})
	m.HandleFunc("POST /v1/tasks/{task}/pull-requests", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body["pull_request"].(string))
		f.mu.Unlock()
		write(w, http.StatusCreated, map[string]any{"linked": true, "task_ref": r.PathValue("task"), "pull": pulls[0]})
	})
	m.HandleFunc("DELETE /v1/tasks/{task}/pull-requests/{owner}/{repo}/{n}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		write(w, http.StatusOK, map[string]any{"unlinked": true, "task_ref": r.PathValue("task")})
	})
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return srv
}

func TestRunPullsListsAndFilters(t *testing.T) {
	fix := &pullsFixture{}
	srv := fix.server(t)
	api := client.New(srv.URL, "tok")
	var out bytes.Buffer
	if err := runPulls(context.Background(), api, "proj", pullsQuery{State: "open", Unlinked: true, Failing: true, NeedsReview: true, Mine: true}, false, &out); err != nil {
		t.Fatal(err)
	}
	asked := fix.seen()[0]
	for _, want := range []string{"state=open", "unlinked=1", "failing=1", "needs_review=1", "mine=1"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the request %q does not ask for %s", asked, want)
		}
	}
	// Unlinked: the draft only (the private one is not shown as unlinked).
	if !strings.Contains(out.String(), "#119") || !strings.Contains(out.String(), "draft") || strings.Contains(out.String(), "#118") {
		t.Errorf("output:\n%s", out.String())
	}

	out.Reset()
	if err := runPulls(context.Background(), api, "proj", pullsQuery{State: "open"}, false, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("output:\n%s", out.String())
	}
	// A failing rollup says how many failed, not how many passed.
	for _, want := range []string{"#118", "open", "T-42", "olive", "1/5 failed", "approved", "Retry backoff"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line for #118 lacks %q: %s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], "2/2 pending") || !strings.Contains(lines[1], "mallory") {
		t.Errorf("line for #119: %s", lines[1])
	}
	// A pull request of someone's private task is its number and state, said so.
	if !strings.Contains(lines[2], "#120") || !strings.Contains(lines[2], "private task") || strings.Contains(lines[2], "Retry") {
		t.Errorf("line for #120: %s", lines[2])
	}
}

func TestRunPullsJSONIsTheServersViews(t *testing.T) {
	fix := &pullsFixture{}
	api := client.New(fix.server(t).URL, "tok")
	var out bytes.Buffer
	if err := runPulls(context.Background(), api, "proj", pullsQuery{State: "all"}, true, &out); err != nil {
		t.Fatal(err)
	}
	var got []privacy.PullRequestView
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 3 || got[0].Number != 118 || got[0].Checks.Failed != 1 || !got[2].Redacted {
		t.Fatalf("--json = %v, %v\n%s", got, err, out.String())
	}
	// An empty list is [], not null, for a script to loop over.
	empty := &pullsFixture{}
	var none bytes.Buffer
	if err := runPulls(context.Background(), client.New(empty.server(t).URL, "tok"), "proj", pullsQuery{State: "all", Task: "T-1"}, true, &none); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(none.String()) != "[]" {
		t.Errorf("an empty --json list = %q", none.String())
	}
}

func TestRunPullShowsTheOnePullRequest(t *testing.T) {
	fix := &pullsFixture{}
	api := client.New(fix.server(t).URL, "tok")
	var out bytes.Buffer
	// A bare number is looked up among the project's pull requests, then fetched in full.
	if err := runPull(context.Background(), api, "proj", pullArg{Number: 118}, false, &out); err != nil {
		t.Fatal(err)
	}
	seen := fix.seen()
	if len(seen) != 2 || !strings.Contains(seen[0], "number=118") || !strings.Contains(seen[1], "/pulls/acme/widgets/118") {
		t.Errorf("requests = %v", seen)
	}
	for _, want := range []string{"acme/widgets#118  Retry backoff", "by octo (olive)", "feature/retry → main", "01234567", "T-42 (verifying, linked by trailer, primary)",
		"4 passed, 1 failed, 0 pending", "approved, 2 unresolved thread(s)", "clean", "+120 −30 in 5 file(s)", "attempts", "agent/T-42/attempt-1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// Named with its repository, no lookup is needed.
	fix2 := &pullsFixture{}
	out.Reset()
	if err := runPull(context.Background(), client.New(fix2.server(t).URL, "tok"), "proj", pullArg{Repository: "acme/widgets", Number: 119}, false, &out); err != nil {
		t.Fatal(err)
	}
	if len(fix2.seen()) != 1 || !strings.Contains(out.String(), "none — conductor task link-pr <task> 119") || !strings.Contains(out.String(), "draft") {
		t.Errorf("unlinked draft:\n%s\n(requests %v)", out.String(), fix2.seen())
	}
	// A private task's pull request says what it is and shows nothing else.
	out.Reset()
	if err := runPull(context.Background(), client.New(fix2.server(t).URL, "tok"), "proj", pullArg{Repository: "acme/widgets", Number: 120}, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "private task") || strings.Contains(out.String(), "checks") {
		t.Errorf("private:\n%s", out.String())
	}
	// Unknown, and unambiguous errors.
	if err := runPull(context.Background(), api, "proj", pullArg{Number: 999}, false, &out); err == nil || !strings.Contains(err.Error(), "no pull request #999") {
		t.Errorf("unknown number: %v", err)
	}
}

func TestParsePullArg(t *testing.T) {
	for in, want := range map[string]pullArg{
		"118":              {Number: 118},
		"#118":             {Number: 118},
		"acme/widgets#118": {Repository: "acme/widgets", Number: 118},
		"https://github.com/acme/widgets/pull/118":     {Repository: "acme/widgets", Number: 118},
		"https://ghe.example.com/acme/widgets/pull/7/": {Repository: "acme/widgets", Number: 7},
		"  42 ": {Number: 42},
	} {
		got, err := parsePullArg(in)
		if err != nil || got != want {
			t.Errorf("parsePullArg(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "T-42", "abc", "acme/widgets", "https://github.com/acme/widgets/issues/3", "-1"} {
		if _, err := parsePullArg(bad); err == nil {
			t.Errorf("parsePullArg(%q) was accepted", bad)
		}
	}
}

func TestRunUnlinkFindsTheRepositoryFromTheTask(t *testing.T) {
	fix := &pullsFixture{}
	api := client.New(fix.server(t).URL, "tok")
	var out bytes.Buffer
	if err := runUnlinkPR(context.Background(), api, "proj", "T-42", pullArg{Number: 118}, false, &out); err != nil {
		t.Fatal(err)
	}
	seen := fix.seen()
	if len(seen) != 2 || !strings.Contains(seen[0], "task=T-42") || seen[1] != "DELETE /v1/tasks/T-42/pull-requests/acme/widgets/118?project=proj" {
		t.Errorf("requests = %v", seen)
	}
	if !strings.Contains(out.String(), "Unlinked acme/widgets#118 from T-42") {
		t.Errorf("output: %q", out.String())
	}
}

// The real binary, against the fixture: `conductor pulls`, `pull`, and `task link-pr` end to end,
// with their flags and --json.
func TestPullsCommandsAgainstTheFixtureServer(t *testing.T) {
	fix := &pullsFixture{}
	srv := fix.server(t)
	run := func(args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(buildCLI(t), args...)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "CONDUCTOR_ENDPOINT="+srv.URL, "CONDUCTOR_TOKEN=tok",
			"CONDUCTOR_PROJECT=proj", "CONDUCTOR_NO_LOCAL_LOGIN=1")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("run %v: %v", args, err)
			}
			code = ee.ExitCode()
		}
		return stdout.String(), stderr.String(), code
	}

	out, errOut, code := run("pulls")
	if code != 0 || !strings.Contains(out, "#118") || !strings.Contains(out, "T-42") || strings.Contains(out, "No pull requests") {
		t.Fatalf("pulls: exit %d\n%s\n%s", code, out, errOut)
	}
	if seen := fix.seen(); !strings.Contains(seen[len(seen)-1], "state=open") {
		t.Errorf("`conductor pulls` is not open-only by default: %v", seen)
	}
	out, _, code = run("pulls", "--unlinked", "--failing", "--needs-review", "--mine", "--all", "--task", "T-42", "--json")
	if code != 0 {
		t.Fatalf("pulls with flags: exit %d", code)
	}
	last := fix.seen()
	for _, want := range []string{"state=all", "unlinked=1", "failing=1", "needs_review=1", "mine=1", "task=T-42"} {
		if !strings.Contains(last[len(last)-1], want) {
			t.Errorf("the flags did not reach the server: %s lacks %s", last[len(last)-1], want)
		}
	}
	var views []privacy.PullRequestView
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Errorf("--json output is not JSON: %v\n%s", err, out)
	}

	out, _, code = run("pull", "118", "--json")
	var detail pullDetail
	if code != 0 || json.Unmarshal([]byte(out), &detail) != nil || detail.Pull.Number != 118 || detail.Task == nil && detail.Pull.TaskRef != "T-42" {
		t.Errorf("pull 118 --json: exit %d\n%s", code, out)
	}
	out, _, code = run("pull", "acme/widgets#119")
	if code != 0 || !strings.Contains(out, "WIP: cache") {
		t.Errorf("pull acme/widgets#119: exit %d\n%s", code, out)
	}
	if _, errOut, code := run("pull", "999"); code == 0 || !strings.Contains(errOut, "no pull request #999") {
		t.Errorf("pull 999: exit %d, %q", code, errOut)
	}
	if _, errOut, code := run("pulls", "--open", "--all"); code == 0 || !strings.Contains(errOut, "contradict") {
		t.Errorf("--open --all: exit %d, %q", code, errOut)
	}

	out, _, code = run("task", "link-pr", "T-42", "acme/widgets#118", "--json")
	var linked struct {
		Linked  bool   `json:"linked"`
		TaskRef string `json:"task_ref"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &linked) != nil || !linked.Linked || linked.TaskRef != "T-42" {
		t.Errorf("task link-pr: exit %d\n%s", code, out)
	}
	if got := fix.lastBody(); got != "acme/widgets#118" {
		t.Errorf("link-pr sent %q", got)
	}
	out, _, code = run("task", "link-pr", "T-42", "118")
	if code != 0 || !strings.Contains(out, "Linked acme/widgets#118 to T-42") {
		t.Errorf("task link-pr (text): exit %d\n%s", code, out)
	}
	if _, errOut, code := run("task", "link-pr", "T-42", "nonsense"); code == 0 || !strings.Contains(errOut, "not a pull request") {
		t.Errorf("link-pr nonsense: exit %d, %q", code, errOut)
	}
	out, _, code = run("task", "unlink-pr", "T-42", "acme/widgets#118")
	if code != 0 || !strings.Contains(out, "Unlinked acme/widgets#118 from T-42") {
		t.Errorf("task unlink-pr: exit %d\n%s", code, out)
	}
	if seen := fix.seen(); seen[len(seen)-1] != "DELETE /v1/tasks/T-42/pull-requests/acme/widgets/118?project=proj" {
		t.Errorf("unlink-pr asked %v", seen[len(seen)-1])
	}
}
