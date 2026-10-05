package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/adamburan/conductor/internal/checkpoint"
	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/quota"
)

// The wrap sidecar's quota watcher. Every minute (CONDUCTOR_QUOTA_INTERVAL) it reads what the
// tools on this machine expose about their usage windows, reports changed readings to the
// control plane, and acts on a level crossed for the first time in its window: a desktop
// notification at every level and, when the login this session runs on reaches the critical
// threshold or runs out, an immediate checkpoint and the exact command that continues the
// session on the login or tool with the most headroom. Off with CONDUCTOR_QUOTA=off. Like
// the usage collector, it never disturbs the session: every failure is swallowed.
type quotaWatcher struct {
	api     *client.Client
	project string
	harness string
	account string
	cwd     string
	pid     int
	started time.Time
	session domain.ID
	th      quota.Thresholds
	home    string

	collect  func(context.Context) quota.Result
	raise    func([]quota.Snapshot, quota.Thresholds, time.Time) ([]quota.Alert, error)
	capture  func(context.Context) (string, error)
	notifier quota.Notifier
	out      io.Writer
	now      func() time.Time

	sent map[string]time.Time // window key → observed_at last reported
}

// startQuotaWatch is the sidecar's one entry point. It returns at once; the watcher runs
// until ctx ends.
func startQuotaWatch(ctx context.Context, api *client.Client, project string, session domain.ID, harness, cwd string, pid int) {
	if quota.Disabled(os.Getenv) {
		return
	}
	h := checkpoint.NormalizeHarness(harness)
	home, _ := os.UserHomeDir()
	repoRoot, _ := config.FindRoot(cwd)
	w := &quotaWatcher{
		api: api, project: project, harness: h, cwd: cwd, pid: pid, session: session,
		account: quota.AccountLabel(h, harnessStateDir(h, os.Getenv), home),
		started: time.Now().Add(-time.Minute), th: quota.LoadThresholds(repoRoot, os.Getenv), home: home,
		collect:  func(ctx context.Context) quota.Result { return quota.Collect(ctx, quota.Options{}) },
		raise:    quota.RaiseLocal,
		notifier: quota.DefaultNotifier(os.Getenv), out: os.Stderr, now: time.Now,
		sent: map[string]time.Time{},
	}
	w.capture = w.checkpoint
	go w.run(ctx, quotaIntervalFromEnv(os.Getenv))
}

// quotaIntervalFromEnv reads CONDUCTOR_QUOTA_INTERVAL; the default is one minute.
func quotaIntervalFromEnv(getenv func(string) string) time.Duration {
	if d, err := time.ParseDuration(getenv("CONDUCTOR_QUOTA_INTERVAL")); err == nil && d >= 15*time.Second {
		return d
	}
	return time.Minute
}

func (w *quotaWatcher) run(ctx context.Context, interval time.Duration) {
	// The first reading comes soon after launch rather than a full interval in, so a login
	// that is already nearly spent is flagged before the session gets going.
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			w.tick(ctx)
			timer.Reset(interval)
		}
	}
}

// tick is one pass. A panic in it is contained here: whatever a tool's log looks like, the
// session it describes keeps running.
func (w *quotaWatcher) tick(ctx context.Context) {
	defer func() { _ = recover() }()
	res := w.collect(ctx)
	w.report(ctx, res.Snapshots)
	alerts, err := w.raise(res.Snapshots, w.th, w.now())
	if err != nil {
		return
	}
	for _, a := range alerts {
		w.act(ctx, a, res.Snapshots)
	}
}

// report sends the readings that are new since the last report.
func (w *quotaWatcher) report(ctx context.Context, snaps []quota.Snapshot) {
	if w.api == nil {
		return
	}
	var changed []quota.Snapshot
	for _, s := range snaps {
		if prev, ok := w.sent[s.Key()]; !ok || s.ObservedAt.After(prev) {
			changed = append(changed, s)
		}
	}
	if len(changed) == 0 {
		return
	}
	if err := postQuota(ctx, w.api, w.project, changed, w.th); err != nil {
		return // the next tick re-sends what did not land
	}
	for _, s := range changed {
		w.sent[s.Key()] = s.ObservedAt
	}
}

// act raises one alert: a notification always; for this session's own login at the critical
// level or beyond, a checkpoint and the command that continues elsewhere.
func (w *quotaWatcher) act(ctx context.Context, a quota.Alert, snaps []quota.Snapshot) {
	now := w.now()
	line := quota.Describe(a, now)
	w.notifier.Notify("Conductor: usage limit", line)
	mine := a.Snapshot.Harness == w.harness && a.Snapshot.Account == w.account
	if !mine || a.Level.Rank() < quota.LevelCritical.Rank() {
		return
	}
	id, err := w.capture(ctx)
	msg := "\r\nConductor: " + line + ".\r\n"
	switch {
	case err != nil:
		msg += "  Could not checkpoint this session: " + err.Error() + "\r\n  "
	case id == "":
		msg += "  "
	default:
		msg += "  Checkpoint " + checkpoint.ShortID(id) + " saved. "
	}
	if s, ok := quota.Suggest(snaps, w.harness, w.account, w.th, now, w.home, os.Getenv); ok {
		head := "no reading yet"
		if s.Headroom != nil {
			head = quota.FormatPercent(100-*s.Headroom) + " used"
		}
		msg += fmt.Sprintf("Continue on %s %q (%s):\r\n      %s\r\n", s.Harness, s.Account, head, s.Command(checkpoint.ShortID(id)))
	} else {
		msg += "No other login on this machine has room.\r\n"
	}
	fmt.Fprint(w.out, msg)
}

// checkpoint captures this session now, regardless of the periodic schedule.
func (w *quotaWatcher) checkpoint(ctx context.Context) (string, error) {
	if checkpoint.Disabled(os.Getenv) {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := checkpoint.Capture(ctx, checkpoint.Request{
		Harness: w.harness, Cwd: w.cwd, PID: w.pid, Since: w.started, Reason: "quota",
		Note:        "Captured because this login reached its usage limit threshold.",
		Conductor:   checkpoint.ConductorRef{Project: w.project, SessionID: w.session},
		ResumedFrom: os.Getenv("CONDUCTOR_RESUMED_FROM"), Force: true,
	})
	if err != nil {
		return "", err
	}
	return res.Manifest.ID, nil
}
