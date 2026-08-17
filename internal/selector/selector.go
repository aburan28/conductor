// Package selector answers two questions about the sessions that are live right now:
// what can this project currently do, and which session should take this piece of work.
//
// It is the session-facing half of routing. internal/router decides what a task *needs* — a
// tier, an effort, a capability floor — from the shape of the task. This package decides
// which of the sessions actually connected can meet that floor, and ranks them. Keeping the
// two apart is what lets the same requirement be satisfied by a Claude session on a laptop,
// a Codex session on a workstation, or a runner-launched attempt, without the router knowing
// any of them exist.
//
// Everything here is deterministic and dependency-free: same candidates and same requirement
// produce the same choice, every time, with a rationale that explains it. A model may ask for
// a capability floor; it can never argue its way past one.
package selector

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Candidate is one session considered for work.
type Candidate struct {
	SessionID    domain.ID
	Principal    string
	PrincipalID  domain.ID
	Harness      string
	State        domain.SessionState
	Capabilities domain.SessionCapabilities
	// Busy is true when the session is already on a task; Queued counts offers it has not
	// answered yet. Neither disqualifies a session, they only sort it down — a frontier
	// session that is busy is often still the right destination for work nothing else can do.
	Busy          bool
	Queued        int
	MachineID     string
	LastHeartbeat time.Time
}

// Choice is the outcome of a selection.
type Choice struct {
	SessionID domain.ID `json:"session_id"`
	Principal string    `json:"principal"`
	Harness   string    `json:"harness"`
	Model     string    `json:"model,omitempty"`
	Tier      domain.Tier
	Effort    domain.Effort
	Rationale []string   `json:"rationale"`
	Rejected  []Reject   `json:"rejected,omitempty"`
	Runners   []Fallback `json:"runners,omitempty"`
}

// Reject records why a live session could not take the work. This is the difference between
// "nothing is available" and "three sessions are up, none of them can think that hard" — the
// second is actionable, the first is not.
type Reject struct {
	SessionID domain.ID `json:"session_id"`
	Principal string    `json:"principal,omitempty"`
	Reason    string    `json:"reason"`
}

// Fallback names an offline path that could satisfy the requirement — a runner able to launch
// a fresh harness at the required model. It is reported, never chosen: dispatching to a runner
// is the scheduler's job, not the selector's.
type Fallback struct {
	Name   string   `json:"name"`
	Models []string `json:"models,omitempty"`
}

// Meets reports whether a candidate satisfies every floor in the requirement, and if not, why.
//
// The reason string is part of the contract. Every rejection here becomes something a person
// or an agent reads when work cannot be placed, so it names the floor and the gap.
func Meets(c Candidate, req domain.CapabilityRequirement) (bool, string) {
	caps := c.Capabilities

	if req.ExcludeSession != "" && c.SessionID == req.ExcludeSession {
		return false, "excluded: this is the session handing the work off"
	}
	if !c.State.Accepting() {
		return false, fmt.Sprintf("session is %s, not accepting work", c.State)
	}
	if req.Harness != "" && !strings.EqualFold(c.Harness, req.Harness) {
		return false, fmt.Sprintf("harness is %s, requires %s", c.Harness, req.Harness)
	}
	if req.Model != "" && !strings.EqualFold(caps.Model, req.Model) {
		return false, fmt.Sprintf("model is %s, requires %s", orUnknown(caps.Model), req.Model)
	}
	if !caps.ServesRole(req.Role) {
		return false, fmt.Sprintf("does not serve the %s role", req.Role)
	}
	if req.Tier != "" {
		if caps.Tier == "" {
			return false, fmt.Sprintf("declared no tier (model %s is not in the catalog), requires %s",
				orUnknown(caps.Model), req.Tier)
		}
		if !caps.Tier.AtLeast(req.Tier) {
			return false, fmt.Sprintf("tier %s is below the required %s", caps.Tier, req.Tier)
		}
	}
	if req.Effort != "" {
		ceiling := caps.Ceiling()
		if ceiling == "" {
			return false, fmt.Sprintf("declared no reasoning effort, requires %s", req.Effort)
		}
		if !ceiling.AtLeast(req.Effort) {
			return false, fmt.Sprintf("tops out at effort %s, requires %s", ceiling, req.Effort)
		}
	}
	if len(req.Capabilities) > 0 && !caps.HasCapabilities(req.Capabilities) {
		return false, fmt.Sprintf("missing capability from %s", strings.Join(req.Capabilities, ", "))
	}
	if req.ContextWindow > 0 && caps.ContextWindow < req.ContextWindow {
		return false, fmt.Sprintf("context window %d is below the required %d",
			caps.ContextWindow, req.ContextWindow)
	}
	if caps.MaxQueue > 0 && c.Queued >= caps.MaxQueue {
		return false, fmt.Sprintf("already holds %d offers, its limit", c.Queued)
	}
	return true, ""
}

