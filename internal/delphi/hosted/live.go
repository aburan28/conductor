package hosted

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aburan28/conductor/internal/delphi"
	"github.com/aburan28/conductor/internal/delphi/compat"
	"github.com/aburan28/conductor/internal/domain"
)

// Live is conductord's worker for OpenAI-compatible endpoints (OpenRouter, Fireworks,
// DeepSeek, Together, vLLM, ...). Those answer once and keep nothing, so the API cannot
// hand them a request and poll later the way it does a batch API, and holding the API
// request open for the whole call would tie it to a model's latency.
//
// Submit therefore records the delegation and hands the request to this worker, which
// makes the call in the background, at most Concurrency at a time. DESIGN.md §12.4 holds
// throughout: the prompt is only ever in this process's memory, and so is the answer, kept
// for AnswerTTL for `conductor delphi answer`. A restart loses both; the loop then marks the
// row missing once it stops being heartbeated, rather than pretend it can resume.
type Live struct {
	// Endpoints are the operator's: presets plus the server's endpoints file. A member
	// never supplies a base URL.
	Endpoints map[string]compat.Endpoint
	// Holder names this process in the rows it holds. Defaults to host:pid:random.
	Holder      string
	Concurrency int           // calls in flight at once (default 8)
	AnswerTTL   time.Duration // how long an answer is kept for fetching (default 24h)
	Attempts    int           // tries per call for retryable failures (default 3)

	once    sync.Once
	sem     chan struct{}
	mu      sync.Mutex
	open    map[domain.ID]context.CancelFunc
	answers map[domain.ID]heldAnswer
	wg      sync.WaitGroup
}

type heldAnswer struct {
	result delphi.Result
	at     time.Time
}

// LiveStaleAfter is how long an open live delegation may go without a heartbeat before the
// loop declares it lost.
const LiveStaleAfter = 5 * time.Minute

const liveHeartbeat = 30 * time.Second

func (l *Live) init() {
	l.once.Do(func() {
		if l.Holder == "" {
			host, _ := os.Hostname()
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			l.Holder = fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b))
		}
		if l.Concurrency <= 0 {
			l.Concurrency = 8
		}
		if l.AnswerTTL <= 0 {
			l.AnswerTTL = 24 * time.Hour
		}
		if l.Attempts <= 0 {
			l.Attempts = 3
		}
		l.sem = make(chan struct{}, l.Concurrency)
		l.open = map[domain.ID]context.CancelFunc{}
		l.answers = map[domain.ID]heldAnswer{}
	})
}

// Has reports whether kind is one of the operator's live endpoints.
func (l *Live) Has(kind string) bool {
	if l == nil {
		return false
	}
	_, ok := l.Endpoints[kind]
	return ok
}

// Names lists the live endpoints, sorted.
func (l *Live) Names() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.Endpoints))
	for n := range l.Endpoints {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// offline adds the live endpoints to the validation registry.
func (l *Live) offline(reg delphi.Registry) delphi.Registry {
	if l == nil {
		return reg
	}
	out := delphi.Registry{}
	for k, v := range reg {
		out[k] = v
	}
	for name, ep := range l.Endpoints {
		out[name] = compat.Offline(ep)
	}
	return out
}

// start runs one delegation's call in the background. finish records the outcome; it is
// called exactly once, after the answer (if any) is held.
func (l *Live) start(ctx context.Context, d domain.DelphiDelegation, prov *compat.Provider, req delphi.Request,
	finish func(context.Context, domain.DelphiDelegation, delphi.Result)) {
	l.init()
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l.mu.Lock()
	l.open[d.ID] = cancel
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer cancel()
		var res delphi.Result
		select {
		case l.sem <- struct{}{}:
			res = l.call(callCtx, prov, req)
			<-l.sem
		case <-callCtx.Done():
			res = delphi.Result{ID: req.ID, Status: delphi.Canceled}
		}
		if callCtx.Err() != nil && res.Status != delphi.Succeeded {
			res = delphi.Result{ID: req.ID, Status: delphi.Canceled, Usage: res.Usage}
		}
		l.mu.Lock()
		delete(l.open, d.ID)
		if res.Status == delphi.Succeeded || res.Status == delphi.Refused {
			l.answers[d.ID] = heldAnswer{result: res, at: time.Now()}
		}
		l.mu.Unlock()
		finish(context.WithoutCancel(ctx), d, res)
	}()
}

