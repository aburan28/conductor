package selector

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

func candidate(id, principal, harness, model string, tier domain.Tier, effort, ceiling domain.Effort) Candidate {
	return Candidate{
		SessionID: id,
		Principal: principal,
		Harness:   harness,
		State:     domain.SessionOnlineIdle,
		Capabilities: domain.SessionCapabilities{
			Model: model, Tier: tier, Effort: effort, MaxEffort: ceiling, Resolved: tier != "",
		},
		LastHeartbeat: time.Unix(1000, 0),
	}
}

// The whole feature turns on this: a floor is a floor. An idle cheap session must not win a
// selection that asked for xhigh on a frontier model just because it was free.
func TestSelectRefusesToDowngradeBelowTheFloor(t *testing.T) {
	cheap := candidate("s1", "alice", "claude", "haiku", domain.TierT1, domain.EffortLow, domain.EffortMedium)
	strong := candidate("s2", "rachel", "claude", "opus", domain.TierT4, domain.EffortMedium, domain.EffortXHigh)
	strong.Busy = true

	choice, err := Select([]Candidate{cheap, strong}, domain.CapabilityRequirement{
		Tier: domain.TierT4, Effort: domain.EffortXHigh,
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if choice.SessionID != "s2" {
		t.Fatalf("chose %s, want the session that meets the floor even though it is busy", choice.SessionID)
	}
	if choice.Effort != domain.EffortXHigh {
		t.Errorf("effort = %s, want the requirement's floor", choice.Effort)
	}
	if len(choice.Rejected) != 1 || choice.Rejected[0].SessionID != "s1" {
		t.Fatalf("rejections = %+v, want s1 explained", choice.Rejected)
	}
	if !strings.Contains(choice.Rejected[0].Reason, "xhigh") &&
		!strings.Contains(choice.Rejected[0].Reason, "tier") {
		t.Errorf("rejection reason %q names neither the tier nor the effort gap", choice.Rejected[0].Reason)
	}
}

// Above the floor, cheaper wins. Spending the frontier session on work a mid-tier one can do
// is how the frontier session ends up unavailable when something actually needs it.
func TestSelectPrefersTheCheapestQualifyingSession(t *testing.T) {
	mid := candidate("s2", "rachel", "claude", "sonnet", domain.TierT2, domain.EffortMedium, domain.EffortHigh)
	top := candidate("s3", "sam", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortMax)

	choice, err := Select([]Candidate{top, mid}, domain.CapabilityRequirement{Tier: domain.TierT2})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if choice.SessionID != "s2" {
		t.Errorf("chose %s, want the cheapest session that clears T2", choice.SessionID)
	}
}

func TestSelectPrefersIdleOverBusyAtEqualCapability(t *testing.T) {
	busy := candidate("s1", "alice", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)
	busy.Busy = true
	idle := candidate("s2", "rachel", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)

	choice, err := Select([]Candidate{busy, idle}, domain.CapabilityRequirement{Tier: domain.TierT4})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if choice.SessionID != "s2" {
		t.Errorf("chose %s, want the idle session", choice.SessionID)
	}
}

// A session that declared only its current effort cannot be asked for more. Assuming
// otherwise routes xhigh work to a session running at medium and quietly gets medium.
func TestCeilingDefaultsToTheRunningEffort(t *testing.T) {
	c := candidate("s1", "alice", "claude", "opus", domain.TierT4, domain.EffortMedium, "")
	if ok, reason := Meets(c, domain.CapabilityRequirement{Effort: domain.EffortXHigh}); ok {
		t.Fatal("a session running at medium with no declared ceiling must not satisfy xhigh")
	} else if !strings.Contains(reason, "tops out") {
		t.Errorf("reason = %q, want it to name the ceiling", reason)
	}
}

func TestUnresolvedModelCannotSatisfyATierFloor(t *testing.T) {
	c := candidate("s1", "alice", "claude", "some-local-model", "", domain.EffortMax, domain.EffortMax)
	ok, reason := Meets(c, domain.CapabilityRequirement{Tier: domain.TierT2})
	if ok {
		t.Fatal("a model the catalog never vouched for must not clear a tier floor")
	}
	if !strings.Contains(reason, "catalog") {
		t.Errorf("reason = %q, want it to explain the model is not in the catalog", reason)
	}
	// It is still usable for work that names no tier.
	if ok, reason := Meets(c, domain.CapabilityRequirement{Effort: domain.EffortHigh}); !ok {
		t.Errorf("unresolved session should still serve an effort-only floor: %s", reason)
	}
}

func TestSessionsNotAcceptingWorkAreExcluded(t *testing.T) {
	blocked := candidate("s1", "alice", "claude", "opus", domain.TierT4, domain.EffortXHigh, domain.EffortXHigh)
	blocked.State = domain.SessionWaitingForInput

	_, err := Select([]Candidate{blocked}, domain.CapabilityRequirement{Tier: domain.TierT4})
	if !errors.Is(err, domain.ErrCapacity) {
		t.Fatalf("err = %v, want ErrCapacity", err)
	}
}

func TestSelectExcludesTheDelegatingSession(t *testing.T) {
	self := candidate("s1", "alice", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)
	other := candidate("s2", "rachel", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)

	choice, err := Select([]Candidate{self, other}, domain.CapabilityRequirement{
		Tier: domain.TierT4, ExcludeSession: "s1",
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if choice.SessionID != "s2" {
		t.Errorf("chose %s, want the work handed away from s1", choice.SessionID)
	}
}

func TestSelectRespectsAQueueLimit(t *testing.T) {
	c := candidate("s1", "alice", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)
	c.Capabilities.MaxQueue = 1
	c.Queued = 1

	if _, err := Select([]Candidate{c}, domain.CapabilityRequirement{}); !errors.Is(err, domain.ErrCapacity) {
		t.Fatalf("err = %v, want a session at its offer limit to be skipped", err)
	}
}

func TestSelectIsDeterministic(t *testing.T) {
	a := candidate("s1", "alice", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)
	b := candidate("s2", "rachel", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortXHigh)
	req := domain.CapabilityRequirement{Tier: domain.TierT4}

	first, err := Select([]Candidate{a, b}, req)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	second, err := Select([]Candidate{b, a}, req)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if first.SessionID != second.SessionID {
		t.Errorf("selection depends on input order: %s vs %s", first.SessionID, second.SessionID)
	}
}

func TestAggregateReportsCeilingsFromAvailableSessionsOnly(t *testing.T) {
	idle := candidate("s1", "alice", "claude", "sonnet", domain.TierT2, domain.EffortMedium, domain.EffortHigh)
	blocked := candidate("s2", "rachel", "claude", "opus", domain.TierT4, domain.EffortHigh, domain.EffortMax)
	blocked.State = domain.SessionBlocked

	inv := Aggregate([]Candidate{idle, blocked})
	if inv.Sessions != 2 || inv.Available != 1 {
		t.Fatalf("sessions = %d available = %d, want 2 and 1", inv.Sessions, inv.Available)
	}
	if inv.MaxTier != domain.TierT2 {
		t.Errorf("max tier = %s, want T2: a blocked session is present, not available", inv.MaxTier)
	}
	if inv.MaxEffort != domain.EffortHigh {
		t.Errorf("max effort = %s, want high", inv.MaxEffort)
	}
	// The blocked session still shows in the model roll-up, with zero availability.
	var found bool
	for _, m := range inv.Models {
		if m.Model == "opus" {
			found = true
			if m.Sessions != 1 || m.Available != 0 {
				t.Errorf("opus presence = %+v, want 1 session and 0 available", m)
			}
		}
	}
	if !found {
		t.Error("the blocked session's model is missing from the roll-up")
	}
}

func TestServesExplainsTheGap(t *testing.T) {
	only := candidate("s1", "alice", "claude", "sonnet", domain.TierT2, domain.EffortMedium, domain.EffortHigh)

	ok, reasons := Serves([]Candidate{only}, domain.CapabilityRequirement{
		Tier: domain.TierT4, Effort: domain.EffortXHigh,
	})
	if ok {
		t.Fatal("T2 at high must not serve T4 at xhigh")
	}
	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "T2") || !strings.Contains(joined, "high") {
		t.Errorf("reasons = %q, want the actual ceiling named", joined)
	}
}

func TestResolveTakesTheCatalogsWordOverTheSessions(t *testing.T) {
	profiles := []domain.ModelProfile{{
		ID: "p1", Alias: "worker.general", Harness: "claude", Model: "claude-opus-5",
		Tier: domain.TierT4, Capabilities: []string{"architecture", "long_context"},
		ContextWindow: 200000, ReasoningEffort: domain.EffortHigh, Enabled: true,
	}}

	// The session asserts T0 and a capability it does not have; the catalog decides.
	declared := domain.SessionCapabilities{
		Model: "claude-opus-5", Effort: domain.EffortXHigh,
		Tier: domain.TierT0, Capabilities: []string{"invented"},
	}
	got := Resolve(declared, "claude", profiles)

	if got.Tier != domain.TierT4 {
		t.Errorf("tier = %s, want the catalog's T4", got.Tier)
	}
	if !got.Resolved || got.ProfileID != "p1" {
		t.Errorf("resolved = %v profile = %s, want the catalog entry", got.Resolved, got.ProfileID)
	}
	if got.HasCapabilities([]string{"invented"}) {
		t.Error("a session must not keep capability tags it asserted itself")
	}
	if got.Effort != domain.EffortXHigh {
		t.Errorf("effort = %s, want the declared xhigh preserved", got.Effort)
	}
	if got.ContextWindow != 200000 {
		t.Errorf("context window = %d, want the catalog's", got.ContextWindow)
	}
}

func TestResolveLeavesUnknownModelsUnresolved(t *testing.T) {
	got := Resolve(domain.SessionCapabilities{Model: "mystery-7b", Effort: domain.EffortLow},
		"opencode", []domain.ModelProfile{{
			ID: "p1", Alias: "worker.fast", Harness: "claude", Model: "haiku",
			Tier: domain.TierT1, Enabled: true,
		}})
	if got.Resolved || got.Tier != "" {
		t.Errorf("got %+v, want an unresolved record with no tier", got)
	}
	if got.Model != "mystery-7b" || got.Effort != domain.EffortLow {
		t.Errorf("got %+v, want the declaration preserved", got)
	}
}

func TestResolveInheritsTheProfileEffortWhenNoneDeclared(t *testing.T) {
	got := Resolve(domain.SessionCapabilities{Model: "haiku"}, "claude", []domain.ModelProfile{{
		ID: "p1", Alias: "worker.fast", Harness: "claude", Model: "haiku",
		Tier: domain.TierT1, ReasoningEffort: domain.EffortLow, Enabled: true,
	}})
	if got.Effort != domain.EffortLow {
		t.Errorf("effort = %s, want the profile default", got.Effort)
	}
}
