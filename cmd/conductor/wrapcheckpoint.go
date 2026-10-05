package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
)

// The wrap sidecar's checkpointer. Every few minutes (CONDUCTOR_CHECKPOINT_INTERVAL,
// default 2m) it bundles the harness's transcript and the working tree into a local
// checkpoint, writing nothing when nothing changed, so the moment a usage limit lands or
// the machine goes away there is a bundle at most one interval old to continue from —
// under another login, on another machine, or in another harness. A final capture runs on
// the way down. Off with CONDUCTOR_CHECKPOINT=off. Nothing here talks to the control plane.
type wrapCheckpointer struct {
	harness   string
	cwd       string
	pid       int
	started   time.Time
	conductor checkpoint.ConductorRef
	resumed   string
	lastID    string
	lastErr   error
	captures  int
}

func newWrapCheckpointer(harness, cwd string, pid int, conductor checkpoint.ConductorRef) *wrapCheckpointer {
	return &wrapCheckpointer{
		harness: harness, cwd: cwd, pid: pid, started: time.Now().Add(-time.Minute),
		conductor: conductor, resumed: os.Getenv("CONDUCTOR_RESUMED_FROM"),
	}
}

func (c *wrapCheckpointer) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.capture(ctx, checkpoint.ReasonPeriodic, false)
		}
	}
}

// capture takes one checkpoint. A failure is remembered for the exit line and never
// surfaces mid-session: the harness owns the terminal.
func (c *wrapCheckpointer) capture(ctx context.Context, reason string, force bool) {
	res, err := checkpoint.Capture(ctx, checkpoint.Request{
		Harness: c.harness, Cwd: c.cwd, PID: c.pid, Since: c.started,
		Reason: reason, Conductor: c.conductor, ResumedFrom: c.resumed, Force: force,
	})
	if err != nil {
		c.lastErr = err
		return
	}
	c.lastErr = nil
	if res.Skipped == "" {
		c.lastID, c.captures = res.Manifest.ID, c.captures+1
	} else if c.lastID == "" {
		c.lastID = res.Manifest.ID
	}
}

// final runs the exit capture on a context that survives Ctrl-C, bounded so a shutdown is
// never held up by a slow git.
func (c *wrapCheckpointer) final(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c.capture(ctx, reason, false)
}

// exitLine is what the user sees after the harness exits.
func (c *wrapCheckpointer) exitLine() string {
	switch {
	case c.lastID != "":
		return fmt.Sprintf("Conductor checkpointed this session (%d capture(s); latest %s). `conductor checkpoint resume %s` continues it anywhere.",
			c.captures, checkpoint.ShortID(c.lastID), checkpoint.ShortID(c.lastID))
	case c.lastErr != nil:
		return "Conductor could not checkpoint this session: " + errString(c.lastErr)
	}
	return ""
}
