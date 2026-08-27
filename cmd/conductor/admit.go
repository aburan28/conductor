package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// sessionAdmission holds a `conductor wrap` session's place in the admission queue.
//
// When a project caps how many sessions run at once, a wrap does not barge in: it takes a
// ticket, and if the ticket is not granted immediately it waits — telling the user where in
// line they are — until a slot frees up. The sidecar heartbeats the ticket so a granted slot
// is not reclaimed mid-session, and releases it on exit so the next person in line advances.
type sessionAdmission struct {
	api      *client.Client
	project  string
	ticketID domain.ID
}

// admitSession requests a slot for a session and blocks until it is granted. A project with
// no session cap returns immediately with nothing to heartbeat or release. A failure to reach
// the queue is not fatal — coordination is cooperative, and a wrap that cannot queue still
// runs rather than being held hostage by a control-plane hiccup.
func admitSession(ctx context.Context, api *client.Client, project, sessionID, harness, model string) *sessionAdmission {
	a := &sessionAdmission{api: api, project: project}
	var ticket domain.AdmissionTicket
	err := api.Post(ctx, "/v1/projects/"+project+"/queue", map[string]any{
		"kind": string(domain.TicketSession), "session_id": sessionID,
		"harness": harness, "model": model,
	}, &ticket)
	if err != nil {
		return a // unlimited project, or an unreachable queue: proceed without a ticket
	}
	a.ticketID = ticket.ID
	if ticket.State == domain.TicketGranted {
		return a
	}

	fmt.Fprintf(os.Stderr, "Conductor: waiting for a session slot (position %d in the queue)…\n", ticket.Position)
	poll := time.NewTicker(3 * time.Second)
	defer poll.Stop()
	lastPos := ticket.Position
	for {
		select {
		case <-ctx.Done():
			return a
		case <-poll.C:
			a.heartbeat(ctx)
			var cur domain.AdmissionTicket
			if err := api.Get(ctx, "/v1/queue/"+a.ticketID, &cur); err != nil {
				// Fall back to the project queue view if the single-ticket read is unavailable.
				continue
			}
			if cur.State == domain.TicketGranted {
				fmt.Fprintln(os.Stderr, "Conductor: slot granted, starting.")
				return a
			}
			if !cur.State.Open() {
				return a // expired or cancelled; stop waiting and let the session run
			}
			if cur.Position != lastPos && cur.Position > 0 {
				lastPos = cur.Position
				fmt.Fprintf(os.Stderr, "Conductor: position %d in the queue…\n", cur.Position)
			}
		}
	}
}

func (a *sessionAdmission) heartbeat(ctx context.Context) {
	if a == nil || a.ticketID == "" {
		return
	}
	_ = a.api.Post(ctx, "/v1/queue/"+a.ticketID+"/heartbeat", nil, nil)
}

func (a *sessionAdmission) release(ctx context.Context) {
	if a == nil || a.ticketID == "" {
		return
	}
	_ = a.api.Delete(ctx, "/v1/queue/"+a.ticketID, nil)
}
