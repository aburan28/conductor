package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/quota"
)

func quotaSnap(account, machine, window string, pct float64, resets time.Time, observed time.Time) quota.Snapshot {
	return quota.Snapshot{
		Harness: "claude", Account: account, Machine: machine, Window: window, WindowMinutes: 300,
		UsedPercent: quota.Float(pct), ResetsAt: quota.Time(resets), Plan: "max",
		Source: "claude-statusline", SourceKind: quota.KindDocumented, ObservedAt: observed,
	}
}

func quotaEvents(t *testing.T, h *harness) []domain.Event {
	t.Helper()
	code, body := h.do(h.bobTok, http.MethodGet, h.projectPath("/events"), nil)
	if code != http.StatusOK {
		t.Fatalf("events = %d %s", code, body)
	}
	var out struct {
		Events []domain.Event `json:"events"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	var q []domain.Event
	for _, e := range out.Events {
		if strings.HasPrefix(e.Type, "quota.") {
			q = append(q, e)
		}
	}
	return q
}

// TestQuotaIsVisibleOnlyToItsOwner: the owner reads every detail back; a teammate reads
// counts and never a login, a machine, or a window of someone else's; an outsider reads
// nothing.
func TestQuotaIsVisibleOnlyToItsOwner(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	resets := now.Add(2 * time.Hour)

	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/quota", map[string]any{
		"project": h.project.Slug,
		"snapshots": []quota.Snapshot{
			quotaSnap("work-login", "alice-laptop", "5h", 96, resets, now),
			quotaSnap("default", "alice-laptop", "5h", 10, resets, now),
		},
	})
	if code != http.StatusOK {
		t.Fatalf("record = %d %s", code, body)
	}
	var rec struct {
		Recorded int           `json:"recorded"`
		Alerts   []quota.Alert `json:"alerts"`
	}
	if err := json.Unmarshal(body, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Recorded != 2 || len(rec.Alerts) != 1 || rec.Alerts[0].Level != quota.LevelCritical {
		t.Fatalf("record result = %+v", rec)
	}

	// The owner sees both logins in full.
	code, body = h.do(h.aliceTok, http.MethodGet, "/v1/quota", nil)
	if code != http.StatusOK {
		t.Fatalf("own quota = %d", code)
	}
	var mine quota.View
	if err := json.Unmarshal(body, &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Snapshots) != 2 || len(mine.Logins) != 2 {
		t.Fatalf("owner view = %+v", mine)
	}
	for _, row := range mine.Snapshots {
		if row.Account == "work-login" && (row.Level != quota.LevelCritical || row.Machine != "alice-laptop") {
			t.Errorf("owner row = %+v", row)
		}
	}

	// A teammate's own view is empty, and the project view is counts.
	code, body = h.do(h.bobTok, http.MethodGet, "/v1/quota", nil)
	if code != http.StatusOK || strings.Contains(string(body), "work-login") {
		t.Errorf("bob's own quota leaked alice's: %d %s", code, body)
	}
	code, body = h.do(h.bobTok, http.MethodGet, h.projectPath("/quota"), nil)
	if code != http.StatusOK {
		t.Fatalf("team quota = %d %s", code, body)
	}
	var team quota.TeamView
	if err := json.Unmarshal(body, &team); err != nil {
		t.Fatal(err)
	}
	if team.Logins != 2 || team.NearLimit != 1 || team.Exhausted != 0 || len(team.Mine.Snapshots) != 0 {
		t.Errorf("team view = %+v", team)
	}
	for _, leaked := range []string{"work-login", "alice-laptop", "alice", h.alice.ID, `"5h"`} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("team view leaks %q: %s", leaked, body)
		}
	}

	// Someone outside the project gets nothing, and cannot aim events at it.
	if code, _ := h.do(h.outTok, http.MethodGet, h.projectPath("/quota"), nil); code != http.StatusNotFound {
		t.Errorf("outsider team view = %d, want 404", code)
	}
	code, _ = h.do(h.outTok, http.MethodPost, "/v1/quota", map[string]any{
		"project": h.project.Slug, "snapshots": []quota.Snapshot{quotaSnap("x", "m", "5h", 99, resets, now)},
	})
	if code != http.StatusNotFound {
		t.Errorf("outsider posting into the project = %d, want 404", code)
	}

	// The event the crossing raised names no one.
	events := quotaEvents(t, h)
	if len(events) != 1 || events[0].Type != "quota.warning" {
		t.Fatalf("quota events = %+v", events)
	}
	e := events[0]
	if e.ActorPrincipal != "" || e.Payload["severity"] != "critical" || e.Payload["harness"] != "claude" ||
		e.Payload["kind"] != "5h" {
		t.Errorf("event = %+v", e)
	}
	raw, _ := json.Marshal(e)
	for _, leaked := range []string{"work-login", "alice-laptop", h.alice.ID} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("event leaks %q: %s", leaked, raw)
		}
	}
}

// TestQuotaEventsAreRaisedOncePerWindowPerLevel: repeated reports of the same window do not
// flood the stream; a higher level raises once more; the next window re-arms.
func TestQuotaEventsAreRaisedOncePerWindowPerLevel(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	resets := now.Add(3 * time.Hour)
	post := func(pct float64, resets, observed time.Time) []quota.Alert {
		t.Helper()
		code, body := h.do(h.aliceTok, http.MethodPost, "/v1/quota", map[string]any{
			"project":    h.project.ID,
			"thresholds": map[string]any{"warn_percent": 80, "critical_percent": 95},
			"snapshots":  []quota.Snapshot{quotaSnap("default", "m1", "5h", pct, resets, observed)},
		})
		if code != http.StatusOK {
			t.Fatalf("record = %d %s", code, body)
		}
		var rec struct {
			Alerts []quota.Alert `json:"alerts"`
		}
		_ = json.Unmarshal(body, &rec)
		return rec.Alerts
	}

	if a := post(50, resets, now.Add(-10*time.Minute)); len(a) != 0 {
		t.Errorf("below threshold raised %+v", a)
	}
	if a := post(82, resets, now.Add(-9*time.Minute)); len(a) != 1 || a[0].Level != quota.LevelWarning {
		t.Errorf("crossing warn raised %+v", a)
	}
	for i := 0; i < 3; i++ {
		if a := post(85+float64(i), resets, now.Add(time.Duration(i-8)*time.Minute)); len(a) != 0 {
			t.Errorf("repeat %d raised %+v", i, a)
		}
	}
	// A late report of an older reading neither moves the window back nor raises anything.
	if a := post(99, resets, now.Add(-time.Hour)); len(a) != 0 {
		t.Errorf("stale reading raised %+v", a)
	}
	if a := post(100, resets, now.Add(-time.Minute)); len(a) != 1 || a[0].Level != quota.LevelExhausted {
		t.Errorf("exhaustion raised %+v", a)
	}
	if a := post(100, resets, now); len(a) != 0 {
		t.Errorf("exhaustion again raised %+v", a)
	}
	// The next window (a reset five hours on) re-arms the levels.
	if a := post(81, resets.Add(5*time.Hour), now); len(a) != 1 || a[0].Level != quota.LevelWarning {
		t.Errorf("next window raised %+v", a)
	}

	var kinds []string
	for _, e := range quotaEvents(t, h) {
		kinds = append(kinds, e.Type+"/"+e.Payload["severity"].(string))
	}
	if len(kinds) != 3 {
		t.Errorf("events = %v, want warning, exhausted, warning", kinds)
	}

	// The stored reading is the newest one, not the late one.
	_, body := h.do(h.aliceTok, http.MethodGet, "/v1/quota", nil)
	var view quota.View
	_ = json.Unmarshal(body, &view)
	if len(view.Snapshots) != 1 || *view.Snapshots[0].UsedPercent != 81 {
		t.Errorf("stored = %+v", view.Snapshots)
	}
}

func TestQuotaRejectsWhatIsNotALabel(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	for _, bad := range []quota.Snapshot{
		quotaSnap("alice@example.com", "m", "5h", 10, now.Add(time.Hour), now),
		quotaSnap("default", "/home/alice/.claude", "5h", 10, now.Add(time.Hour), now),
		quotaSnap("default", "m", "five hour window", 10, now.Add(time.Hour), now),
		{Harness: "claude", Account: "default", Window: "5h", Source: "x", SourceKind: "scraped"},
	} {
		code, _ := h.do(h.aliceTok, http.MethodPost, "/v1/quota", map[string]any{"snapshots": []quota.Snapshot{bad}})
		if code != http.StatusBadRequest {
			t.Errorf("%+v accepted with %d", bad, code)
		}
	}
	// Without a project the readings are recorded and nothing is announced anywhere.
	code, body := h.do(h.aliceTok, http.MethodPost, "/v1/quota", map[string]any{
		"snapshots": []quota.Snapshot{quotaSnap("default", "m", "5h", 99, now.Add(time.Hour), now)},
	})
	if code != http.StatusOK || !strings.Contains(string(body), `"recorded":1`) {
		t.Errorf("projectless report = %d %s", code, body)
	}
	if ev := quotaEvents(t, h); len(ev) != 0 {
		t.Errorf("projectless report emitted %+v", ev)
	}
}

func TestQuotaRemoteIdentityHintIsOwnerOnlyAndSwitchesAccounts(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	oldKey, newKey := "default-id-111111111111", "default-id-222222222222"
	post := func(account, email, source string, observed time.Time) (int, []byte) {
		t.Helper()
		snap := quotaSnap(account, "alice-laptop", "weekly", 81, now.Add(7*24*time.Hour), observed)
		snap.IdentityEmail, snap.IdentitySource = email, source
		return h.do(h.aliceTok, http.MethodPost, "/v1/quota", map[string]any{
			"project": h.project.Slug, "snapshots": []quota.Snapshot{snap},
		})
	}
	oldEmail, newEmail := "old@example.test", "new@example.test"
	if code, body := post(oldKey, oldEmail, "unverified_local_metadata", now.Add(-3*time.Minute)); code != http.StatusOK {
		t.Fatalf("remote report = %d %s", code, body)
	}
	readMine := func() quota.View {
		t.Helper()
		code, body := h.do(h.aliceTok, http.MethodGet, "/v1/quota", nil)
		if code != http.StatusOK {
			t.Fatalf("owner view = %d %s", code, body)
		}
		var view quota.View
		if err := json.Unmarshal(body, &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	if view := readMine(); len(view.Snapshots) != 1 || view.Snapshots[0].IdentityEmail != oldEmail ||
		view.Snapshots[0].IdentitySource != "unverified_local_metadata" {
		t.Fatalf("owner hint after remote report = %+v", view.Snapshots)
	}
	if code, body := h.do(h.aliceTok, http.MethodGet, h.projectPath("/quota"), nil); code != http.StatusOK {
		t.Fatalf("owner project quota = %d %s", code, body)
	} else {
		var team quota.TeamView
		if err := json.Unmarshal(body, &team); err != nil || len(team.Mine.Snapshots) != 1 ||
			team.Mine.Snapshots[0].IdentityEmail != oldEmail {
			t.Fatalf("dashboard owner hint = %+v, %v", team.Mine.Snapshots, err)
		}
	}
	var storedEmail, rawSnapshot string
	if err := h.store.Pool().QueryRow(t.Context(), `SELECT email FROM quota_identity_hints
		WHERE principal_id = $1::uuid AND machine = 'alice-laptop' AND account = $2`,
		h.alice.ID, oldKey).Scan(&storedEmail); err != nil || storedEmail != oldEmail {
		t.Fatalf("owner-scoped persisted hint = %q, %v", storedEmail, err)
	}
	if err := h.store.Pool().QueryRow(t.Context(), `SELECT row_to_json(q)::text FROM quota_snapshots q
		WHERE principal_id = $1::uuid AND account = $2`, h.alice.ID, oldKey).Scan(&rawSnapshot); err != nil ||
		strings.Contains(rawSnapshot, oldEmail) {
		t.Fatalf("email reached stored snapshot: %v", err)
	}
	var otherHints int
	if err := h.store.Pool().QueryRow(t.Context(), `SELECT count(*) FROM quota_identity_hints
		WHERE principal_id = $1::uuid`, h.bob.ID).Scan(&otherHints); err != nil || otherHints != 0 {
		t.Fatalf("hint crossed principal boundary: %d, %v", otherHints, err)
	}
	for _, path := range []string{"/v1/quota", h.projectPath("/quota")} {
		code, body := h.do(h.bobTok, http.MethodGet, path, nil)
		if code != http.StatusOK || strings.Contains(string(body), oldEmail) || strings.Contains(string(body), oldKey) {
			t.Fatalf("other principal view leaked identity on %s: %d %s", path, code, body)
		}
	}
	for _, e := range quotaEvents(t, h) {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), oldEmail) {
			t.Fatalf("project event leaked identity: %s", raw)
		}
	}
	if code, body := post(newKey, newEmail, "unverified_local_metadata", now.Add(-time.Minute)); code != http.StatusOK {
		t.Fatalf("switched account report = %d %s", code, body)
	}
	// A delayed report from the previous account cannot reactivate its hint.
	if code, body := post(oldKey, oldEmail, "unverified_local_metadata", now.Add(-3*time.Minute)); code != http.StatusOK {
		t.Fatalf("late account report = %d %s", code, body)
	}
	view := readMine()
	if len(view.Snapshots) != 2 {
		t.Fatalf("account switch rows = %+v", view.Snapshots)
	}
	for _, row := range view.Snapshots {
		switch row.Account {
		case oldKey:
			if row.IdentityEmail != "" || row.IdentitySource != "" {
				t.Errorf("old account kept stale hint: %+v", row)
			}
		case newKey:
			if row.IdentityEmail != newEmail || row.IdentitySource != "unverified_local_metadata" {
				t.Errorf("new account hint = %+v", row)
			}
		default:
			t.Errorf("unexpected account: %+v", row)
		}
	}
	for _, e := range quotaEvents(t, h) {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), oldEmail) || strings.Contains(string(raw), newEmail) {
			t.Fatalf("switched identity reached project event: %s", raw)
		}
	}
	for _, bad := range []struct{ email, source string }{
		{"bad-email", "unverified_local_metadata"},
		{"owner@example.test", "verified_provider"},
	} {
		code, body := post(newKey, bad.email, bad.source, now)
		if code != http.StatusBadRequest || strings.Contains(string(body), bad.email) {
			t.Errorf("invalid hint accepted or echoed: %d %s", code, body)
		}
	}
}
