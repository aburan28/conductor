package notify

import (
	"fmt"
	"sort"
	"strings"

	"github.com/adamburan/conductor/internal/domain"
)

// EventType is a domain event a channel can subscribe to.
type EventType struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// Catalog lists the event types conductord actually writes that are worth sending outside
// the dashboard. Every entry is emitted somewhere in the code; a subscription to anything
// else is refused rather than silently never firing.
//
// Not here, deliberately: attempt.progress (a heartbeat-rate stream nobody wants in Slack),
// and the per-agent coordination chatter (offers, admission tickets) that only the session
// involved acts on.
var Catalog = []EventType{
	{"conflict.blocked", "someone was refused territory another task holds"},
	{"conflict.suggest_join", "someone started work that looks like a task already in flight"},
	{"conflict.detected", "the conflict graph found two tasks colliding (medium severity and up)"},
	{"task.status_changed", "a task moved to another status (narrow with :done, :failed, :blocked_conflict, …)"},
	{"scope.released", "territory someone was blocked on is free again"},
	{"attempt.stalled", "an agent's attempt has gone silent"},
	{"lease.expired", "a lease lapsed and its task was requeued or failed"},
	{"github.pr_merged", "a task's pull request merged"},
	{"github.pr_closed", "a task's pull request closed without merging"},
	{"github.pr_checked", "Conductor checked a pull request against open work"},
	{"task.pull_request_linked", "a pull request was linked to a task"},
	{"task.claimed", "someone started a task"},
	{"task.released", "a task was handed back"},
	{"task.handoff", "a task was handed off to another harness"},
	{"task.blocked", "a task is waiting on a dependency"},
	{"task.unblocked", "a task's dependencies are met"},
	{"task.assigned", "a task was offered to a session"},
	{"attempt.evidence", "an attempt submitted validation evidence"},
	{"budget.downshift", "the project's spend crossed its downshift threshold"},
	{"budget.exhausted", "the project's spend reached its pause threshold"},
	{"budget.shared", "a member shared part of their token allowance"},
	{"quota.warning", "a member's subscription login is near its usage limit"},
	{"quota.exhausted", "a member's subscription login hit its usage limit"},
}

// DefaultEvents is what a channel gets when none are named: the moments a team acts on —
// someone blocked by or converging on another's work, a new conflict, work paused on a
// conflict, territory freed, agents stalled or lost, work landed or failed,
// and money or quota running out.
var DefaultEvents = []string{
	"conflict.blocked",
	"conflict.suggest_join",
	"conflict.detected",
	"task.status_changed:blocked_conflict",
	"scope.released",
	"attempt.stalled",
	"lease.expired",
	"github.pr_merged",
	"task.status_changed:done",
	"task.status_changed:failed",
	"budget.downshift",
	"budget.exhausted",
	"quota.warning",
	"quota.exhausted",
}

// AllEvents subscribes a channel to every catalog entry.
const AllEvents = "*"

func catalogHas(t string) bool {
	for _, e := range Catalog {
		if e.Type == t {
			return true
		}
	}
	return false
}

// NormalizeEvents validates a subscription list, removing duplicates. An empty list means
// DefaultEvents.
func NormalizeEvents(in []string) ([]string, error) {
	if len(in) == 0 {
		return append([]string(nil), DefaultEvents...), nil
	}
	seen := map[string]bool{}
	var out []string
	for _, raw := range in {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" || seen[entry] {
				continue
			}
			if entry == "default" {
				for _, d := range DefaultEvents {
					if !seen[d] {
						seen[d] = true
						out = append(out, d)
					}
				}
				continue
			}
			if err := validEntry(entry); err != nil {
				return nil, err
			}
			seen[entry] = true
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), DefaultEvents...), nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] == AllEvents && out[j] != AllEvents })
	return out, nil
}

func validEntry(entry string) error {
	if entry == AllEvents {
		return nil
	}
	typ, qualifier, qualified := strings.Cut(entry, ":")
	if !catalogHas(typ) {
		return fmt.Errorf("%w: %q is not an event type notifications can carry (see `conductor notify events`)",
			domain.ErrInvalidArgument, typ)
	}
	if !qualified {
		return nil
	}
	if typ != "task.status_changed" {
		return fmt.Errorf("%w: only task.status_changed takes a :status qualifier", domain.ErrInvalidArgument)
	}
	for _, s := range domain.AllTaskStatuses {
		if string(s) == qualifier {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not a task status", domain.ErrInvalidArgument, qualifier)
}

// wants reports whether a subscription list matches an event. A qualified entry matches
// task.status_changed by the status the task moved to.
func wants(subscriptions []string, e domain.Event) bool {
	for _, s := range subscriptions {
		if s == AllEvents {
			if catalogHas(e.Type) {
				return true
			}
			continue
		}
		typ, qualifier, qualified := strings.Cut(s, ":")
		if typ != e.Type {
			continue
		}
		if !qualified {
			return true
		}
		if to, _ := e.Payload["to"].(string); to == qualifier {
			return true
		}
	}
	return false
}
