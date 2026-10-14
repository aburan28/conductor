// Package tracker syncs a team's issue tracker with Conductor's task ledger.
//
// Tasks are a second backlog beside the tracker a team already uses, and asking people to
// keep two is an adoption tax. With sync enabled for a project, open issues that carry the
// project's label become ready tasks, an issue's later edits reach its task, closing or
// reopening it cancels or revives the task, and the task's claim and completion are written
// back to the issue as one short comment each.
//
// This package is the tracker-neutral part: how an issue maps to a task (fields.go), when an
// edit or a close is applied (Plan, StateRule), what the issue is told (PlanWriteBack), and
// the idempotent application of all of it through db.Store. An adapter supplies Items it read
// from its tracker, by webhook or by polling, and a Remote that performs the write-back. Only
// GitHub Issues has an adapter (internal/api/github_issues.go); Linear is the next one
// planned (docs/DESIGN.md §17.5).
//
// Content flows one way, from the tracker to Conductor. Nothing a person writes on a task
// (its title, objective, criteria, progress, or anything private) is ever sent to the
// tracker: the write-back says only who claimed the task, where its pull request is, and
// that it is done.
package tracker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
)

// Item is one issue as an adapter read it.
type Item struct {
	Tracker string // e.g. db.TrackerGitHub
	// Key names the item within its tracker, e.g. acme/widgets#12 for GitHub.
	Key       string
	URL       string
	Title     string
	Body      string
	Open      bool
	Labels    []string
	UpdatedAt time.Time
	// Public is true when anyone can read the item (a public repository's issue).
	Public bool
}

// Ref is the task external_ref an item is imported under: tracker:key, e.g.
// github:acme/widgets#12.
func (it Item) Ref() string { return Ref(it.Tracker, it.Key) }

// Ref formats an external_ref.
func Ref(tracker, key string) string { return tracker + ":" + key }

// ParseRef splits an external_ref written by Ref.
func ParseRef(ref string) (tracker, key string, ok bool) {
	tracker, key, ok = strings.Cut(ref, ":")
	if !ok || tracker == "" || key == "" || strings.ContainsAny(tracker, "/# ") {
		return "", "", false
	}
	return tracker, key, true
}

// Qualifies reports whether an open item is imported under a configuration: it carries the
// configured label, or no label is configured. Labels compare case-insensitively, as GitHub
// does.
func Qualifies(cfg db.TrackerConfig, labels []string) bool {
	if cfg.Label == "" {
		return true
	}
	for _, l := range labels {
		if strings.EqualFold(l, cfg.Label) {
			return true
		}
	}
	return false
}

// ImportVisibility is the visibility an imported task gets. An item anyone can read (a
// public repository's issue) gets the configured public visibility, team_artifacts unless
// changed: hiding a task whose every word is already public protects nothing, and it would
// suppress the write-back (PlanWriteBack). Any other item gets the project's default, as a
// task filed by hand would.
func ImportVisibility(project domain.Project, cfg db.TrackerConfig, public bool) domain.Visibility {
	if public {
		if cfg.PublicVisibility != "" {
			return cfg.PublicVisibility
		}
		return domain.VisibilityTeamArtifacts
	}
	if project.Config.DefaultVisibility != "" {
		return project.Config.DefaultVisibility
	}
	return domain.VisibilityTeamSummary
}

func remoteState(open bool) string {
	if open {
		return db.TrackerRemoteOpen
	}
	return db.TrackerRemoteClosed
}

// Observe applies one reading of an item to a project: import it if it is new and
// qualifies, carry its edits to the task, and mirror its state. Reading the same item again
// changes nothing, and a reading older than one already applied is ignored, so the webhook
// and the poller may both deliver it, in any order, any number of times.
func Observe(ctx context.Context, st *db.Store, cfg db.TrackerConfig, project domain.Project, it Item) (db.TrackerResult, error) {
	fields := MapFields(it.Key, it.Title, it.Body)
	remote := &db.TrackerRemote{URL: it.URL, State: remoteState(it.Open), Public: it.Public,
		UpdatedAt: it.UpdatedAt, TitleHash: fields.TitleHash(), BodyHash: fields.BodyHash()}
	return st.ApplyTrackerItem(ctx, project.ID, it.Tracker, it.Ref(), func(cur db.TrackerItem) (db.TrackerChange, error) {
		return Plan(cfg, project, it, fields, remote, cur), nil
	})
}