// call retries retryable failures with a short backoff.
func (l *Live) call(ctx context.Context, prov *compat.Provider, req delphi.Request) delphi.Result {
	var res delphi.Result
	for attempt := 0; attempt < l.Attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return res
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		res = prov.Call(ctx, req)
		if !res.Retryable() || ctx.Err() != nil {
			return res
		}
	}
	return res
}

// cancel stops a call this process holds; it reports false if it holds none.
func (l *Live) cancel(id domain.ID) bool {
	if l == nil {
		return false
	}
	l.init()
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.open[id]
	if ok {
		c()
	}
	return ok
}

// answer returns a held answer.
func (l *Live) answer(id domain.ID) (delphi.Result, bool) {
	if l == nil {
		return delphi.Result{}, false
	}
	l.init()
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.answers[id]
	if ok && time.Since(a.at) > l.AnswerTTL {
		delete(l.answers, id)
		return delphi.Result{}, false
	}
	return a.result, ok
}

// openIDs lists the delegations whose calls are in flight here.
func (l *Live) openIDs() []domain.ID {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]domain.ID, 0, len(l.open))
	for id := range l.open {
		out = append(out, id)
	}
	return out
}

// sweep drops answers older than AnswerTTL.
func (l *Live) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, a := range l.answers {
		if time.Since(a.at) > l.AnswerTTL {
			delete(l.answers, id)
		}
	}
}

// RunLive heartbeats the calls this process holds and drops expired answers until ctx
// ends; then it cancels what is still in flight and waits for it to be recorded.
func (s *Service) RunLive(ctx context.Context) {
	l := s.Live
	if l == nil {
		return
	}
	l.init()
	t := time.NewTicker(liveHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, id := range l.openIDs() {
				l.cancel(id)
			}
			l.wg.Wait()
			return
		case <-t.C:
		}
		if err := s.Store.HeartbeatLiveDelphi(ctx, l.openIDs()); err != nil && ctx.Err() == nil {
			s.log().Warn("delphi live: heartbeat failed", "error", err)
		}
		l.sweep()
	}
}

// liveProvider builds the provider for a live endpoint with the project's key: the vault
// secret named by the endpoint's key variable. A keyless endpoint is one the operator ran.
func (s *Service) liveProvider(ctx context.Context, project domain.Project, kind string) (*compat.Provider, error) {
	ep := s.Live.Endpoints[kind]
	if ep.KeyEnv == "" {
		return compat.NewWithKey(ep, ""), nil
	}
	if s.Key == nil {
		return nil, fmt.Errorf("%w: the secret vault is not enabled on this server", domain.ErrInvalidArgument)
	}
	key, err := s.Key(ctx, project, ep.KeyEnv)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("%w: project secret %s is not set; run `conductor secrets set %s`", domain.ErrInvalidArgument, ep.KeyEnv, ep.KeyEnv)
	}
	if err != nil {
		return nil, err
	}
	return compat.NewWithKey(ep, key), nil
}

// submitLive records a live delegation and starts its call. Nothing about the request is
// stored but its hash; the request itself goes to the worker in memory.
func (s *Service) submitLive(ctx context.Context, project domain.Project, row domain.DelphiDelegation, req delphi.Request) (domain.DelphiDelegation, bool, error) {
	prov, err := s.liveProvider(ctx, project, req.Provider)
	if err != nil {
		return row, false, err
	}
	s.Live.init()
	row.Live, row.Holder = true, s.Live.Holder
	d, err := s.Store.CreateDelphiDelegation(ctx, row)
	if errors.Is(err, domain.ErrDuplicate) {
		if d.BodySHA256 == row.BodySHA256 {
			return d, false, nil
		}
		return domain.DelphiDelegation{}, false, fmt.Errorf("%w: ref %q is already used by a different request", domain.ErrInvalidArgument, req.ID)
	}
	if err != nil {
		return d, false, err
	}
	// The label stands in for the batch id: a live call has no provider-side handle.
	if d, err = s.Store.SetDelphiRunning(context.WithoutCancel(ctx), d.ID, d.Label); err != nil {
		return d, false, err
	}
	s.Live.start(ctx, d, prov, req, func(ctx context.Context, d domain.DelphiDelegation, res delphi.Result) {
		if err := s.finish(ctx, project, prov, d, res); err != nil {
			s.log().Warn("delphi live: outcome not recorded", "project", project.ID, "ref", d.Ref, "error", err)
		}
	})
	return d, true, nil
}
