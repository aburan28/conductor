package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/domain"
)

// lockedPlane records requests from more than one goroutine (the tool caller and the lease
// keeper), which the plain stub does not need to.
type lockedPlane struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	status   int // response status for /v1/leases/heartbeat; 0 means 200
}

func newLockedPlane(t *testing.T) *lockedPlane {
	t.Helper()
	p := &lockedPlane{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.mu.Lock()
		p.requests = append(p.requests, recorded{r.Method, r.URL.Path, body})
		status := p.status
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/leases/heartbeat" && status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"lease has expired","code":"lease_not_held"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *lockedPlane) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.requests {
		if r.Path == path {
			n++
		}
	}
	return n
}

func (p *lockedPlane) last(path string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.requests) - 1; i >= 0; i-- {
		if p.requests[i].Path == path {
			return p.requests[i].Body
		}
	}
	return nil
}

// A claimed lease is renewed in the background, with no tool call from the model, and the
// keeper stops once the server says the claim is gone.
func TestLeaseKeeperRenewsWithoutToolCalls(t *testing.T) {
	plane := newLockedPlane(t)
	s := &Server{api: client.New(plane.URL, "tok"), project: "demo", keepPoll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.keepLeaseAlive(ctx, nil)

	time.Sleep(50 * time.Millisecond)
	if n := plane.count("/v1/leases/heartbeat"); n != 0 {
		t.Fatalf("heartbeats with no claim = %d", n)
	}

	s.setFence(domain.Fence{TaskID: "t-1", AttemptID: "a-1", LeaseID: "l-1", FencingEpoch: 3})
	deadline := time.Now().Add(2 * time.Second)
	for plane.count("/v1/leases/heartbeat") < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := plane.count("/v1/leases/heartbeat"); n < 3 {
		t.Fatalf("heartbeats = %d, want the lease renewed repeatedly", n)
	}
	if body := plane.last("/v1/leases/heartbeat"); body["lease_id"] != "l-1" || body["fencing_epoch"] != float64(3) {
		t.Errorf("heartbeat body = %v", body)
	}

	plane.mu.Lock()
	plane.status = http.StatusConflict
	plane.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	stopped := plane.count("/v1/leases/heartbeat")
	time.Sleep(100 * time.Millisecond)
	if n := plane.count("/v1/leases/heartbeat"); n != stopped {
		t.Errorf("the keeper kept renewing a lost lease (%d -> %d)", stopped, n)
	}
}

// The keeper honours a liveness check: an HTTP session whose client went quiet stops
// holding its claim.
func TestLeaseKeeperStopsWhenTheSessionIsIdle(t *testing.T) {
	plane := newLockedPlane(t)
	s := &Server{api: client.New(plane.URL, "tok"), project: "demo", keepPoll: 10 * time.Millisecond}
	s.setFence(domain.Fence{TaskID: "t-1", AttemptID: "a-1", LeaseID: "l-1", FencingEpoch: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.keepLeaseAlive(ctx, func() bool { return false })
	time.Sleep(80 * time.Millisecond)
	if n := plane.count("/v1/leases/heartbeat"); n != 0 {
		t.Errorf("an idle session's lease was renewed %d times", n)
	}
}

// coord_publish_result used to build the command list and commit and then send neither.
func TestPublishResultSendsCommandsAndCommit(t *testing.T) {
	plane := newLockedPlane(t)
	s := &Server{api: client.New(plane.URL, "tok"), project: "demo"}
	s.setFence(domain.Fence{TaskID: "t-1", AttemptID: "a-1", LeaseID: "l-1", FencingEpoch: 2})

	if text, isErr := call(t, s, "coord_publish_result", map[string]any{
		"commit_sha":    "abc123",
		"changed_paths": []string{"internal/x.go"},
		"commands": []map[string]any{
			{"command": "go test ./...", "exit_code": 0},
			{"command": "go vet ./...", "exit_code": 1},
		},
	}); isErr {
		t.Fatalf("publish errored: %s", text)
	}
	body := plane.last("/v1/attempts/a-1/evidence")
	if body == nil {
		t.Fatal("no evidence was sent")
	}
	if body["commit_sha"] != "abc123" || body["lease_id"] != "l-1" || body["fencing_epoch"] != float64(2) {
		t.Errorf("evidence body = %v", body)
	}
	cmds, _ := body["commands"].([]any)
	if len(cmds) != 2 {
		t.Fatalf("commands = %v, want both", body["commands"])
	}
	second, _ := cmds[1].(map[string]any)
	if second["command"] != "go vet ./..." || second["exit_code"] != float64(1) {
		t.Errorf("second command = %v", second)
	}
}

// start_work binds the claim to the wrapping session, whose heartbeat then keeps it alive.
func TestStartWorkBindsTheSession(t *testing.T) {
	plane := newLockedPlane(t)
	s := &Server{api: client.New(plane.URL, "tok"), project: "demo", session: "sess-1"}
	call(t, s, "coord_start_work", map[string]any{"summary": "x"})
	if body := plane.last("/v1/projects/demo/work/start"); body == nil || body["session_id"] != "sess-1" {
		t.Errorf("start_work body = %v, want session_id", body)
	}
}