// Plan decides what one reading of an item does, given what is on record. It is pure, so the
// rules can be read (and tested) apart from the storage.
//
// Import: an open item that qualifies and is not linked yet becomes a ready task, or, when a
// task filed by hand already names it in external_ref, is linked to that task. A closed item
// is never imported, and an item that stops qualifying (its label removed) keeps its task:
// removing a label is not a decision to drop work.
//
// Edits, last writer wins: a field group (the title; the objective with the criteria)
// changed on the item reaches the task unless the task's copy was also edited in Conductor
// since the last sync, in which case the later edit wins — the item's updated_at against the
// time the task's text was edited (tracker_links.task_edited_at). A Conductor edit that wins
// stays until the item's text changes again. GitHub's updated_at moves for any change to the
// issue (a label, a comment's edit), so the comparison errs toward the item for an edit that
// was not to the text; that is the price of a rule a person can predict.
func Plan(cfg db.TrackerConfig, project domain.Project, it Item, fields Fields, remote *db.TrackerRemote, cur db.TrackerItem) db.TrackerChange {
	if !cur.Linked {
		if !it.Open || !Qualifies(cfg, it.Labels) {
			return db.TrackerChange{}
		}
		if cur.Existing != nil {
			return db.TrackerChange{Adopt: true, Remote: remote}
		}
		return db.TrackerChange{Remote: remote, Create: &db.CreateTaskParams{
			CreatedBy:          cfg.EnabledBy,
			Title:              fields.Title,
			Objective:          fields.Objective,
			AcceptanceCriteria: fields.Criteria,
			Status:             domain.TaskReady,
			Visibility:         ImportVisibility(project, cfg, it.Public),
			MaxAttempts:        project.Config.MaxAttempts,
			WorkflowSHA:        project.WorkflowSHA,
		}}
	}
	if at := cur.Link.RemoteUpdatedAt; at != nil && it.UpdatedAt.Before(*at) {
		return db.TrackerChange{} // an older reading than one already applied
	}
	ch := db.TrackerChange{Remote: remote}
	if remoteWins(cur.Link.SyncedTitleHash, remote.TitleHash, HashTitle(cur.Task.Title), cur.Link.TaskEditedAt, it.UpdatedAt) {
		ch.Title = &fields.Title
	}
	if remoteWins(cur.Link.SyncedBodyHash, remote.BodyHash, HashBody(cur.Task.Objective, cur.Task.AcceptanceCriteria),
		cur.Link.TaskEditedAt, it.UpdatedAt) {
		criteria := fields.Criteria
		ch.Objective, ch.Criteria = &fields.Objective, &criteria
	}
	ch.Status, ch.Reason = StateRule(it.Tracker, it.Open, cur.Task, cur.Link)
	return ch
}

// remoteWins applies the last-writer-wins rule to one field group. synced is the hash of what
// the item last said; remote and local are the item's and the task's current hashes.
func remoteWins(synced, remote, local string, editedAt *time.Time, remoteAt time.Time) bool {
	switch {
	case remote == synced:
		return false // the item did not change it
	case local == synced:
		return true // only the item changed it
	case local == remote:
		return false // both say the same already
	}
	return editedAt == nil || remoteAt.After(*editedAt)
}

// StateRule mirrors an item's state onto its task.
//
//   - Closed: the task is cancelled, unless it is already finished (done, cancelled,
//     superseded) or its work is waiting to land (verifying, review, merging, or an open pull
//     request). A pull request that closes the issue ("Closes #12") closes it moments before
//     the merge completes the task, and cancelling then would beat the merge to it; such a
//     task is left for the merge to finish, or, if the pull request closes unmerged and the
//     task returns to ready, cancelled then (the write-back re-checks it).
//   - Reopened: a task the sync cancelled goes back to ready. A task a person cancelled, or one
//     that is done, stays as it is.
func StateRule(tracker string, open bool, task domain.Task, link db.TrackerLink) (db.TrackerStatusChange, string) {
	switch {
	case !open && !finished(task.Status) && !workLanding(task):
		return db.TrackerCancel, "issue closed on " + tracker
	case open && task.Status == domain.TaskCancelled && link.CancelledBySync:
		return db.TrackerRevive, "issue reopened on " + tracker
	}
	return db.TrackerKeepStatus, ""
}

