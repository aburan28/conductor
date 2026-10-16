package db

import (
	"errors"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

func TestDelphiDelegationLifecycle(t *testing.T) {
	f := newFixture(t)
	task := f.newTask(t, "delegated")
	in := domain.DelphiDelegation{OrganizationID: f.org.ID, ProjectID: f.project.ID, Ref: "r1",
		PrincipalID: f.alice.ID, TaskID: task.ID, Provider: "anthropic", Model: "claude-mythos-5-1",
		BodySHA256: "abc", Label: "L1", Labels: map[string]string{"k": "v"}}
	d, err := f.store.CreateDelphiDelegation(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != domain.DelphiSubmitting || d.Labels["k"] != "v" || d.TaskID != task.ID {
		t.Fatalf("created %+v", d)
	}

	// The same ref again: the existing row comes back with ErrDuplicate.
	again, err := f.store.CreateDelphiDelegation(f.ctx, in)
	if !errors.Is(err, domain.ErrDuplicate) || again.ID != d.ID {
		t.Fatalf("duplicate: %v %+v", err, again)
	}

	open, err := f.store.ListOpenDelphiDelegations(f.ctx)
	if err != nil || !containsDelphi(open, d.ID) {
		t.Fatalf("open list misses the submitting row: %v", err)
	}
	if _, _, err := f.store.FinishDelphi(f.ctx, d.ID, DelphiOutcome{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.store.GetDelphiDelegation(f.ctx, f.project.ID, "r1"); got.State != domain.DelphiSubmitting {
		t.Fatalf("a submitting row must not finish before it has a batch: %s", got.State)
	}

	d, err = f.store.SetDelphiRunning(f.ctx, d.ID, "msgbatch_1")
	if err != nil || d.State != domain.DelphiRunning || d.BatchID != "msgbatch_1" {
		t.Fatalf("running: %v %+v", err, d)
	}
	if _, err := f.store.SetDelphiRunning(f.ctx, d.ID, "other"); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("rebinding a running row: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	o := DelphiOutcome{Status: "succeeded", Truncated: true, StopReason: "max_tokens", InputTokens: 10,
		OutputTokens: 20, CostUSD: 0.000525, FinishedAt: now}
	fin, changed, err := f.store.FinishDelphi(f.ctx, d.ID, o)
	if err != nil || !changed || fin.State != domain.DelphiFinished || fin.Status != "succeeded" ||
		!fin.Truncated || fin.OutputTokens != 20 || fin.CostUSD != 0.000525 || fin.FinishedAt == nil {
		t.Fatalf("finish: %v %v %+v", err, changed, fin)
	}
	if _, changed, _ := f.store.FinishDelphi(f.ctx, d.ID, o); changed {
		t.Fatal("finished twice: side effects would repeat")
	}
	list, err := f.store.ListDelphiDelegations(f.ctx, f.project.ID, domain.DelphiFinished, 10)
	if err != nil || len(list) != 1 || list[0].Ref != "r1" {
		t.Fatalf("list: %v %+v", err, list)
	}

	// Abandon and delete only apply to submitting rows.
	s2, _ := f.store.CreateDelphiDelegation(f.ctx, domain.DelphiDelegation{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, Ref: "r2", Provider: "openai", Model: "m", BodySHA256: "x", Label: "L2"})
	if ab, err := f.store.AbandonDelphi(f.ctx, s2.ID); err != nil || ab.State != domain.DelphiAbandoned {
		t.Fatalf("abandon: %v %+v", err, ab)
	}
	if _, err := f.store.AbandonDelphi(f.ctx, d.ID); !errors.Is(err, domain.ErrIllegalTransition) {
		t.Fatalf("abandoning a finished row: %v", err)
	}
	s3, _ := f.store.CreateDelphiDelegation(f.ctx, domain.DelphiDelegation{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, Ref: "r3", Provider: "openai", Model: "m", BodySHA256: "x", Label: "L3"})
	if err := f.store.DeleteUnsubmittedDelphi(f.ctx, s3.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.GetDelphiDelegation(f.ctx, f.project.ID, "r3"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted row still there: %v", err)
	}
	if _, err := f.store.CreateDelphiDelegation(f.ctx, domain.DelphiDelegation{OrganizationID: f.org.ID,
		ProjectID: f.project.ID, Ref: "r3", Provider: "bedrock", Model: "m", BodySHA256: "x", Label: "L4"}); err == nil {
		t.Fatal("an unknown provider passed the CHECK constraint")
	}
}

func TestUsageAcceptsDelegationSource(t *testing.T) {
	f := newFixture(t)
	n, err := f.store.UpsertUsageBuckets(f.ctx, []domain.UsageBucket{{ProjectID: f.project.ID, PrincipalID: f.alice.ID,
		Source: "delegation", Harness: "delphi", Model: "claude-mythos-5-1", Provider: "anthropic",
		ExternalSessionID: "delphi:x", BucketStart: time.Now().UTC().Truncate(time.Hour), Requests: 1,
		InputTokens: 10, OutputTokens: 20, CostUSD: 0.0005, CostSource: "reported"}})
	if err != nil || n != 1 {
		t.Fatalf("usage row: %d %v", n, err)
	}
}

func containsDelphi(ds []domain.DelphiDelegation, id domain.ID) bool {
	for _, d := range ds {
		if d.ID == id {
			return true
		}
	}
	return false
}
