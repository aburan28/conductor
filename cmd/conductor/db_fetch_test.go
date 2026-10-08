package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/pgarchive"
)

// errS3Failure is the shape of an error from a bucket that is down or refusing us: not "not
// archived", so Postgres must not read it as the end of the archive.
var errS3Failure = errors.New("backup: S3 GET conductor/db/7001/wal/000000010000000000000029: 503 Service Unavailable: SlowDown")

func TestFetchWALOnlyAMissingSegmentIsNotArchived(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want fetchWALResult
	}{
		{"segment written", nil, fetchWALDone},
		{"segment absent", pgarchive.ErrNotArchived, fetchWALNotArchived},
		{"segment absent, wrapped", fmt.Errorf("fetch: %w", pgarchive.ErrNotArchived), fetchWALNotArchived},
		{"S3 error", errS3Failure, fetchWALFailed},
		{"missing settings", errors.New("the database is not archived: configure a bucket"), fetchWALFailed},
		{"bad key", errors.New("the archive key file is corrupt"), fetchWALFailed},
	}
	for _, c := range cases {
		if got := classifyFetchWAL(c.err); got != c.want {
			t.Errorf("%s: classified %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFetchWALRetriesAreBounded(t *testing.T) {
	calls := 0
	res, err := runFetchWAL(context.Background(), func(context.Context) error {
		calls++
		return errS3Failure
	}, fetchWALAttempts, 0)
	if res != fetchWALFailed || !errors.Is(err, errS3Failure) {
		t.Fatalf("result %v, err %v; want a failure carrying the S3 error", res, err)
	}
	if calls != fetchWALAttempts {
		t.Fatalf("fetched %d times; want exactly %d and no more", calls, fetchWALAttempts)
	}
}

func TestFetchWALDoesNotRetryANotArchivedSegment(t *testing.T) {
	calls := 0
	res, _ := runFetchWAL(context.Background(), func(context.Context) error {
		calls++
		return pgarchive.ErrNotArchived
	}, fetchWALAttempts, 0)
	if res != fetchWALNotArchived || calls != 1 {
		t.Fatalf("result %v after %d fetches; want not archived after one", res, calls)
	}
}

func TestFetchWALRecoversFromATransientFailure(t *testing.T) {
	calls := 0
	res, err := runFetchWAL(context.Background(), func(context.Context) error {
		calls++
		if calls == 1 {
			return errS3Failure
		}
		return nil
	}, fetchWALAttempts, 0)
	if res != fetchWALDone || err != nil || calls != 2 {
		t.Fatalf("result %v, err %v after %d fetches; want done on the second", res, err, calls)
	}
}

func TestFetchWALStopsRetryingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	res, _ := runFetchWAL(ctx, func(context.Context) error {
		calls++
		return errS3Failure
	}, fetchWALAttempts, time.Hour)
	if res != fetchWALFailed || calls != 1 {
		t.Fatalf("result %v after %d fetches; want one fetch and then failure", res, calls)
	}
}
