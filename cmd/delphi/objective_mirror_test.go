package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeConductor records what the mirror sends: tasks (with parents), timeline events and
// settles. down makes every call fail, as an outage would.
type fakeConductor struct {
	mu       sync.Mutex
	tasks    map[string]map[string]any // id -> create body
	byExt    map[string]string
	timeline []map[string]any // {task, type, payload}
	settled  map[string]string
	raw      []string
	down     bool
}

func newFakeConductor(t *testing.T) *fakeConductor {
	f := &fakeConductor{tasks: map[string]map[string]any{}, byExt: map[string]string{}, settled: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			http.Error(w, `{"error":"down","code":"unavailable"}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, `{"error":"auth"}`, http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		f.raw = append(f.raw, string(b))
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.Method == "POST" && len(parts) == 4 && parts[3] == "tasks":
			ext := body["external_ref"].(string)
			if _, ok := f.byExt[ext]; ok {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `{"error":"duplicate","code":"duplicate"}`)
				return
			}
			id := fmt.Sprintf("id-%d", len(f.tasks)+1)
			f.tasks[id], f.byExt[ext] = body, id
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%q,"ref":"T-%d"}`, id, len(f.tasks))
		case r.Method == "GET" && len(parts) == 4 && parts[3] == "tasks":
			id := f.byExt[r.URL.Query().Get("external_ref")]
			fmt.Fprintf(w, `{"tasks":[{"id":%q,"ref":"T-x"}]}`, id)
		case r.Method == "POST" && len(parts) == 4 && parts[3] == "timeline":
			f.timeline = append(f.timeline, map[string]any{"task": parts[2], "type": body["type"], "payload": body["payload"]})
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case r.Method == "POST" && len(parts) == 4 && parts[3] == "settle":
			f.settled[parts[2]] = body["status"].(string)
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, `{"error":"no route"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONDUCTOR_ENDPOINT", srv.URL)
	t.Setenv("CONDUCTOR_TOKEN", "tok")
	t.Setenv("CONDUCTOR_PROJECT", "app")
	return f
}

func TestObjectiveMirrorsItsTreeIntoConductorTasksAndTimelines(t *testing.T) {
	fa, fo := servers(t)
	objectiveRegistry(t)
	cond := newFakeConductor(t)
	fa.mu.Lock()
	fa.outcome = func(id string) string {
		if id == "n0-plan-a0" {
			return claudeMsg(twoWorkItems, "end_turn", "")
		}
		return claudeMsg("SECRET ANSWER "+id, "end_turn", "")
	}
	fa.mu.Unlock()
	fo.mu.Lock()
	fo.outcome = func(id string, _ map[string]any) (string, string) {
		if id == "n0_2-review-a0" {
			return oaOK(id, `{"verdict":"reject","feedback":"PRIVATE FEEDBACK"}`, "completed"), ""
		}
		return oaOK(id, `{"verdict":"accept","feedback":"ok"}`, "completed"), ""
	}
	fo.mu.Unlock()

	out := filepath.Join(t.TempDir(), "obj")
	code, stdout, se := cli(t, "", "objective", "init", "-out", out, "-objective", "Compare A and B",
		"-criterion", "both covered", "-budget-usd", "1", "-conductor")
	if code != 0 || !strings.Contains(stdout, "mirrored to Conductor as task T-1") {
		t.Fatalf("init: %d\n%s\n%s", code, stdout, se)
	}
	// Conductor goes down mid-run: the tree carries on, and the mirror catches up after.
	cond.mu.Lock()
	cond.down = true
	cond.mu.Unlock()
	if code, so, se := cli(t, "", "objective", "run", "-out", out, "-every", "1ms"); code != 0 {
		t.Fatalf("run: %d\n%s\n%s", code, so, se)
	}
	cond.mu.Lock()
	cond.down = false
	cond.mu.Unlock()
	if code, so, se := cli(t, "", "objective", "sync", "-out", out); code != 0 || !strings.Contains(so, "up to date") {
		t.Fatalf("sync: %d\n%s\n%s", code, so, se)
	}

	cond.mu.Lock()
	defer cond.mu.Unlock()
	if len(cond.tasks) != 3 {
		t.Fatalf("tasks: %v", cond.tasks)
	}
	root := cond.byExt[cond.tasks["id-1"]["external_ref"].(string)]
	children := 0
	for id, body := range cond.tasks {
		if id != root && body["parent"] == root {
			children++
		}
		if labels := fmt.Sprint(body["labels"]); !strings.Contains(labels, "delphi-objective") || body["status"] != "proposed" {
			t.Errorf("task %s: labels %s status %v", id, labels, body["status"])
		}
	}
	if children != 2 {
		t.Errorf("%d children point at the root task", children)
	}
	types := map[string]int{}
	for _, e := range cond.timeline {
		types[e["type"].(string)]++
	}
	for _, want := range []string{"objective.created", "objective.routed", "objective.delegated", "objective.answered",
		"objective.decomposed", "objective.checked", "objective.reviewer", "objective.synthesized", "objective.settled"} {
		if types[want] == 0 {
			t.Errorf("no %s on any timeline (got %v)", want, types)
		}
	}
	if len(cond.settled) != 3 || cond.settled[root] != "done" {
		t.Errorf("settled: %v", cond.settled)
	}
	// Nothing a model wrote as an answer or a review crossed over.
	for _, raw := range cond.raw {
		if strings.Contains(raw, "SECRET ANSWER") || strings.Contains(raw, "PRIVATE FEEDBACK") {
			t.Fatalf("model output reached Conductor: %s", raw)
		}
	}
	// A second sync has nothing to send.
	before := len(cond.raw)
	cond.mu.Unlock()
	code, _, _ = cli(t, "", "objective", "sync", "-out", out)
	cond.mu.Lock()
	if code != 0 || len(cond.raw) != before {
		t.Errorf("resync sent %d calls (exit %d)", len(cond.raw)-before, code)
	}
}

func TestObjectiveMirrorFindsATaskFiledBeforeACrash(t *testing.T) {
	servers(t)
	objectiveRegistry(t)
	cond := newFakeConductor(t)
	out := filepath.Join(t.TempDir(), "obj")
	if code, _, se := cli(t, "", "objective", "init", "-out", out, "-objective", "x", "-criterion", "c", "-budget-usd", "1", "-conductor"); code != 0 {
		t.Fatalf("init: %d %s", code, se)
	}
	// Forget the task, as if the process died after filing it but before saving.
	m, err := loadMirror(out)
	if err != nil {
		t.Fatal(err)
	}
	m.st.Tasks, m.st.Seq = map[string]mirrorTask{}, 0
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if code, _, se := cli(t, "", "objective", "sync", "-out", out); code != 0 {
		t.Fatalf("sync: %d %s", code, se)
	}
	cond.mu.Lock()
	defer cond.mu.Unlock()
	if len(cond.tasks) != 1 {
		t.Fatalf("a replay filed a second task: %v", cond.tasks)
	}
}

func TestPublicReasonQuotesNoModel(t *testing.T) {
	for in, want := range map[string]string{
		"all 2 attempt(s) rejected; the last: rejected by openai:gpt: it is wrong because X": "all 2 attempt(s) rejected",
		"n0.2 failed: all 1 attempt(s) rejected; the last: rejected by a: secret":            "child n0.2 failed",
		"budget exhausted ($1 of $1 spent) and no ancestor could cover it":                   "budget exhausted ($1 of $1 spent)",
		"no independent reviewer: same vendor":                                               "no independent reviewer",
		"review failed: reviewer returned invalid JSON: blah":                                "review failed",
	} {
		if got := publicReason("failed", in); got != want {
			t.Errorf("publicReason(%q) = %q, want %q", in, got, want)
		}
	}
}
