package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/notify"
	"github.com/adamburan/conductor/internal/secretbox"
)

// withNotifications swaps the harness's server for one with notifications enabled. The
// relay is confined to this harness's project: the database is shared with every other test
// in the package.
func (h *harness) withNotifications(t *testing.T) *notify.Notifier {
	t.Helper()
	raw, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	svc := coord.New(h.store)
	n := notify.New(h.store, svc, notify.Options{
		SecretKey: &secretbox.Source{Env: raw},
		// The receivers in these tests listen on loopback over plain HTTP.
		Network:  notify.NetworkPolicy{AllowPrivate: true, AllowHTTP: true},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Projects: []domain.ID{h.project.ID},
	})
	h.server.Close()
	h.server = httptest.NewServer(New(h.store, svc, Options{Notify: n}).Handler())
	t.Cleanup(h.server.Close)
	t.Cleanup(func() {
		_, _ = h.store.Pool().Exec(context.Background(),
			`DELETE FROM notification_channels WHERE project_id = $1::uuid`, h.project.ID)
	})
	return n
}

// sink records request bodies.
type sink struct {
	mu     sync.Mutex
	server *httptest.Server
	bodies []string
}

func newSink(t *testing.T) *sink {
	s := &sink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		s.mu.Unlock()
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *sink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

// Only a maintainer manages channels: a contributor or an observer can neither add, list,
// test, nor remove one, and a non-member is told the project does not exist.
func TestNotificationChannelsNeedAMaintainer(t *testing.T) {
	h := newHarness(t)
	h.withNotifications(t)
	_, obsTok := h.member("olive", domain.RoleObserver)
	_, maintTok := h.member("mia", domain.RoleMaintainer)
	recv := newSink(t)
	create := map[string]any{"kind": "slack", "url": recv.server.URL + "/services/T/B/x"}

	for name, tok := range map[string]string{"contributor": h.bobTok, "observer": obsTok} {
		if code, body := h.do(tok, http.MethodPost, h.projectPath("/notifications"), create); code != http.StatusForbidden {
			t.Errorf("%s POST = %d\n%s", name, code, body)
		}
		if code, _ := h.do(tok, http.MethodGet, h.projectPath("/notifications"), nil); code != http.StatusForbidden {
			t.Errorf("%s GET = %d", name, code)
		}
	}
	if code, _ := h.do(h.outTok, http.MethodPost, h.projectPath("/notifications"), create); code != http.StatusNotFound {
		t.Errorf("outsider POST = %d, want 404", code)
	}

	code, body := h.do(maintTok, http.MethodPost, h.projectPath("/notifications"), create)
	if code != http.StatusCreated {
		t.Fatalf("maintainer POST = %d\n%s", code, body)
	}
	var created notify.Created
	_ = json.Unmarshal(body, &created)
	for name, tok := range map[string]string{"contributor": h.bobTok, "observer": obsTok} {
		if code, _ := h.do(tok, http.MethodPost, h.projectPath("/notifications/"+created.ID+"/test"), nil); code != http.StatusForbidden {
			t.Errorf("%s test = %d", name, code)
		}
		if code, _ := h.do(tok, http.MethodDelete, h.projectPath("/notifications/"+created.ID), nil); code != http.StatusForbidden {
			t.Errorf("%s DELETE = %d", name, code)
		}
	}
	if code, body := h.do(maintTok, http.MethodPost, h.projectPath("/notifications/"+created.ID+"/test"), nil); code != http.StatusOK ||
		!strings.Contains(string(body), `"ok":true`) || len(recv.all()) != 1 {
		t.Errorf("maintainer test = %d %s, %d received", code, body, len(recv.all()))
	}
	if code, _ := h.do(maintTok, http.MethodDelete, h.projectPath("/notifications/"+created.ID), nil); code != http.StatusNoContent {
		t.Errorf("maintainer DELETE = %d", code)
	}
	if code, _ := h.do(maintTok, http.MethodDelete, h.projectPath("/notifications/"+created.ID), nil); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", code)
	}
	if code, _ := h.do(maintTok, http.MethodDelete, h.projectPath("/notifications/not-a-uuid"), nil); code != http.StatusNotFound {
		t.Errorf("DELETE of a malformed id = %d, want 404", code)
	}
	if got := h.auditActions(created.ID); strings.Join(got, ",") != "notification.channel_added,notification.channel_removed" {
		t.Errorf("audit = %v", got)
	}
}

// The webhook URL and signing secret are credentials: the secret is returned once, by the
// request that creates the channel, and neither is ever returned again.
func TestNotificationCredentialsAreNotReturned(t *testing.T) {
	h := newHarness(t)
	h.withNotifications(t)
	const token = "pathTOKENzq81"
	code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/notifications"),
		map[string]any{"kind": "webhook", "url": "http://127.0.0.1:9/hooks/" + token, "events": []string{"github.pr_merged"}})
	if code != http.StatusCreated {
		t.Fatalf("create = %d\n%s", code, body)
	}
	var created notify.Created
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "whsec_") || !created.Signed {
		t.Fatalf("a webhook channel was created without its signing secret: %s", body)
	}
	if strings.Contains(string(body), token) {
		t.Errorf("the create response echoes the URL:\n%s", body)
	}

	_, list := h.do(h.aliceTok, http.MethodGet, h.projectPath("/notifications"), nil)
	if !strings.Contains(string(list), created.ID) || !strings.Contains(string(list), `"scope.released"`) {
		t.Fatalf("list does not show the channel and the catalog:\n%s", list)
	}
	for _, secret := range []string{token, created.Secret, strings.TrimPrefix(created.Secret, "whsec_"), "sealed"} {
		if strings.Contains(string(list), secret) {
			t.Errorf("list returns %q:\n%s", secret, list)
		}
	}

	// Bad input is the caller's error, not the server's.
	for _, bad := range []map[string]any{
		{"kind": "smtp", "url": "https://example.com/x"},
		{"kind": "webhook", "url": "ftp://example.com/x"},
		{"kind": "webhook", "url": "https://example.com/x", "events": []string{"conflict.detected"}},
	} {
		if code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/notifications"), bad); code != http.StatusBadRequest {
			t.Errorf("%v = %d\n%s", bad, code, body)
		}
	}
}

