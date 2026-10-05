package mcp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// defaultKeepPoll is how often an idle lease keeper checks whether a claim has appeared.
const defaultKeepPoll = 5 * time.Second

// Bounds on the renewal interval. Renewing at a third of the remaining lifetime leaves two
// missed beats before a lease lapses, whatever TTL the project chose.
const (
	minKeepInterval = time.Second
	maxKeepInterval = 30 * time.Second
)

func (s *Server) setFence(f domain.Fence) {
	s.fenceMu.Lock()
	s.fence = f
	s.fenceMu.Unlock()
}

func (s *Server) currentFence() domain.Fence {
	s.fenceMu.Lock()
	defer s.fenceMu.Unlock()
	return s.fence
}

// keepLeaseAlive renews the lease of the task this gateway session has claimed, for as long
// as ctx lives and alive reports the session is still in use (nil means always).
//
// A heartbeat is not a tool (DESIGN.md §7.2): a model should never spend tokens telling the
// server it is still alive, and a model that is thinking, or running a long test suite, makes
// no tool calls for minutes. Before this the lease lived only as long as the model happened
// to call coord_report_progress, so a claim taken through MCP lapsed after one TTL and the
// reconciler handed the territory to someone else mid-edit.
//
// When the server says the fence is stale or the lease is gone, the keeper stops renewing
// that fence: the claim is lost, and the agent's next fenced call reports it plainly. A new
// claim (a new fence) is picked up on the next pass.
func (s *Server) keepLeaseAlive(ctx context.Context, alive func() bool) {
	poll := s.keepPoll
	if poll <= 0 {
		poll = defaultKeepPoll
	}
	var dead domain.ID
	wait := poll
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = poll
		fence := s.currentFence()
		if fence.LeaseID == "" || fence.LeaseID == dead || (alive != nil && !alive()) {
			continue
		}
		var lease domain.Lease
		err := s.api.Post(ctx, "/v1/leases/heartbeat", map[string]any{
			"task_id": fence.TaskID, "attempt_id": fence.AttemptID,
			"lease_id": fence.LeaseID, "fencing_epoch": fence.FencingEpoch,
		}, &lease)
		var apiErr *client.APIError
		switch {
		case err == nil:
			wait = renewalInterval(time.Until(lease.ExpiresAt), poll)
		case errors.As(err, &apiErr) && (apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusNotFound):
			dead = fence.LeaseID
		}
		// Any other failure (the control plane is restarting, the network blinked) is retried
		// on the ordinary poll, which is well inside a lease's lifetime.
	}
}

// renewalInterval is a third of a lease's remaining lifetime, within sane bounds.
func renewalInterval(remaining, fallback time.Duration) time.Duration {
	if remaining <= 0 {
		return fallback
	}
	d := remaining / 3
	if d < minKeepInterval {
		d = minKeepInterval
	}
	if d > maxKeepInterval {
		d = maxKeepInterval
	}
	return d
}
