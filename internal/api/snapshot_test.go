package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/domain"
)

// The dashboard's per-session Save and Export gather the same server-side record: the
// session view, the capability record, and the inbox. Save additionally persists it under
// the project's .conductor/snapshots, so a snapshot file must land there and read back.

func TestSessionSaveWritesSnapshotFile(t *testing.T) {
	h := newHarness(t)
	h.seedProfiles(t)

	// An offered task gives the snapshot an assignment to carry.
	session := h.registerSession(t, h.aliceTok, map[string]any{"model": "test-opus"})
	task := h.createTask(t, h.aliceTok, "Snapshot me")
	if code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+task.ID+"/assign",
		map[string]any{"session_id": session.ID, "require": map[string]any{}}); code != http.StatusCreated {
		t.Fatalf("assign = %d\n%s", code, body)
	}

	root := t.TempDir()
	if err := h.store.SetProjectRepoPath(context.Background(), h.project.ID, root); err != nil {
		t.Fatalf("set repo path: %v", err)
	}

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/sessions/"+session.ID+"/save", nil)
	if code != http.StatusCreated {
		t.Fatalf("save own session = %d\n%s", code, body)
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode save response: %v\n%s", err, body)
	}
	if !strings.HasPrefix(out.Path, filepath.Join(root, ".conductor", "snapshots")) {
		t.Fatalf("path %q is not under .conductor/snapshots in the project root", out.Path)
	}

	data, err := os.ReadFile(out.Path)
	if err != nil {
		t.Fatalf("snapshot file: %v", err)
	}
	var snap struct {
		Session struct {
			ID           domain.ID   `json:"id"`
			State        string       `json:"state"`
			Principal    string       `json:"principal"`
			Capabilities struct {
				Model string `json:"model"`
			} `json:"capabilities"`
		} `json:"session"`
		Capability struct {
			Tier domain.Tier `json:"tier"`
		} `json:"capability"`
		Assignments []struct {
			TaskID domain.ID `json:"task_id"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("snapshot is not JSON: %v\n%s", err, data)
	}
	if snap.Session.ID != session.ID || snap.Session.Principal != "alice" {
		t.Errorf("snapshot session = %+v", snap.Session)
	}
	if snap.Session.Capabilities.Model != "test-opus" {
		t.Errorf("snapshot model = %q, want test-opus", snap.Session.Capabilities.Model)
	}
	if snap.Capability.Tier == "" {
		t.Error("snapshot carries no capability record")
	}
	if len(snap.Assignments) != 1 || snap.Assignments[0].TaskID != task.ID {
		t.Errorf("snapshot assignments = %+v, want the offered task", snap.Assignments)
	}

	// A teammate may snapshot a session, but the inbox is the owner's: a contributor who
	// is not the principal gets everything but the assignments.
	code, body = h.do(h.bobTok, http.MethodPost, "/v1/sessions/"+session.ID+"/save", nil)
	if code != http.StatusCreated {
		t.Fatalf("save as teammate = %d\n%s", code, body)
	}
	var theirs struct {
		Assignments []json.RawMessage `json:"assignments"`
	}
	if err := json.Unmarshal(body, &theirs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(theirs.Assignments) != 0 {
		t.Errorf("a contributor could read another principal's inbox: %+v", theirs.Assignments)
	}

	// Someone outside the project cannot reach the session at all.
	if code, _ := h.do(h.outTok, http.MethodPost, "/v1/sessions/"+session.ID+"/save", nil); code != http.StatusNotFound {
		t.Errorf("outsider save = %d, want 404", code)
	}
}

// The download: same JSON, plus the headers that make a browser save it as a file. The
// token rides the query string here, because a browser download cannot set headers — the
// same allowance the SSE streams need.
func TestSessionExportHeadersAndBody(t *testing.T) {
	h := newHarness(t)
	session := h.registerSession(t, h.bobTok, map[string]any{"model": "test-haiku"})

	req, err := http.NewRequest(http.MethodGet,
		h.server.URL+"/v1/sessions/"+session.ID+"/export?token="+h.bobTok, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d, want 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "attachment; filename=session-"+session.ID+".json" {
		t.Errorf("content-disposition = %q", cd)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}

	var snap struct {
		Session struct {
			ID domain.ID `json:"id"`
		} `json:"session"`
		Assignments []json.RawMessage `json:"assignments"`
		SavedAt     string            `json:"saved_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("export body is not JSON: %v", err)
	}
	if snap.Session.ID != session.ID {
		t.Errorf("exported session = %s, want %s", snap.Session.ID, session.ID)
	}
	if snap.SavedAt == "" {
		t.Error("export carries no saved_at; a file found on disk later says nothing about its moment")
	}
	if snap.Assignments == nil {
		t.Error("export omits the assignments key entirely")
	}
}