// Select picks the session that should take the work.
//
// Ranking, in order: least loaded first, then the *cheapest* session that still clears every
// floor. Preferring the cheapest qualifying session rather than the most capable one is the
// point — the floor already guarantees capability, so spending a frontier session on work a
// mid-tier one can do just makes the frontier session unavailable when something actually
// needs it.
func Select(cands []Candidate, req domain.CapabilityRequirement) (Choice, error) {
	var eligible []Candidate
	var rejected []Reject

	for _, c := range cands {
		if ok, reason := Meets(c, req); ok {
			eligible = append(eligible, c)
		} else {
			rejected = append(rejected, Reject{SessionID: c.SessionID, Principal: c.Principal, Reason: reason})
		}
	}
	sort.SliceStable(rejected, func(i, j int) bool { return rejected[i].SessionID < rejected[j].SessionID })

	if len(eligible) == 0 {
		return Choice{Rejected: rejected}, fmt.Errorf(
			"%w: no live session meets %s (%d session(s) considered)",
			domain.ErrCapacity, req.Describe(), len(cands))
	}

	sort.SliceStable(eligible, func(i, j int) bool { return less(eligible[i], eligible[j]) })
	best := eligible[0]

	choice := Choice{
		SessionID: best.SessionID,
		Principal: best.Principal,
		Harness:   best.Harness,
		Model:     best.Capabilities.Model,
		Tier:      best.Capabilities.Tier,
		Effort:    effortFor(best, req),
		Rejected:  rejected,
	}
	choice.Rationale = append(choice.Rationale,
		fmt.Sprintf("requirement: %s", req.Describe()),
		fmt.Sprintf("%d of %d live session(s) qualified", len(eligible), len(cands)),
		fmt.Sprintf("chose %s (%s on %s, tier %s, effort ceiling %s), %s",
			best.Principal, orUnknown(best.Capabilities.Model), best.Harness,
			orUnknown(string(best.Capabilities.Tier)), orUnknown(string(best.Capabilities.Ceiling())),
			loadNote(best)))
	return choice, nil
}

// less orders eligible candidates. Idle beats busy, fewer queued offers beats more, cheaper
// tier beats more capable, then a stable tie-break on session id so the same input always
// produces the same choice.
func less(a, b Candidate) bool {
	if a.Busy != b.Busy {
		return !a.Busy
	}
	if a.Queued != b.Queued {
		return a.Queued < b.Queued
	}
	if at, bt := tierRank(a.Capabilities.Tier), tierRank(b.Capabilities.Tier); at != bt {
		return at < bt
	}
	if ae, be := effortRank(a.Capabilities.Ceiling()), effortRank(b.Capabilities.Ceiling()); ae != be {
		return ae < be
	}
	if !a.LastHeartbeat.Equal(b.LastHeartbeat) {
		return a.LastHeartbeat.After(b.LastHeartbeat)
	}
	return a.SessionID < b.SessionID
}

// effortFor is the effort the receiving session should run this work at: the requirement's
// floor when it names one, otherwise whatever the session is already doing.
func effortFor(c Candidate, req domain.CapabilityRequirement) domain.Effort {
	if req.Effort != "" {
		return req.Effort
	}
	return c.Capabilities.Effort
}

