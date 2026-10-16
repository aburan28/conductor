package delphi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"
)

// Engine is the delegation loop: flush pending requests into provider batches,
// poll the batches in flight, collect answers, retry what is worth retrying.
// Call Tick on a schedule; it does one pass and returns.
type Engine struct {
	Store     Store
	Providers Registry // providers that can call their API
	// FlushEvery submits a provider's pending requests once the oldest has
	// waited this long. 0 submits on every tick.
	FlushEvery time.Duration
	// FlushAt submits as soon as this many requests are pending for a
	// provider, whatever their age. 0 disables the count trigger.
	FlushAt int
	// MaxAttempts bounds resubmission of retryable failures (expired, missing,
	// transient errors). 1 means never resubmit.
	MaxAttempts int
	// StatusOf extracts the HTTP status from a provider error, 0 if none.
	StatusOf func(error) int
	OnResult func(Result)
	Log      func(format string, a ...any)
	Now      func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logf(format string, a ...any) {
	if e.Log != nil {
		e.Log(format, a...)
	}
}

func newLabel(now time.Time) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "delphi-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// TickReport says what one Tick did.
type TickReport struct {
	Submitted, Collected, Requeued, Finished int
	Stuck                                    []string // flights needing `delphi reconcile`
}

// Tick runs one pass: collect finished flights first, then flush the queue.
// force submits everything pending regardless of the flush policy.
func (e *Engine) Tick(ctx context.Context, force bool) (TickReport, error) {
	var rep TickReport
	flights, err := e.Store.Flights()
	if err != nil {
		return rep, err
	}
	for _, f := range flights {
		if err := e.advance(ctx, f, &rep); err != nil {
			if errors.Is(err, context.Canceled) {
				return rep, err
			}
			e.logf("flight %s (%s %s): %v", f.Label, f.Provider, f.BatchID, err)
		}
	}
	return rep, e.flush(ctx, force, &rep)
}

func (e *Engine) advance(ctx context.Context, f Flight, rep *TickReport) error {
	prov, ok := e.Providers[f.Provider]
	if !ok {
		return fmt.Errorf("provider %s is not configured (missing credentials?)", f.Provider)
	}
	if f.State == FlightSubmitting {
		// A previous submit's outcome is unknown. Never resubmit blindly:
		// look the batch up by label, or leave it for an operator.
		id, found, err := prov.Find(ctx, f.Label)
		switch {
		case errors.Is(err, ErrNotSupported):
			rep.Stuck = append(rep.Stuck, f.Label)
			return fmt.Errorf("submission outcome unknown and %s batches cannot be looked up; run `delphi reconcile`", f.Provider)
		case err != nil:
			return err
		case !found:
			rep.Stuck = append(rep.Stuck, f.Label)
			return errors.New("submission outcome unknown and no batch carries its label yet; run `delphi reconcile` if it never appears")
		}
		f.BatchID, f.State = id, FlightRunning
		if err := e.Store.UpdateFlight(f); err != nil {
			return err
		}
		e.Store.Event("recovered", map[string]any{"label": f.Label, "batch": id})
	}
	st, err := prov.Poll(ctx, f.BatchID)
	if err != nil {
		return err
	}
	f.Status = st
	if !st.Ended {
		return e.Store.UpdateFlight(f)
	}
	return e.collect(ctx, prov, f, rep)
}

func (e *Engine) collect(ctx context.Context, prov Provider, f Flight, rep *TickReport) error {
	got := map[string]Result{}
	if f.Status.Failure == "" {
		err := prov.Results(ctx, f.BatchID, func(r Result) error {
			got[r.ID] = r
			return nil
		})
		if err != nil {
			return err
		}
	}
	now := e.now()
	var done []Result
	var retries []Request
	for _, req := range f.Requests {
		r, ok := got[req.ID]
		switch {
		case f.Status.Failure != "":
			// The provider rejected the whole batch; the same input fails again.
			r = Result{ID: req.ID, Status: Errored, Error: &Error{Type: "batch_failed", Message: f.Status.Failure}}
		case !ok:
			r = Result{ID: req.ID, Status: Missing}
		}
		r.Provider, r.BatchID, r.Attempt, r.Meta, r.Hash = f.Provider, f.BatchID, req.Attempt+1, req.Meta, Hash(req)
		if r.Model == "" {
			r.Model = req.Model
		}
		if r.Retryable() && req.Attempt+1 < max(e.MaxAttempts, 1) {
			retry := req
			retry.Attempt++
			retries = append(retries, retry)
			continue
		}
		r.FinishedAt = now.UTC().Format(time.RFC3339)
		done = append(done, r)
	}
	if err := e.Store.FinishFlight(f, done, retries, now); err != nil {
		return err
	}
	e.Store.Event("collected", map[string]any{"label": f.Label, "batch": f.BatchID, "provider": f.Provider,
		"state": f.Status.State, "finished": len(done), "requeued": len(retries)})
	e.logf("collected %s %s (%s): %d finished, %d requeued", f.Provider, f.BatchID, f.Status.State, len(done), len(retries))
	rep.Collected++
	rep.Finished += len(done)
	rep.Requeued += len(retries)
	for _, r := range done {
		if e.OnResult != nil {
			e.OnResult(r)
		}
	}
	return nil
}

func (e *Engine) flush(ctx context.Context, force bool, rep *TickReport) error {
	pending, err := e.Store.Pending()
	if err != nil {
		return err
	}
	byProv := map[string][]Queued{}
	for _, q := range pending {
		byProv[q.Provider] = append(byProv[q.Provider], q)
	}
	names := make([]string, 0, len(byProv))
	for n := range byProv {
		names = append(names, n)
	}
	sort.Strings(names)
	now := e.now()
	for _, name := range names {
		qs := byProv[name]
		prov, ok := e.Providers[name]
		if !ok {
			e.logf("%d request(s) wait for provider %s, which is not configured (missing credentials?)", len(qs), name)
			continue
		}
		due := force || e.FlushEvery == 0 || now.Sub(qs[0].EnqueuedAt) >= e.FlushEvery || (e.FlushAt > 0 && len(qs) >= e.FlushAt)
		if !due {
			continue
		}
		reqs := make([]Request, len(qs))
		for i, q := range qs {
			reqs[i] = q.Request
		}
		chunks, err := Chunk(reqs, prov.Limits())
		if err != nil {
			return err
		}
		for _, ch := range chunks {
			if err := e.submit(ctx, prov, ch, rep); err != nil {
				return err
			}
		}
	}
	return nil
}

// definitelyNotCreated reports whether a failed submit cannot have created a
// batch: the provider answered with an error status.
func (e *Engine) definitelyNotCreated(err error) (status int, sure bool) {
	if e.StatusOf != nil {
		if s := e.StatusOf(err); s != 0 {
			return s, true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && !ne.Timeout() {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return 0, true // never connected
		}
	}
	return 0, false
}

func (e *Engine) submit(ctx context.Context, prov Provider, reqs []Request, rep *TickReport) error {
	now := e.now()
	f := Flight{Label: newLabel(now), Provider: prov.Name(), State: FlightSubmitting, SubmittedAt: now, Requests: reqs}
	// Record the intent first: if we crash during the call, the flight shows
	// a submission whose outcome must be reconciled, not repeated.
	if err := e.Store.BeginFlight(f); err != nil {
		return err
	}
	id, err := prov.Submit(ctx, reqs, f.Label)
	if err != nil {
		status, sure := e.definitelyNotCreated(err)
		if !sure {
			e.logf("submit to %s failed with unknown outcome (%v); flight %s waits for reconciliation", prov.Name(), err, f.Label)
			rep.Stuck = append(rep.Stuck, f.Label)
			return nil
		}
		if status >= 400 && status < 500 && status != 408 && status != 409 && status != 429 {
			// Rejected outright (bad model, auth, malformed): retrying the same
			// requests would fail the same way, so they finish as errors.
			var done []Result
			for _, r := range reqs {
				done = append(done, Result{ID: r.ID, Provider: prov.Name(), Model: r.Model, Status: Errored, Attempt: r.Attempt + 1,
					Meta: r.Meta, Hash: Hash(r), FinishedAt: now.UTC().Format(time.RFC3339),
					Error: &Error{Type: fmt.Sprintf("submit_http_%d", status), Message: err.Error()}})
			}
			e.logf("submit to %s rejected (HTTP %d): %v", prov.Name(), status, err)
			if ferr := e.Store.FinishFlight(f, done, nil, now); ferr != nil {
				return ferr
			}
			for _, r := range done {
				if e.OnResult != nil {
					e.OnResult(r)
				}
			}
			rep.Finished += len(done)
			return nil
		}
		e.logf("submit to %s failed (%v); %d request(s) stay queued", prov.Name(), err, len(reqs))
		return e.Store.AbortFlight(f)
	}
	f.BatchID, f.State = id, FlightRunning
	if err := e.Store.UpdateFlight(f); err != nil {
		return err
	}
	e.Store.Event("submitted", map[string]any{"label": f.Label, "batch": id, "provider": prov.Name(), "requests": len(reqs)})
	e.logf("submitted %d request(s) to %s as %s", len(reqs), prov.Name(), id)
	rep.Submitted += len(reqs)
	return nil
}

// Reconcile resolves a flight stuck in FlightSubmitting: adopt binds it to a
// batch the operator found; requeue declares that no batch was created.
func Reconcile(s Store, label, adoptBatchID string, requeue bool) error {
	flights, err := s.Flights()
	if err != nil {
		return err
	}
	for _, f := range flights {
		if f.Label != label {
			continue
		}
		if f.State != FlightSubmitting {
			return fmt.Errorf("flight %s is %s, not stuck", label, f.State)
		}
		switch {
		case adoptBatchID != "":
			f.BatchID, f.State = adoptBatchID, FlightRunning
			s.Event("adopted", map[string]any{"label": label, "batch": adoptBatchID})
			return s.UpdateFlight(f)
		case requeue:
			s.Event("requeued_unsubmitted", map[string]any{"label": label})
			return s.AbortFlight(f)
		}
		return errors.New("give a batch id to adopt, or -requeue")
	}
	return fmt.Errorf("no flight %s", label)
}
