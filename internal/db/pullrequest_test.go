package db

import (
	"strconv"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/domain"
)

// Pull requests as first-class objects (migration 0020; docs/COLLABORATION.md §4.5).

func (f *fixture) seenPull(t *testing.T, repo string, number int) PullRequest {
	t.Helper()
	pr, _, err := f.store.UpsertPullRequest(f.ctx, PullRequestUpsert{ProjectID: f.project.ID, Repository: repo, Number: number,
		URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(number), State: PullRequestOpen, Title: "pull " + strconv.Itoa(number),
		HeadRef: "feature/" + strconv.Itoa(number), HeadSHA: "sha" + strconv.Itoa(number), HeadRepo: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("UpsertPullRequest: %v", err)
	}
	return pr
}

func (f *fixture) taskPull(t *testing.T, id domain.ID) domain.Task {
	t.Helper()
	task, err := f.store.GetTask(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// A stronger source replaces a weaker one and a weaker never replaces a stronger; a person's
// link (and a person's unlink) outranks all the rest.
func TestPullRequestLinkPrecedence(t *testing.T) {
	f := newFixture(t)
	a := f.newTask(t, "Branch task")
	b := f.newTask(t, "Trailer task")
	c := f.newTask(t, "Manual task")
	f.seenPull(t, "acme/w", 1)

	link := func(task domain.Task, source string) PullRequestLink {
		t.Helper()
		out, err := f.store.LinkPullRequestTask(f.ctx, f.project.ID, "acme/w", 1, task.ID, source)
		if err != nil {
			t.Fatalf("link %s: %v", source, err)
		}
		return out
	}
	owner := func() (domain.ID, string) {
		t.Helper()
		pr, err := f.store.GetPullRequest(f.ctx, f.project.ID, "acme/w", 1)
		if err != nil {
			t.Fatal(err)
		}
		return pr.TaskID, pr.LinkSource
	}

	if out := link(a, LinkBranch); !out.Linked {
		t.Fatalf("branch link = %+v", out)
	}
	// A body reference outranks the branch; so does a trailer.
	if out := link(b, LinkBodyRef); !out.Linked || out.Previous != a.ID {
		t.Fatalf("body_ref over branch = %+v", out)
	}
	if id, src := owner(); id != b.ID || src != LinkBodyRef {
		t.Fatalf("owner = %s %s, want %s body_ref", id, src, b.ID)
	}
	// A weaker source does not take it back.
	if out := link(a, LinkBranch); out.Linked {
		t.Fatalf("branch replaced body_ref: %+v", out)
	}
	if id, _ := owner(); id != b.ID {
		t.Fatalf("a weaker source moved the link to %s", id)
	}
	// Equal automatic sources do not flap between tasks.
	if out := link(a, LinkBodyRef); out.Linked {
		t.Fatalf("body_ref replaced body_ref: %+v", out)
	}
	if out := link(a, LinkTrailer); !out.Linked {
		t.Fatalf("trailer over body_ref = %+v", out)
	}
	// Manual beats everything, and survives an automatic source naming another task.
	if out := link(c, LinkManual); !out.Linked || out.Previous != a.ID {
		t.Fatalf("manual = %+v", out)
	}
	if out := link(a, LinkTrailer); out.Linked {
		t.Fatalf("trailer replaced manual: %+v", out)
	}
	if id, src := owner(); id != c.ID || src != LinkManual {
		t.Fatalf("owner = %s %s, want %s manual", id, src, c.ID)
	}
	// The mirror followed the moves: the first two tasks no longer show the pull request.
	for _, task := range []domain.Task{a, b} {
		if got := f.taskPull(t, task.ID); got.PullRequestURL != "" || got.PullRequestState != "" {
			t.Errorf("%s still mirrors a pull request it lost: %+v", got.Ref, got)
		}
	}
	if got := f.taskPull(t, c.ID); got.PullRequestURL != "https://github.com/acme/w/pull/1" || got.PullRequestState != PullRequestOpen {
		t.Errorf("%s does not mirror its pull request: url %q state %q", got.Ref, got.PullRequestURL, got.PullRequestState)
	}

	// Unlinking by hand sticks: no automatic source links it again.
	if done, err := f.store.UnlinkPullRequestTask(f.ctx, f.project.ID, "acme/w", 1, c.ID); err != nil || !done {
		t.Fatalf("unlink = %v, %v", done, err)
	}
	if out := link(a, LinkBranch); out.Linked {
		t.Fatalf("a branch linked a pull request a person unlinked: %+v", out)
	}
	if out := link(a, LinkManual); !out.Linked {
		t.Fatalf("a person could not link it again: %+v", out)
	}
	if done, err := f.store.UnlinkPullRequestTask(f.ctx, f.project.ID, "acme/w", 1, b.ID); err != nil || done {
		t.Errorf("unlink from a task it is not linked to = %v, %v", done, err)
	}
}

// A task with several pull requests is done when its primary merges or its last open one
// does; a close frees a waiting task only when none is still open.
func TestMultiplePullRequestsCompleteTheTaskByTheLastOne(t *testing.T) {
	f := newFixture(t)
	verifying := func(title string) domain.Task {
		t.Helper()
		task := f.newTask(t, title)
		claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice, path("internal/pr/"+title+".go")))
		if err != nil {
			t.Fatal(err)
		}
		f.finish(t, claim)
		return task
	}
	linkAll := func(task domain.Task, numbers ...int) {
		t.Helper()
		for _, n := range numbers {
			f.seenPull(t, "acme/w", n)
			if _, err := f.store.LinkPullRequestTask(f.ctx, f.project.ID, "acme/w", n, task.ID, LinkBranch); err != nil {
				t.Fatal(err)
			}
		}
	}
	end := func(n int, merged bool) PullRequestOutcome {
		t.Helper()
		out, err := f.store.EndPullRequest(f.ctx, f.project.ID, "acme/w", n, merged)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Non-primary merges first: the task waits for the rest; the last one completes it.
	a := verifying("a")
	linkAll(a, 10, 11, 12)
	if got := f.taskPull(t, a.ID); got.PullRequestURL != "https://github.com/acme/w/pull/10" {
		t.Fatalf("the first linked pull request is not primary: %q", got.PullRequestURL)
	}
	if out := end(11, true); !out.Recorded || out.Changed || out.OthersOpen != 2 {
		t.Fatalf("merge of a non-primary with others open = %+v", out)
	}
	if out := end(12, true); out.Changed || out.OthersOpen != 1 {
		t.Fatalf("second merge = %+v", out)
	}
	if got := f.taskPull(t, a.ID); got.Status != domain.TaskVerifying {
		t.Fatalf("the task moved to %s with a pull request still open", got.Status)
	}
	out := end(10, true)
	if !out.Changed || out.Status != domain.TaskDone || out.OthersOpen != 0 {
		t.Fatalf("last merge = %+v", out)
	}
	if got := f.taskPull(t, a.ID); got.PullRequestState != PullRequestMerged || got.PullRequestURL != "https://github.com/acme/w/pull/10" {
		t.Fatalf("mirror after merge = %q %q", got.PullRequestURL, got.PullRequestState)
	}

	// The primary merging completes the task at once, whatever else is open.
	b := verifying("b")
	linkAll(b, 20, 21)
	if out := end(20, true); !out.Changed || out.Status != domain.TaskDone || out.OthersOpen != 1 {
		t.Fatalf("primary merge with another open = %+v", out)
	}

	// A close with another open pull request leaves the task waiting, and the mirror moves
	// to the one that can still merge; closing the last frees the task.
	c := verifying("c")
	linkAll(c, 30, 31)
	if out := end(30, false); !out.Recorded || out.Changed || out.OthersOpen != 1 {
		t.Fatalf("close with another open = %+v", out)
	}
	if got := f.taskPull(t, c.ID); got.Status != domain.TaskVerifying || got.PullRequestURL != "https://github.com/acme/w/pull/31" || got.PullRequestState != PullRequestOpen {
		t.Fatalf("after closing the primary: %s, mirror %q %q", got.Status, got.PullRequestURL, got.PullRequestState)
	}
	if out := end(31, false); !out.Changed || out.Status != domain.TaskReady {
		t.Fatalf("close of the last = %+v", out)
	}

	// Seeing an end twice records it once, and a merge never moves back to closed.
	if out := end(31, false); out.Recorded || out.Changed {
		t.Errorf("second close = %+v", out)
	}
	if out := end(10, false); out.Recorded {
		t.Errorf("a merged pull request was recorded as closed: %+v", out)
	}
}

// A pull request first seen after it ended — opened and merged between two polls — still
// completes the task its branch names, once.
func TestPullRequestFirstSeenMergedCompletesItsTask(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Opened and merged between polls")
	claim, err := f.store.Claim(f.ctx, f.claimParams(task, f.alice, path("internal/pr/fast.go")))
	if err != nil {
		t.Fatal(err)
	}
	f.finish(t, claim)
	f.seenPull(t, "acme/w", 40)
	if _, err := f.store.LinkPullRequestTask(f.ctx, f.project.ID, "acme/w", 40, task.ID, LinkBranch); err != nil {
		t.Fatal(err)
	}
	out, err := f.store.EndPullRequest(f.ctx, f.project.ID, "acme/w", 40, true)
	if err != nil || !out.Recorded || out.Status != domain.TaskDone {
		t.Fatalf("merge = %+v, %v", out, err)
	}
	// An unlinked pull request ends without touching anyone.
	f.seenPull(t, "acme/w", 41)
	if out, err := f.store.EndPullRequest(f.ctx, f.project.ID, "acme/w", 41, true); err != nil || !out.Recorded || out.Changed || out.TaskRef != "" {
		t.Fatalf("unlinked merge = %+v, %v", out, err)
	}
}

// The list filters answer the questions a reviewer asks.
func TestListPullRequestsFilters(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "Linked")
	f.seenPull(t, "acme/w", 50) // linked, passing, approved
	f.seenPull(t, "acme/w", 51) // unlinked, failing
	f.seenPull(t, "acme/w", 52) // draft
	f.seenPull(t, "acme/w", 53) // merged
	if _, err := f.store.LinkPullRequestTask(f.ctx, f.project.ID, "acme/w", 50, task.ID, LinkManual); err != nil {
		t.Fatal(err)
	}
	set := func(n int, r PullRequestRollup) {
		t.Helper()
		if _, _, err := f.store.SetPullRequestRollup(f.ctx, f.project.ID, "acme/w", n, r); err != nil {
			t.Fatal(err)
		}
	}
	set(50, PullRequestRollup{ChecksTotal: 2, ChecksPassed: 2, ReviewDecision: "approved", Mergeable: "clean", Additions: 10, Deletions: 2, ChangedFiles: 3})
	set(51, PullRequestRollup{ChecksTotal: 3, ChecksPassed: 1, ChecksFailed: 2, ReviewDecision: "review_required"})
	if _, _, err := f.store.UpsertPullRequest(f.ctx, PullRequestUpsert{ProjectID: f.project.ID, Repository: "acme/w", Number: 52,
		URL: "https://github.com/acme/w/pull/52", Draft: true, Title: "wip", State: PullRequestOpen}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EndPullRequest(f.ctx, f.project.ID, "acme/w", 53, true); err != nil {
		t.Fatal(err)
	}

	numbers := func(filter PullRequestFilter) []int {
		t.Helper()
		filter.Repository = "acme/w"
		rows, err := f.store.ListPullRequests(f.ctx, f.project.ID, filter)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, r := range rows {
			out = append(out, r.Number)
		}
		return out
	}
	same := func(name string, got []int, want ...int) {
		t.Helper()
		m := map[int]bool{}
		for _, n := range got {
			m[n] = true
		}
		if len(got) != len(want) {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
		for _, n := range want {
			if !m[n] {
				t.Errorf("%s = %v, want %v", name, got, want)
				return
			}
		}
	}
	same("open", numbers(PullRequestFilter{State: PullRequestOpen}), 50, 51, 52)
	same("merged", numbers(PullRequestFilter{State: PullRequestMerged}), 53)
	same("all", numbers(PullRequestFilter{}), 50, 51, 52, 53)
	same("unlinked", numbers(PullRequestFilter{Unlinked: true, State: PullRequestOpen}), 51, 52)
	same("task", numbers(PullRequestFilter{TaskID: task.ID}), 50)
	same("failing", numbers(PullRequestFilter{Failing: true}), 51)
	// Needs review: open, not a draft, and not already approved.
	same("needs review", numbers(PullRequestFilter{NeedsReview: true}), 51)
	same("mine (task owner)", numbers(PullRequestFilter{Mine: f.alice.ID}), 50)
	same("mine (someone else)", numbers(PullRequestFilter{Mine: f.bob.ID}))
	same("number", numbers(PullRequestFilter{Number: 51}), 51)

	pr, err := f.store.GetPullRequest(f.ctx, f.project.ID, "ACME/W", 50)
	if err != nil {
		t.Fatalf("the repository is looked up without regard to case: %v", err)
	}
	if pr.TaskRef != task.Ref || !pr.Primary || pr.ChangedFiles != 3 || pr.Additions != 10 || pr.Mergeable != "clean" {
		t.Errorf("pull request 50 = %+v", pr)
	}
}

// The title is bounded: a pull request cannot make this table a place for long text.
func TestPullRequestTitleIsBounded(t *testing.T) {
	f := newFixture(t)
	long := make([]rune, 5000)
	for i := range long {
		long[i] = 'x'
	}
	pr, _, err := f.store.UpsertPullRequest(f.ctx, PullRequestUpsert{ProjectID: f.project.ID, Repository: "acme/w", Number: 60,
		URL: "https://github.com/acme/w/pull/60", State: PullRequestOpen, Title: string(long)})
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(pr.Title)); n > 300 {
		t.Errorf("stored title is %d characters", n)
	}
}

// What a webhook or poll learns when a pull request changes: a moved head, a pull request
// leaving draft, and a reopen — and a redelivery reports none of them twice.
func TestUpsertPullRequestReportsWhatChanged(t *testing.T) {
	f := newFixture(t)
	up := func(sha string, draft bool, state string) (PullRequest, PullRequestChange) {
		t.Helper()
		pr, ch, err := f.store.UpsertPullRequest(f.ctx, PullRequestUpsert{ProjectID: f.project.ID, Repository: "acme/w", Number: 70,
			URL: "https://github.com/acme/w/pull/70", State: state, Draft: draft, HeadSHA: sha, HeadRef: "feature/x"})
		if err != nil {
			t.Fatal(err)
		}
		return pr, ch
	}
	if _, ch := up("a1", true, PullRequestOpen); !ch.Created {
		t.Errorf("first sighting = %+v", ch)
	}
	if _, ch := up("a1", true, PullRequestOpen); ch.Created || ch.HeadMoved || ch.LeftDraft {
		t.Errorf("a redelivery reported %+v", ch)
	}
	if _, ch := up("b2", true, PullRequestOpen); !ch.HeadMoved {
		t.Errorf("a new head = %+v", ch)
	}
	if _, ch := up("b2", false, PullRequestOpen); !ch.LeftDraft || ch.HeadMoved {
		t.Errorf("leaving draft = %+v", ch)
	}
	// An upsert never ends a pull request: a listing that races a webhook cannot skip the
	// step that completes the task.
	pr, _ := up("b2", false, PullRequestMerged)
	if pr.State != PullRequestOpen {
		t.Errorf("an upsert moved the pull request to %s", pr.State)
	}
	if _, err := f.store.EndPullRequest(f.ctx, f.project.ID, "acme/w", 70, false); err != nil {
		t.Fatal(err)
	}
	if pr, ch := up("b2", false, PullRequestOpen); !ch.Reopened || pr.State != PullRequestOpen {
		t.Errorf("a closed pull request seen open again = %+v (%s)", ch, pr.State)
	}
}

// A task that already carried a pull request under migration 0006 becomes a row, linked and
// primary, so the table is complete from the first poll and a merge still completes the task.
func TestMigrationBackfillsPullRequestsFromTheTaskColumns(t *testing.T) {
	f := newFixture(t)
	body, err := migrationFS.ReadFile("migrations/0020_pull_requests.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, backfill, found := strings.Cut(string(body), "-- Pull requests already linked under migration 0006")
	if !found {
		t.Fatal("the migration has no backfill section")
	}
	backfill = "--" + backfill // the rest of the comment line the cut began inside
	for _, c := range []struct{ url, state, repo string }{
		{"https://github.com/acme/w/pull/30", "open", "acme/w"},
		{"https://ghe.example.com/acme/w/pull/7/", "merged", "acme/w"},
		{"https://github.com/acme/w/pull/31", "closed", "acme/w"},
	} {
		task := f.newTask(t, "had a pull request before "+c.state)
		if _, err := f.store.Pool().Exec(f.ctx, `UPDATE tasks SET pull_request_url = $2, pull_request_state = $3 WHERE id = $1::uuid`, task.ID, c.url, c.state); err != nil {
			t.Fatal(err)
		}
	}
	// A task with no pull request, and one whose URL is not a pull request's, are left alone.
	plain := f.newTask(t, "no pull request")
	odd := f.newTask(t, "an odd address")
	if _, err := f.store.Pool().Exec(f.ctx, `UPDATE tasks SET pull_request_url = 'https://example.com/not-a-pr', pull_request_state = 'open' WHERE id = $1::uuid`, odd.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Pool().Exec(f.ctx, backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// Running it again changes nothing.
	if _, err := f.store.Pool().Exec(f.ctx, backfill); err != nil {
		t.Fatalf("backfill twice: %v", err)
	}
	rows, err := f.store.ListPullRequests(f.ctx, f.project.ID, PullRequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{30: "open", 7: "merged", 31: "closed"}
	if len(rows) != len(want) {
		t.Fatalf("%d rows backfilled, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		if want[r.Number] != r.State || r.Repository != "acme/w" || r.TaskID == "" || r.LinkSource != LinkBranch || !r.Primary {
			t.Errorf("backfilled row = %+v", r)
		}
	}
	if rows, _ := f.store.ListPullRequests(f.ctx, f.project.ID, PullRequestFilter{TaskID: plain.ID}); len(rows) != 0 {
		t.Errorf("a task without a pull request got %d rows", len(rows))
	}
	if rows, _ := f.store.ListPullRequests(f.ctx, f.project.ID, PullRequestFilter{TaskID: odd.ID}); len(rows) != 0 {
		t.Errorf("a task with an address that is not a pull request got %d rows", len(rows))
	}
}