func finished(s domain.TaskStatus) bool {
	return s == domain.TaskDone || s == domain.TaskCancelled || s == domain.TaskSuperseded
}

func workLanding(t domain.Task) bool {
	return db.PendingMerge(t.Status) || t.PullRequestState == db.PullRequestOpen
}

// Recheck applies StateRule from the item state on record, without a fresh reading: the
// write-back calls it for a task whose item closed while its work was landing, once the task
// has moved on.
func Recheck(ctx context.Context, st *db.Store, link db.TrackerLink) (db.TrackerResult, error) {
	return st.ApplyTrackerItem(ctx, link.ProjectID, link.Tracker, link.ExternalRef, func(cur db.TrackerItem) (db.TrackerChange, error) {
		if !cur.Linked {
			return db.TrackerChange{}, nil
		}
		status, reason := StateRule(cur.Link.Tracker, cur.Link.RemoteState == db.TrackerRemoteOpen, cur.Task, cur.Link)
		return db.TrackerChange{Status: status, Reason: reason}, nil
	})
}

// ---------------------------------------------------------------------------
// Write-back
// ---------------------------------------------------------------------------

// Remote is what an adapter does on its tracker for the write-back. key is an Item.Key.
type Remote interface {
	Comment(ctx context.Context, key, body string) error
	// SetLabel adds the label when present is true and removes it otherwise.
	SetLabel(ctx context.Context, key, label string, present bool) error
	// IsOpen reads whether the item is open now, just before closing it.
	IsOpen(ctx context.Context, key string) (bool, error)
	Close(ctx context.Context, key string) error
	// Shareable reports whether a URL may appear on the item. The GitHub adapter allows a
	// pull request in the item's own repository, which every reader of the item can see.
	Shareable(key, url string) bool
}

// WriteBackPlan is what an item is to be told about its task.
type WriteBackPlan struct {
	Claim string // a comment announcing the claim, when one is due
	// Label is the label to add (AddLabel) or the one to remove (RemoveLabel).
	AddLabel, RemoveLabel string
	// Done is the completion comment, posted with Close, which closes the item: what a done
	// task's open item is told, once.
	Done    string
	Close   bool
	Recheck bool // the item closed and the task can now follow it
}

// Empty reports whether the plan does nothing.
func (p WriteBackPlan) Empty() bool {
	return p == WriteBackPlan{}
}

// PlanWriteBack decides what an item is told about its task. The rules, which exist so a
// tracker learns no more than its readers should:
//
//   - Never any task content: a comment says who claimed the task, where its pull request is,
//     and that it is done, in fixed words.
//   - A private task linked to a public item is not written back at all (a label put on it
//     earlier is still removed): announcing it would publish that private work exists.
//   - The claimant's handle is named only when the task is not private and either the item
//     is not public or the task is shared at team_artifacts or above; otherwise the comment
//     says only that the task was claimed.
//   - A pull request is linked only if the Remote calls its URL shareable.
//   - One claim comment per claimant: a lease that expires and is taken again by the same
//     person says nothing new.
func PlanWriteBack(t db.TrackedTask, r Remote) WriteBackPlan {
	var p WriteBackPlan
	open := t.Link.RemoteState == db.TrackerRemoteOpen
	if !open && !finished(t.Status) && !db.PendingMerge(t.Status) && t.PullRequestState != db.PullRequestOpen {
		p.Recheck = true
	}
	silent := t.Visibility == domain.VisibilityPrivate && t.Link.RemotePublic
	working := !finished(t.Status) && (t.HolderID != "" || db.PendingMerge(t.Status))

	want := ""
	if working && open && !silent {
		want = t.InProgressLabel
	}
	if t.Link.LabelApplied != want {
		if t.Link.LabelApplied != "" {
			p.RemoveLabel = t.Link.LabelApplied
		}
		p.AddLabel = want
	}
	if silent {
		return p
	}
	_, key, _ := ParseRef(t.Link.ExternalRef)
	if open && t.HolderID != "" && t.HolderID != t.Link.ClaimNotedFor && !finished(t.Status) {
		p.Claim = "Claimed via Conductor."
		if named(t) {
			p.Claim = fmt.Sprintf("Claimed by `%s` via Conductor.", strings.ReplaceAll(t.HolderHandle, "`", ""))
		}
	}
	if t.Status == domain.TaskDone && open && !t.Link.DoneNoted {
		p.Close = true
		p.Done = "Done via Conductor."
		if t.PullRequestURL != "" && r != nil && r.Shareable(key, t.PullRequestURL) {
			p.Done = "Done via Conductor in " + t.PullRequestURL + "."
		}
	}
	return p
}

