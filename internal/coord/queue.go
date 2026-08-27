package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
)

// The admission queue (DESIGN.md §7.7 concurrency, domain.AdmissionTicket).
//
// A project can cap how many sessions or attempts run at once. Past the cap, work does not
// fail — it takes a ticket and waits, and the scheduler grants tickets in arrival order as
// slots free up. This service layer is thin: the store owns the transactional grant logic
// under the project advisory lock; here we resolve the project's policy and emit events.

// EnqueueRequest asks for a slot.
type EnqueueRequest struct {
	Kind      domain.TicketKind
	SessionID domain.ID
	TaskRef   string
	Harness   string
	Model     string
	Priority  int
	Note      string
}

// Enqueue takes a ticket for the caller. The result says whether it was granted at once or
// is waiting, and where in line.
func (s *Service) Enqueue(ctx context.Context, c Caller, projectID domain.ID, req EnqueueRequest) (domain.AdmissionTicket, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	var taskID domain.ID
	if req.TaskRef != "" {
		task, err := resolveTask(ctx, s.Store, projectID, req.TaskRef)
		if err != nil {
			return domain.AdmissionTicket{}, err
		}
		taskID = task.ID
	}
	ticket, err := s.Store.Enqueue(ctx, db.EnqueueParams{
		ProjectID: projectID, PrincipalID: c.Principal.ID, SessionID: req.SessionID,
		TaskID: taskID, Kind: req.Kind, Harness: req.Harness, Model: req.Model,
		Priority: req.Priority, Note: req.Note, Policy: project.Config.Queue,
	})
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	s.emitTicket(ctx, project, c.Principal.ID, ticket)
	return ticket, nil
}

// HeartbeatTicket keeps a ticket alive while its holder waits or works.
func (s *Service) HeartbeatTicket(ctx context.Context, c Caller, ticketID domain.ID) (domain.AdmissionTicket, error) {
	ticket, err := s.Store.GetTicket(ctx, ticketID)
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	if ticket.PrincipalID != c.Principal.ID && !c.Role.Can(domain.RoleMaintainer) {
		return domain.AdmissionTicket{}, fmt.Errorf("%w: not your ticket", domain.ErrNotPermitted)
	}
	project, err := s.Store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	return s.Store.HeartbeatTicket(ctx, ticketID, project.Config.Queue.TicketTTL())
}

// ReleaseTicket ends a ticket. Its holder or a maintainer may release it; releasing frees a
// slot for the next in line.
func (s *Service) ReleaseTicket(ctx context.Context, c Caller, ticketID domain.ID, cancelled bool) (domain.AdmissionTicket, error) {
	ticket, err := s.Store.GetTicket(ctx, ticketID)
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	if ticket.PrincipalID != c.Principal.ID && !c.Role.Can(domain.RoleMaintainer) {
		return domain.AdmissionTicket{}, fmt.Errorf("%w: not your ticket", domain.ErrNotPermitted)
	}
	project, err := s.Store.GetProject(ctx, ticket.ProjectID)
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	state := domain.TicketReleased
	if cancelled {
		state = domain.TicketCancelled
	}
	out, err := s.Store.ReleaseTicket(ctx, ticketID, state, project.Config.Queue)
	if err != nil {
		return domain.AdmissionTicket{}, err
	}
	s.emitTicket(ctx, project, c.Principal.ID, out)
	return out, nil
}

// QueueView is the queue as the API and dashboard render it.
type QueueView struct {
	Policy  domain.QueuePolicy       `json:"policy"`
	Active  QueueActive              `json:"active"`
	Tickets []domain.AdmissionTicket `json:"tickets"`
}

// QueueActive is what currently occupies slots.
type QueueActive struct {
	Sessions int `json:"sessions"`
	Attempts int `json:"attempts"`
}

// Queue returns the current admission queue for a project.
func (s *Service) Queue(ctx context.Context, projectID domain.ID, includeClosed bool) (QueueView, error) {
	project, err := s.Store.GetProject(ctx, projectID)
	if err != nil {
		return QueueView{}, err
	}
	snap, err := s.Store.ListQueue(ctx, projectID, includeClosed)
	if err != nil {
		return QueueView{}, err
	}
	return QueueView{
		Policy:  project.Config.Queue,
		Active:  QueueActive{Sessions: snap.ActiveSessions, Attempts: snap.ActiveAttempts},
		Tickets: snap.Tickets,
	}, nil
}

func (s *Service) emitTicket(ctx context.Context, project domain.Project, actor domain.ID, ticket domain.AdmissionTicket) {
	eventType := "queue.waiting"
	if ticket.State == domain.TicketGranted {
		eventType = "queue.granted"
	} else if !ticket.State.Open() {
		eventType = "queue.released"
	}
	_ = s.Store.AppendEvent(ctx, project.OrganizationID, project.ID, actor,
		"ticket", ticket.ID, eventType, domain.VisibilityTeamSummary, map[string]any{
			"ticket_id": ticket.ID, "kind": string(ticket.Kind), "state": string(ticket.State),
			"position": ticket.Position, "harness": ticket.Harness, "model": ticket.Model,
			"task_ref": ticket.TaskRef,
		})
}

// resolveTask finds a task by id or project-scoped ref.
func resolveTask(ctx context.Context, store *db.Store, projectID domain.ID, ref string) (domain.Task, error) {
	if task, err := store.GetTask(ctx, ref); err == nil {
		return task, nil
	} else if !errors.Is(err, domain.ErrNotFound) && !badUUID(err) {
		return domain.Task{}, err
	}
	return store.GetTaskByRef(ctx, projectID, ref)
}

// badUUID reports whether an error came from casting a non-UUID string, which happens when a
// ref is passed where an id was expected. That is a lookup miss, not a fault.
func badUUID(err error) bool {
	return err != nil && strings.Contains(err.Error(), "invalid input syntax for type uuid")
}
