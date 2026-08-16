package domain

import "errors"

// Sentinel errors. The API layer maps each to an HTTP status exactly once, in
// internal/api/errors.go, so status-code decisions cannot drift between handlers.
var (
	// ErrNotFound covers both "does not exist" and "exists but you cannot see it". Those
	// are deliberately indistinguishable to the caller: revealing that a task exists in
	// another principal's private scope is itself a disclosure.
	ErrNotFound = errors.New("not found")

	ErrInvalidEnum       = errors.New("invalid value")
	ErrInvalidArgument   = errors.New("invalid argument")
	ErrIllegalTransition = errors.New("illegal state transition")

	// ErrStaleFencing is returned when a caller presents a fencing epoch older than the
	// task's current epoch. The caller must stop immediately (DESIGN.md §10.3).
	ErrStaleFencing = errors.New("stale fencing epoch")

	// ErrLeaseNotHeld means the lease is released, expired, or held by someone else.
	ErrLeaseNotHeld = errors.New("lease not held")

	// ErrAlreadyClaimed means another principal holds an active lease on the task.
	ErrAlreadyClaimed = errors.New("task already claimed")

	// ErrConflict is returned by the reservation engine when a requested scope collides
	// with an active reservation under the conflict matrix.
	ErrConflict = errors.New("scope conflict")

	// ErrDuplicate is returned when an intent matches existing work exactly.
	ErrDuplicate = errors.New("duplicate work")

	ErrDependencyCycle = errors.New("dependency cycle")
	ErrNotPermitted    = errors.New("not permitted")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrBudgetExhausted = errors.New("budget exhausted")
	ErrCapacity        = errors.New("no capacity")
	ErrConcurrency     = errors.New("concurrency limit reached")
)
