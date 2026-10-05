package coord

import (
	"context"
	"log/slog"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// maxJoinAnnouncements bounds the suggest_join events one check can write: the closest few
// candidates are the useful ones.
const maxJoinAnnouncements = 3

// announceIntent records, as domain events, that a check or start-work ran into someone
// else's work: refused by another task's territory (conflict.blocked), or pointed at similar
// work already in flight (conflict.suggest_join). These are the moments a team wants to hear
// about outside the terminal where they happened — the holder learns someone is waiting, the
// team sees two efforts converging — and notification channels subscribe to them.
//
// Each is written at most once per (requester, task in the way, outcome) per
// db.ConflictAlertWindow, so an agent polling a blocked check does not flood the stream. The
// payload names the requester, the task in the way, and the contested territory — what the
// refusal itself told the requester — and never the requester's summary or the holder's
// title. Announcing is best effort: the decision has been made and is returned either way.
func (s *Service) announceIntent(ctx context.Context, c Caller, project domain.Project, decision IntentDecision) {
	if c.System || c.Principal.ID == "" {
		return
	}
	var out []db.ConflictAnnouncement
	switch decision.Outcome {
	case domain.OutcomeBlockConflict:
		byTask := map[domain.ID]int{}
		for _, cf := range decision.Conflicts {
			// Only someone else's territory: a principal blocked by their own other task
			// already knows.
			if !cf.Outcome.Blocks() || cf.HolderTaskID == "" || cf.HolderOwner == c.Principal.Handle {
				continue
			}
			i, seen := byTask[cf.HolderTaskID]
			if !seen {
				out = append(out, db.ConflictAnnouncement{
					OrganizationID: project.OrganizationID, ProjectID: project.ID,
					Requester: c.Principal.ID, TaskID: cf.HolderTaskID, Outcome: db.ConflictAlertBlocked,
					Payload: map[string]any{
						"task_id": cf.HolderTaskID, "task_ref": cf.HolderTaskRef,
						"principal": c.Principal.Handle, "principal_id": c.Principal.ID,
						"outcome": string(domain.OutcomeBlockConflict), "kind": string(cf.Kind),
						"severity": string(cf.Severity), "resources": []string{},
					},
				})
				i = len(out) - 1
				byTask[cf.HolderTaskID] = i
			}
			res := out[i].Payload["resources"].([]string)
			out[i].Payload["resources"] = appendUnique(res, cf.ResourceKey)
		}
	case domain.OutcomeSuggestJoin:
		for _, d := range decision.Duplicates {
			if len(out) == maxJoinAnnouncements {
				break
			}
			if d.Exact || d.TaskID == "" || d.OwnerID == c.Principal.ID {
				continue
			}
			out = append(out, db.ConflictAnnouncement{
				OrganizationID: project.OrganizationID, ProjectID: project.ID,
				Requester: c.Principal.ID, TaskID: d.TaskID, Outcome: db.ConflictAlertSuggestJoin,
				Payload: map[string]any{
					"task_id": d.TaskID, "task_ref": d.TaskRef,
					"principal": c.Principal.Handle, "principal_id": c.Principal.ID,
					"outcome": string(domain.OutcomeSuggestJoin),
				},
			})
		}
	}
	for _, a := range out {
		if _, err := s.Store.AnnounceConflict(ctx, a, db.ConflictAlertWindow); err != nil {
			slog.Warn("conflict announcement not written", "outcome", a.Outcome, "task", a.TaskID, "error", err)
		}
	}
}
