package coord

import (
	"context"
	"sort"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// The swarm view (DESIGN.md §7.10, §13.8).
//
// A "swarm" is the pooled execution capacity of a team: the runners that can launch fresh
// harnesses, plus the interactive sessions that are accepting work, plus each member's token
// position — so a coworker with headroom to spare can see it and share it. It answers "how
// much can we run right now, who is contributing it, and who has budget left", which is what
// you need before you decide whether to queue work or ask someone to connect a machine.
//
// As everywhere else, this shares capacity and identity, never work content: a contributor
// row names a person, a harness, and a budget position — never what anyone is building.

// SwarmView is the whole picture.
type SwarmView struct {
	Contributors []SwarmContributor `json:"contributors"`
	Capacity     SwarmCapacity      `json:"capacity"`
	QueueDepth   int                `json:"queue_depth"`
}

// SwarmCapacity is the roll-up.
type SwarmCapacity struct {
	Runners           int `json:"runners"`
	SessionsAccepting int `json:"sessions_accepting"`
	SlotsFree         int `json:"slots_free"`
}

// SwarmContributor is one source of capacity: a runner machine or a live session.
type SwarmContributor struct {
	Principal     string               `json:"principal"`
	PrincipalID   domain.ID            `json:"principal_id"`
	Kind          string               `json:"kind"` // runner | session
	Name          string               `json:"name,omitempty"`
	Harness       string               `json:"harness,omitempty"`
	Harnesses     []string             `json:"harnesses,omitempty"`
	Models        []string             `json:"models,omitempty"`
	State         string               `json:"state,omitempty"`
	InFlight      int                  `json:"in_flight,omitempty"`
	MaxConcurrent int                  `json:"max_concurrent,omitempty"`
	Accepting     bool                 `json:"accepting"`
	LastHeartbeat time.Time            `json:"last_heartbeat"`
	Budget        *domain.MemberBudget `json:"budget,omitempty"`
}

// Swarm builds the swarm view for a project. Member budgets are attached when the project
// runs a per-member allowance; otherwise the budget field is left off.
func (s *Service) Swarm(ctx context.Context, c Caller, projectID domain.ID) (SwarmView, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return SwarmView{}, err
	}
	view := SwarmView{}

	// Runners.
	runners, err := s.Store.ListRunners(ctx, projectID)
	if err != nil {
		return SwarmView{}, err
	}
	principalIDs := map[domain.ID]bool{}
	for _, r := range runners {
		online := r.State == "online" && time.Since(r.HeartbeatAt) < 2*time.Minute
		if online {
			view.Capacity.Runners++
			view.Capacity.SlotsFree += r.MaxConcurrency - r.InFlight
		}
		principalIDs[r.PrincipalID] = true
		view.Contributors = append(view.Contributors, SwarmContributor{
			PrincipalID: r.PrincipalID, Kind: "runner", Name: r.Name,
			Harnesses: r.Capabilities.Harnesses, Models: r.Capabilities.Models,
			State: r.State, InFlight: r.InFlight, MaxConcurrent: r.MaxConcurrency,
			Accepting: online && r.InFlight < r.MaxConcurrency, LastHeartbeat: r.HeartbeatAt,
		})
	}

	// Live sessions.
	sessions, err := s.Store.LiveSessions(ctx, projectID)
	if err != nil {
		return SwarmView{}, err
	}
	for _, sess := range sessions {
		principalIDs[sess.PrincipalID] = true
		accepting := sess.State.Accepting()
		if accepting {
			view.Capacity.SessionsAccepting++
		}
		models := []string{}
		if sess.Capabilities.Model != "" {
			models = []string{sess.Capabilities.Model}
		}
		view.Contributors = append(view.Contributors, SwarmContributor{
			PrincipalID: sess.PrincipalID, Kind: "session", Harness: sess.Harness,
			Models: models, State: string(sess.State), Accepting: accepting,
			LastHeartbeat: sess.HeartbeatAt,
		})
	}

	// Names.
	ids := make([]domain.ID, 0, len(principalIDs))
	for id := range principalIDs {
		ids = append(ids, id)
	}
	principals, err := s.Store.PrincipalsByID(ctx, ids)
	if err != nil {
		return SwarmView{}, err
	}
	for i := range view.Contributors {
		view.Contributors[i].Principal = principals[view.Contributors[i].PrincipalID].Handle
	}

	// Budgets, when the project runs an allowance.
	if allowance := project.Config.Budget.MemberTokens; allowance > 0 {
		budgets, err := s.Store.MemberBudgets(ctx, projectID, allowance)
		if err != nil {
			return SwarmView{}, err
		}
		byID := map[domain.ID]domain.MemberBudget{}
		for _, b := range budgets {
			byID[b.PrincipalID] = b
		}
		for i := range view.Contributors {
			if b, ok := byID[view.Contributors[i].PrincipalID]; ok {
				bc := b
				view.Contributors[i].Budget = &bc
			}
		}
	}

	// Queue depth.
	snap, err := s.Store.ListQueue(ctx, projectID, false)
	if err != nil {
		return SwarmView{}, err
	}
	for _, t := range snap.Tickets {
		if t.State == domain.TicketQueued {
			view.QueueDepth++
		}
	}

	sort.SliceStable(view.Contributors, func(i, j int) bool {
		a, b := view.Contributors[i], view.Contributors[j]
		if a.Accepting != b.Accepting {
			return a.Accepting
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Principal < b.Principal
	})
	if view.Contributors == nil {
		view.Contributors = []SwarmContributor{}
	}
	if view.Capacity.SlotsFree < 0 {
		view.Capacity.SlotsFree = 0
	}
	return view, nil
}
