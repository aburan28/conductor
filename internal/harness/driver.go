// Package harness is the southbound execution interface: the common shape every coding
// agent runtime is driven through (DESIGN.md §16).
//
// The critical property of this package is where text stops. Harness stream adapters parse
// each runtime's native JSON events and emit only measurements — token counts, tool names,
// turn counts, exit status, cost. Assistant text, tool arguments, tool results, and reasoning
// are discarded inside the adapter, before anything can reach the store. That is why the
// control plane has nowhere to put a transcript: not because callers are careful, but because
// the boundary type has no field for one.
package harness

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Capabilities is what a harness advertises about itself (DESIGN.md §16.1).
type Capabilities struct {
	Kind           string `json:"kind"`
	Available      bool   `json:"available"`
	Version        string `json:"version,omitempty"`
	SupportsMCP    bool   `json:"supports_mcp"`
	SupportsEffort bool   `json:"supports_reasoning_effort"`
	SupportsJSON   bool   `json:"supports_structured_events"`
	SupportsResume bool   `json:"supports_resume"`
	Detail         string `json:"detail,omitempty"`
}

// RunSpec is the attempt specification handed to a driver (DESIGN.md §16.1).
//
// Instruction is the task card and workflow rules — a generated, auditable document, not a
// user's conversation. Nothing in this struct originates from another principal's chat.
type RunSpec struct {
	TaskRef       string
	TaskCardPath  string
	Instruction   string
	Cwd           string
	Branch        string
	BaseSHA       string
	Model         string
	Effort        domain.Effort
	Role          domain.AgentRole
	MaxTurns      int
	Timeout       time.Duration
	Env           []string
	MCPConfigPath string
	// PermissionMode and NetworkPolicy are enforced by the harness itself where it supports
	// them; the runner additionally constrains the process (§25.3).
	PermissionMode string
	NetworkPolicy  string
	Fence          domain.Fence
	WorkflowSHA    string
	// ExtraArgs is passed through verbatim, so an operator can adapt to a harness version
	// this build predates without waiting for a code change.
	ExtraArgs []string
}

// EventKind classifies a lifecycle event.
type EventKind string

const (
	EventStarted  EventKind = "started"
	EventTurn     EventKind = "turn"
	EventToolUse  EventKind = "tool_use"
	EventUsage    EventKind = "usage"
	EventStatus   EventKind = "status"
	EventError    EventKind = "error"
	EventFinished EventKind = "finished"
)

// Event is the sanitized lifecycle signal.
//
// Note the absence of a text field. `Tool` is a tool *name*; `Note` is adapter-generated
// (for example "exit code 1"), never harness output. Adding a free-text field here would
// undo the privacy boundary this package exists to hold.
type Event struct {
	Kind      EventKind `json:"kind"`
	Tool      string    `json:"tool,omitempty"`
	Turns     int       `json:"turns,omitempty"`
	TokensIn  int64     `json:"tokens_in,omitempty"`
	TokensOut int64     `json:"tokens_out,omitempty"`
	CostUSD   float64   `json:"cost_usd,omitempty"`
	Note      string    `json:"note,omitempty"`
	At        time.Time `json:"at"`
}

// Result is the terminal summary of a run.
type Result struct {
	ExitCode     int           `json:"exit_code"`
	Turns        int           `json:"turns"`
	TokensIn     int64         `json:"tokens_in"`
	TokensOut    int64         `json:"tokens_out"`
	CostUSD      float64       `json:"cost_usd"`
	Duration     time.Duration `json:"duration"`
	FailureClass string        `json:"failure_class,omitempty"`
	// Succeeded is the harness's own verdict. It is advisory: completion still requires
	// runner-attested validation evidence (DESIGN.md §25.5).
	Succeeded bool `json:"succeeded"`
}

// Handle controls a running attempt.
type Handle interface {
	Events() <-chan Event
	Wait(ctx context.Context) (Result, error)
	Cancel(ctx context.Context) error
}

// Driver launches attempts on one harness.
type Driver interface {
	Name() string
	Capabilities(ctx context.Context) Capabilities
	Start(ctx context.Context, spec RunSpec) (Handle, error)
}

// Registry holds the drivers this process can dispatch to.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

func NewRegistry() *Registry {
	return &Registry{drivers: map[string]Driver{}}
}

func (r *Registry) Register(d Driver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[d.Name()] = d
}

func (r *Registry) Get(name string) (Driver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[name]
	if !ok {
		return nil, fmt.Errorf("%w: no driver registered for harness %q", domain.ErrNotFound, name)
	}
	return d, nil
}

// Names lists registered harnesses, sorted for deterministic output.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.drivers))
	for name := range r.drivers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Available returns the harnesses that are actually installed and runnable, which is what the
// router filters candidate model profiles against.
func (r *Registry) Available(ctx context.Context) []string {
	var out []string
	for _, name := range r.Names() {
		d, err := r.Get(name)
		if err != nil {
			continue
		}
		if d.Capabilities(ctx).Available {
			out = append(out, name)
		}
	}
	return out
}

// CapabilityReport describes every registered driver, for `conductor doctor` and runner
// registration.
func (r *Registry) CapabilityReport(ctx context.Context) []Capabilities {
	var out []Capabilities
	for _, name := range r.Names() {
		if d, err := r.Get(name); err == nil {
			out = append(out, d.Capabilities(ctx))
		}
	}
	return out
}