// Without the notifier configured, the routes say so rather than failing.
func TestNotificationsNotConfigured(t *testing.T) {
	h := newHarness(t)
	code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/notifications"), nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(string(body), "not_configured") {
		t.Errorf("GET without a notifier = %d\n%s", code, body)
	}
}

// PRIVACY: a private task's title, intent, summary, paths, and territory never reach a
// notification channel, whatever the event and whichever kind of channel. Every payload is
// rendered as an ordinary project member would see the event, and then narrowed further: the
// task is "a private task" and nothing more.
func TestNotificationsNeverCarryAPrivateTask(t *testing.T) {
	h := newHarness(t)
	n := h.withNotifications(t)
	ctx := context.Background()
	hook, slack := newSink(t), newSink(t)
	for kind, s := range map[string]*sink{"webhook": hook, "slack": slack} {
		if code, body := h.do(h.aliceTok, http.MethodPost, h.projectPath("/notifications"),
			map[string]any{"kind": kind, "url": s.server.URL, "events": []string{"*"}}); code != http.StatusCreated {
			t.Fatalf("create %s = %d\n%s", kind, code, body)
		}
	}

	started := h.startWork(h.aliceTok, map[string]any{
		"summary": "ZQXINTENT rework billing", "title": "ZQXTITLE billing rework", "visibility": "private",
		"acceptance_criteria": []map[string]any{{"text": "ZQXCRITERION holds"}},
		"scopes":              []map[string]any{{"resource": "dir:internal/billing", "mode": "write_exclusive"}},
	})
	var task domain.Task
	if code := h.jsonDo(h.aliceTok, http.MethodGet, "/v1/tasks/"+started.TaskID, nil, &task); code != http.StatusOK {
		t.Fatalf("get task = %d", code)
	}
	fence := map[string]any{"task_id": started.TaskID, "lease_id": started.LeaseID,
		"attempt_id": started.AttemptID, "fencing_epoch": started.FencingEpoch}
	if code, body := h.do(h.aliceTok, http.MethodPost, "/v1/attempts/"+started.AttemptID+"/progress",
		mergeMaps(fence, map[string]any{"phase": "implementing", "summary": "ZQXSUMMARY so far",
			"blocker": "ZQXBLOCKER waiting", "changed_paths": []string{"internal/billing/ZQXPATH.go"}})); code != http.StatusAccepted {
		t.Fatalf("progress = %d\n%s", code, body)
	}
	// Bob is refused the same territory, so the release below tells him it is free.
	if code, body := h.do(h.bobTok, http.MethodPost, h.projectPath("/intents/check"), map[string]any{
		"summary": "touch billing", "scopes": []map[string]any{{"resource": "dir:internal/billing/x", "mode": "write_exclusive"}},
	}); code != http.StatusOK || !strings.Contains(string(body), "block_conflict") {
		t.Fatalf("bob's check = %d\n%s", code, body)
	}
	if code, body := h.do(h.aliceTok, http.MethodPost, "/v1/tasks/"+started.TaskID+"/release",
		mergeMaps(fence, map[string]any{"reason": "ZQXREASON stepping away"})); code != http.StatusOK {
		t.Fatalf("release = %d\n%s", code, body)
	}
	// A system event whose writer labelled it team-visible and put the task's words in it:
	// the task's own visibility still governs.
	if err := h.store.AppendEvent(ctx, h.org.ID, h.project.ID, "", "attempt", started.AttemptID, "attempt.stalled",
		domain.VisibilityTeamSummary, map[string]any{"task_ref": task.Ref, "harness": "claude",
			"reason": "ZQXSUMMARY stalled", "changed_paths": []string{"internal/billing/ZQXPATH.go"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.UpdateTaskStatus(ctx, started.TaskID, domain.TaskCancelled); err != nil {
		t.Fatal(err)
	}

	for {
		r, err := n.Pass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Claimed == 0 {
			break
		}
	}

	bodies := append(hook.all(), slack.all()...)
	if len(hook.all()) < 4 || len(slack.all()) != len(hook.all()) {
		t.Fatalf("webhook got %d, slack %d notifications; the test needs the task's events to go out",
			len(hook.all()), len(slack.all()))
	}
	sawRelease := false
	for _, body := range bodies {
		for _, marker := range append(privateMarkers, "ZQXREASON", "internal/billing", task.Ref, started.TaskID) {
			if strings.Contains(body, marker) {
				t.Errorf("a notification carries %q:\n%s", marker, body)
			}
		}
		if !strings.Contains(body, "a private task") {
			t.Errorf("a notification about the private task does not say so:\n%s", body)
		}
		if strings.Contains(body, "scope.released") && strings.Contains(body, "@bob") {
			sawRelease = true
		}
	}
	if !sawRelease {
		t.Error("bob was not told the territory he waited for is free")
	}
}
