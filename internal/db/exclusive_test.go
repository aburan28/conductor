package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestExclusiveCancelsWorkerAfterBackendLoss(t *testing.T) {
	admin := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	name := fmt.Sprintf("test/exclusive/%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := NewWithPool(pool)
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := store.TryExclusive(ctx, name, func(workCtx context.Context) error {
			close(started)
			<-workCtx.Done()
			return context.Cause(workCtx)
		})
		finished <- err
	}()
	// Cancel and join before pool.Close, including a failed assertion before termination.
	defer func() { cancel(); <-finished }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("exclusive worker did not start")
	}
	called := false
	ran, err := admin.TryExclusive(ctx, name, func(context.Context) error { called = true; return nil })
	if err != nil || ran || called {
		t.Fatalf("contender ran while lock held: ran=%v called=%v err=%v", ran, called, err)
	}
	var terminated bool
	if err := admin.Pool().QueryRow(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1`, name).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate lock backend: terminated=%v err=%v", terminated, err)
	}
	select {
	case err := <-finished:
		finished <- err // Keep the deferred join valid.
		if !errors.Is(err, ErrExclusiveLockLost) {
			t.Fatalf("worker returned %v, want lock-loss cancellation", err)
		}
	case <-time.After(exclusiveCheckInterval + exclusiveCheckTimeout + 2*time.Second):
		t.Fatal("worker was not canceled after its lock connection died")
	}
	ran, err = admin.TryExclusive(ctx, name, func(context.Context) error { return nil })
	if err != nil || !ran {
		t.Fatalf("successor could not acquire released lock: ran=%v err=%v", ran, err)
	}
}

func TestExclusiveReleasesLockAfterCallbackCancellation(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	name := fmt.Sprintf("test/exclusive-cancel/%d", time.Now().UnixNano())
	ran, err := store.TryExclusive(ctx, name, func(workCtx context.Context) error {
		cancel()
		return workCtx.Err()
	})
	if !ran || !errors.Is(err, context.Canceled) || errors.Is(err, ErrExclusiveLockLost) {
		t.Fatalf("callback cancellation: ran=%v err=%v", ran, err)
	}
	// Use a separate pool: advisory locks are reentrant on the same connection, so
	// reusing the original pool could hide an unlock failure.
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ran, err = NewWithPool(pool).TryExclusive(context.Background(), name, func(context.Context) error { return nil })
	if err != nil || !ran {
		t.Fatalf("lock survived canceled callback: ran=%v err=%v", ran, err)
	}
}