func named(t db.TrackedTask) bool {
	if t.Visibility == domain.VisibilityPrivate || t.HolderHandle == "" {
		return false
	}
	return !t.Link.RemotePublic || t.Visibility == domain.VisibilityTeamArtifacts || t.Visibility == domain.VisibilitySharedDebug
}

// WriteBack tells an item what PlanWriteBack says it should be told, recording each step as
// soon as the tracker accepted it, so a pass cut short (a rate limit, a crash) does not post a
// comment twice. Delivery is at least once: a crash between a comment being accepted and its
// record being written repeats that one comment.
//
// A completed task's item gets one comment and is closed, unless it is closed already — most
// often because the pull request's "Closes #N" closed it — in which case the closing says
// what happened and no comment is added. The completion is recorded once both are done, so a
// close that fails is retried, at the cost of repeating its comment.
func WriteBack(ctx context.Context, st *db.Store, t db.TrackedTask, r Remote) error {
	p := PlanWriteBack(t, r)
	if p.Recheck {
		// The task changes status; the next pass sees it changed and writes that back.
		res, err := Recheck(ctx, st, t.Link)
		if err != nil || res.Status != res.From {
			return err
		}
	}
	_, key, ok := ParseRef(t.Link.ExternalRef)
	if !ok {
		return fmt.Errorf("tracker link %q is not tracker:key", t.Link.ExternalRef)
	}
	noted := db.TrackerNoted{ClaimNotedFor: t.Link.ClaimNotedFor, LabelApplied: t.Link.LabelApplied, DoneNoted: t.Link.DoneNoted}
	record := func(reconciled *time.Time) error {
		return st.RecordTrackerWriteBack(ctx, t.Link.ProjectID, t.Link.ExternalRef, noted, reconciled)
	}
	if p.Claim != "" {
		if err := r.Comment(ctx, key, p.Claim); err != nil {
			return err
		}
		noted.ClaimNotedFor = t.HolderID
		if err := record(nil); err != nil {
			return err
		}
	}
	if p.RemoveLabel != "" {
		if err := r.SetLabel(ctx, key, p.RemoveLabel, false); err != nil {
			return err
		}
		noted.LabelApplied = ""
		if err := record(nil); err != nil {
			return err
		}
	}
	if p.AddLabel != "" {
		if err := r.SetLabel(ctx, key, p.AddLabel, true); err != nil {
			return err
		}
		noted.LabelApplied = p.AddLabel
		if err := record(nil); err != nil {
			return err
		}
	}
	if p.Close {
		open, err := r.IsOpen(ctx, key)
		if err != nil {
			return err
		}
		if open {
			if err := r.Comment(ctx, key, p.Done); err != nil {
				return err
			}
			if err := r.Close(ctx, key); err != nil {
				return err
			}
		}
		noted.DoneNoted, noted.RemoteState = true, db.TrackerRemoteClosed
	} else if t.Status == domain.TaskDone {
		// The item was closed already; the closing says what happened. Once noted, an item
		// someone reopens after its task is done is left open.
		noted.DoneNoted = true
	}
	updated := t.UpdatedAt
	return record(&updated)
}