func loadNote(c Candidate) string {
	switch {
	case c.Busy && c.Queued > 0:
		return fmt.Sprintf("busy with %d offer(s) queued", c.Queued)
	case c.Busy:
		return "busy but the cheapest qualifying session"
	case c.Queued > 0:
		return fmt.Sprintf("idle with %d offer(s) queued", c.Queued)
	default:
		return "idle"
	}
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

// Inventory is what this project can do right now, rolled up across live sessions.
type Inventory struct {
	Sessions   int             `json:"sessions"`
	Available  int             `json:"available"`
	MaxTier    domain.Tier     `json:"max_tier,omitempty"`
	MaxEffort  domain.Effort   `json:"max_reasoning_effort,omitempty"`
	Harnesses  []string        `json:"harnesses"`
	Models     []ModelPresence `json:"models"`
	Tiers      map[string]int  `json:"tiers"`
	Efforts    map[string]int  `json:"reasoning_efforts"`
	Capability []string        `json:"capabilities"`
	Runners    []Fallback      `json:"runners,omitempty"`
	Gaps       []string        `json:"gaps,omitempty"`
}

// ModelPresence is one model currently being driven, and by how many sessions.
type ModelPresence struct {
	Model     string        `json:"model"`
	Harness   string        `json:"harness"`
	Tier      domain.Tier   `json:"tier,omitempty"`
	Ceiling   domain.Effort `json:"max_reasoning_effort,omitempty"`
	Sessions  int           `json:"sessions"`
	Available int           `json:"available"`
}

// Aggregate rolls candidates up into an inventory.
//
// The counts distinguish present from available on purpose. A session that is blocked or
// waiting on its human is visible in presence but cannot be handed anything, and reporting it
// as capacity would promise something Conductor cannot deliver.
func Aggregate(cands []Candidate) Inventory {
	inv := Inventory{
		Sessions: len(cands),
		Tiers:    map[string]int{},
		Efforts:  map[string]int{},
	}

	harnesses := map[string]bool{}
	tags := map[string]bool{}
	byModel := map[string]*ModelPresence{}

	for _, c := range cands {
		available := c.State.Accepting()
		if available {
			inv.Available++
		}
		if c.Harness != "" {
			harnesses[c.Harness] = true
		}
		caps := c.Capabilities
		if caps.Tier != "" {
			inv.Tiers[string(caps.Tier)]++
			if available && tierRank(caps.Tier) > tierRank(inv.MaxTier) {
				inv.MaxTier = caps.Tier
			}
		}
		if ceiling := caps.Ceiling(); ceiling != "" {
			inv.Efforts[string(ceiling)]++
			if available && effortRank(ceiling) > effortRank(inv.MaxEffort) {
				inv.MaxEffort = ceiling
			}
		}
		for _, tag := range caps.Capabilities {
			tags[tag] = true
		}

		key := c.Harness + "/" + caps.Model
		entry, ok := byModel[key]
		if !ok {
			entry = &ModelPresence{
				Model: orUnknown(caps.Model), Harness: c.Harness,
				Tier: caps.Tier, Ceiling: caps.Ceiling(),
			}
			byModel[key] = entry
		}
		entry.Sessions++
		if available {
			entry.Available++
		}
	}

	inv.Harnesses = sortedKeys(harnesses)
	inv.Capability = sortedKeys(tags)
	for _, key := range sortedKeys(mapKeys(byModel)) {
		inv.Models = append(inv.Models, *byModel[key])
	}
	if inv.Models == nil {
		inv.Models = []ModelPresence{}
	}
	return inv
}

// Serves reports whether the inventory can satisfy a requirement, and what is missing if not.
// This is what lets a coordinator ask "can anyone here do xhigh on a T4 model" before it
// designs work that depends on the answer.
func Serves(cands []Candidate, req domain.CapabilityRequirement) (bool, []string) {
	var reasons []string
	for _, c := range cands {
		if ok, _ := Meets(c, req); ok {
			return true, nil
		}
	}
	inv := Aggregate(cands)
	if req.Tier != "" && (inv.MaxTier == "" || !inv.MaxTier.AtLeast(req.Tier)) {
		reasons = append(reasons, fmt.Sprintf("highest available tier is %s, needs %s",
			orUnknown(string(inv.MaxTier)), req.Tier))
	}
	if req.Effort != "" && (inv.MaxEffort == "" || !inv.MaxEffort.AtLeast(req.Effort)) {
		reasons = append(reasons, fmt.Sprintf("highest available reasoning effort is %s, needs %s",
			orUnknown(string(inv.MaxEffort)), req.Effort))
	}
	if reasons == nil {
		reasons = append(reasons, "sessions with the required capability exist but none are accepting work")
	}
	return false, reasons
}

func tierRank(t domain.Tier) int {
	for i, known := range []domain.Tier{domain.TierT0, domain.TierT1, domain.TierT2, domain.TierT3, domain.TierT4} {
		if t == known {
			return i
		}
	}
	return -1
}

func effortRank(e domain.Effort) int {
	for i, known := range domain.AllEfforts {
		if e == known {
			return i
		}
	}
	return -1
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func sortedKeys(in any) []string {
	var out []string
	switch m := in.(type) {
	case map[string]bool:
		for k := range m {
			out = append(out, k)
		}
	case []string:
		out = append(out, m...)
	}
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}

func mapKeys(m map[string]*ModelPresence) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
