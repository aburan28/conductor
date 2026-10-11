package tracker

import (
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

func TestMapFieldsSplitsTheAcceptanceSection(t *testing.T) {
	body := "Retries should back off.\r\n\r\n<!-- template: describe the bug -->\r\n" +
		"## Acceptance criteria\n- [ ] retries use jitter\n* [x] max 5 attempts\n1. logged once\n\n" +
		"## Notes\nSee the runbook."
	f := MapFields("acme/w#1", "  Retry   storms\n", body)
	if f.Title != "Retry storms" {
		t.Errorf("title = %q", f.Title)
	}
	if strings.Contains(f.Objective, "template") || strings.Contains(f.Objective, "jitter") {
		t.Errorf("objective kept a comment or the acceptance section: %q", f.Objective)
	}
	if !strings.Contains(f.Objective, "Retries should back off.") || !strings.Contains(f.Objective, "See the runbook.") {
		t.Errorf("objective lost the body around the section: %q", f.Objective)
	}
	var texts []string
	for _, c := range f.Criteria {
		texts = append(texts, c.Text)
	}
	if strings.Join(texts, "|") != "retries use jitter|max 5 attempts|logged once" {
		t.Errorf("criteria = %q", texts)
	}

	// A section without a list is one criterion; no section is none; an empty title still
	// names the issue.
	if f := MapFields("acme/w#2", "", "# Acceptance\nIt works on Tuesdays."); len(f.Criteria) != 1 ||
		f.Criteria[0].Text != "It works on Tuesdays." || f.Title != "Issue acme/w#2" || f.Objective != "" {
		t.Errorf("prose section: %+v", f)
	}
	if f := MapFields("k", "t", "Just a body."); len(f.Criteria) != 0 {
		t.Errorf("criteria without a section: %+v", f.Criteria)
	}
}

func TestMapFieldsTruncatesSensibly(t *testing.T) {
	para := strings.Repeat("word ", 60) // 300 runes
	f := MapFields("k", strings.Repeat("t", 900), para+"\n\n"+para+"\n\n"+para)
	if n := len([]rune(f.Title)); n > maxTitle {
		t.Errorf("title is %d runes, want at most %d", n, maxTitle)
	}
	if n := len([]rune(f.Objective)); n > maxObjective {
		t.Errorf("objective is %d runes, want at most %d", n, maxObjective)
	}
	// Cut at the paragraph boundary, and marked as cut.
	if !strings.HasSuffix(f.Objective, "word …") || strings.Count(f.Objective, "\n\n") != 0 {
		t.Errorf("objective was not cut at a paragraph: %q", f.Objective[len(f.Objective)-20:])
	}
}

func TestRefRoundTrip(t *testing.T) {
	it := Item{Tracker: "github", Key: "acme/widgets#12"}
	tr, key, ok := ParseRef(it.Ref())
	if !ok || tr != "github" || key != "acme/widgets#12" || it.Ref() != "github:acme/widgets#12" {
		t.Errorf("ParseRef(%q) = %q %q %v", it.Ref(), tr, key, ok)
	}
	for _, bad := range []string{"", "nope", ":x", "github:", "a/b:c"} {
		if _, _, ok := ParseRef(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestQualifiesAndVisibility(t *testing.T) {
	cfg := db.TrackerConfig{Label: "conductor", PublicVisibility: domain.VisibilityTeamArtifacts}
	if !Qualifies(cfg, []string{"bug", "Conductor"}) || Qualifies(cfg, []string{"bug"}) {
		t.Error("label filter")
	}
	if !Qualifies(db.TrackerConfig{}, nil) {
		t.Error("no label configured should import everything")
	}
	proj := domain.Project{Config: domain.ProjectConfig{DefaultVisibility: domain.VisibilityPrivate}}
	if v := ImportVisibility(proj, cfg, false); v != domain.VisibilityPrivate {
		t.Errorf("private repository: %s, want the project default", v)
	}
	if v := ImportVisibility(proj, cfg, true); v != domain.VisibilityTeamArtifacts {
		t.Errorf("public repository: %s, want team_artifacts", v)
	}
	cfg.PublicVisibility = domain.VisibilityTeamSummary
	if v := ImportVisibility(proj, cfg, true); v != domain.VisibilityTeamSummary {
		t.Errorf("configured public visibility: %s", v)
	}
}

// The last-writer-wins rule, one field group at a time.
func TestPlanEditRule(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	old := MapFields("k", "Old title", "Old body")
	linked := func(taskTitle string, editedAt *time.Time) db.TrackerItem {
		return db.TrackerItem{Linked: true,
			Link: db.TrackerLink{SyncedTitleHash: old.TitleHash(), SyncedBodyHash: old.BodyHash(), RemoteUpdatedAt: &t0,
				TaskEditedAt: editedAt, RemoteState: db.TrackerRemoteOpen},
			Task: domain.Task{Title: taskTitle, Objective: old.Objective, Status: domain.TaskReady}}
	}
	plan := func(cur db.TrackerItem, title string, at time.Time) db.TrackerChange {
		it := Item{Tracker: "github", Key: "k", Title: title, Body: "Old body", Open: true, UpdatedAt: at}
		f := MapFields(it.Key, it.Title, it.Body)
		return Plan(db.TrackerConfig{}, domain.Project{}, it, f,
			&db.TrackerRemote{TitleHash: f.TitleHash(), BodyHash: f.BodyHash()}, cur)
	}
	later, earlier := t0.Add(2*time.Hour), t0.Add(time.Hour)

	if ch := plan(linked("Old title", nil), "New title", later); ch.Title == nil || *ch.Title != "New title" || ch.Objective != nil {
		t.Errorf("an issue edit to an untouched task: %+v", ch)
	}
	// Edited in Conductor after the issue changed: Conductor's edit stays.
	if ch := plan(linked("Mine", &later), "New title", earlier); ch.Title != nil {
		t.Errorf("an older issue edit overwrote a newer Conductor edit: %q", *ch.Title)
	}
	// Edited in Conductor before the issue changed: the issue's edit wins.
	if ch := plan(linked("Mine", &earlier), "New title", later); ch.Title == nil {
		t.Error("a newer issue edit lost to an older Conductor edit")
	}
	// The issue did not change its title (a label, say): a Conductor edit is never touched.
	if ch := plan(linked("Mine", &earlier), "Old title", later); ch.Title != nil {
		t.Error("an unchanged issue title overwrote a Conductor edit")
	}
	// A reading older than one already applied changes nothing at all.
	if ch := plan(linked("Old title", nil), "Stale", t0.Add(-time.Minute)); ch.Remote != nil || ch.Title != nil {
		t.Errorf("a stale reading was applied: %+v", ch)
	}
}

func TestStateRule(t *testing.T) {
	cases := []struct {
		name   string
		open   bool
		task   domain.Task
		bySync bool
		want   db.TrackerStatusChange
	}{
		{"closed, ready", false, domain.Task{Status: domain.TaskReady}, false, db.TrackerCancel},
		{"closed, running", false, domain.Task{Status: domain.TaskRunning}, false, db.TrackerCancel},
		{"closed, done", false, domain.Task{Status: domain.TaskDone}, false, db.TrackerKeepStatus},
		{"closed, landing", false, domain.Task{Status: domain.TaskVerifying}, false, db.TrackerKeepStatus},
		{"closed, open pull request", false, domain.Task{Status: domain.TaskRunning, PullRequestState: "open"}, false, db.TrackerKeepStatus},
		{"reopened, cancelled by sync", true, domain.Task{Status: domain.TaskCancelled}, true, db.TrackerRevive},
		{"reopened, cancelled by a person", true, domain.Task{Status: domain.TaskCancelled}, false, db.TrackerKeepStatus},
		{"reopened, done", true, domain.Task{Status: domain.TaskDone}, true, db.TrackerKeepStatus},
	}
	for _, c := range cases {
		if got, _ := StateRule("github", c.open, c.task, db.TrackerLink{CancelledBySync: c.bySync}); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// PlanWriteBack's privacy rules: never content, nothing for a private task on a public item,
// and a handle only where the item's readers may see it.
func TestPlanWriteBackPrivacy(t *testing.T) {
	base := func(vis domain.Visibility, public bool) db.TrackedTask {
		return db.TrackedTask{Link: db.TrackerLink{ExternalRef: "github:acme/w#1", RemoteState: db.TrackerRemoteOpen, RemotePublic: public},
			Status: domain.TaskClaimed, Visibility: vis, HolderID: "p1", HolderHandle: "alice", InProgressLabel: "in-progress"}
	}
	var r shareRemote
	if p := PlanWriteBack(base(domain.VisibilityTeamSummary, false), r); p.Claim != "Claimed by `alice` via Conductor." || p.AddLabel != "in-progress" {
		t.Errorf("private repository, team task: %+v", p)
	}
	if p := PlanWriteBack(base(domain.VisibilityTeamSummary, true), r); p.Claim != "Claimed via Conductor." {
		t.Errorf("public repository, team_summary task named its claimant: %+v", p)
	}
	if p := PlanWriteBack(base(domain.VisibilityTeamArtifacts, true), r); !strings.Contains(p.Claim, "alice") {
		t.Errorf("public repository, team_artifacts task: %+v", p)
	}
	if p := PlanWriteBack(base(domain.VisibilityPrivate, false), r); p.Claim != "Claimed via Conductor." {
		t.Errorf("private task on a private repository named its claimant: %+v", p)
	}
	if p := PlanWriteBack(base(domain.VisibilityPrivate, true), r); !p.Empty() {
		t.Errorf("private task on a public repository was written back: %+v", p)
	}
	// One claim comment per claimant.
	noted := base(domain.VisibilityTeamSummary, false)
	noted.Link.ClaimNotedFor, noted.Link.LabelApplied = "p1", "in-progress"
	if p := PlanWriteBack(noted, r); !p.Empty() {
		t.Errorf("an already-announced claim: %+v", p)
	}

	done := base(domain.VisibilityTeamSummary, true)
	done.Status, done.HolderID, done.Link.LabelApplied = domain.TaskDone, "", "in-progress"
	done.PullRequestURL = "https://github.com/acme/w/pull/9"
	if p := PlanWriteBack(done, r); p.Done != "Done via Conductor in https://github.com/acme/w/pull/9." || !p.Close || p.RemoveLabel != "in-progress" {
		t.Errorf("done: %+v", p)
	}
	done.PullRequestURL = "https://github.com/acme/secret/pull/3"
	if p := PlanWriteBack(done, r); p.Done != "Done via Conductor." {
		t.Errorf("a pull request in another repository was linked: %+v", p)
	}
}

// shareRemote is a Remote that only answers Shareable, which is all planning asks.
type shareRemote struct{ Remote }

func (shareRemote) Shareable(_, u string) bool { return strings.Contains(u, "/acme/w/") }
