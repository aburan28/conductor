package domain

import "fmt"

// The transition tables below are the single source of truth for legal state changes
// (DESIGN.md §9). Both the API and the scheduler go through CanTransitionTask /
// CanTransitionAttempt, so an illegal transition is a 409 with a specific message rather
// than a silent no-op or a corrupted row.

var taskTransitions = map[TaskStatus][]TaskStatus{
	TaskProposed: {TaskReady, TaskCancelled, TaskSuperseded},

	TaskReady: {TaskClaimed, TaskBlockedDependency, TaskBlockedConflict, TaskCancelled, TaskSuperseded},

	// claimed -> ready covers "lease expired before the attempt started".
	TaskClaimed: {TaskRunning, TaskReady, TaskBlockedConflict, TaskFailed, TaskCancelled},

	TaskRunning: {
		TaskBlockedDependency, TaskBlockedConflict, TaskBlockedInput,
		TaskVerifying, TaskReady, TaskFailed, TaskCancelled,
	},

	// A blocker can clear either with the lease still valid (-> running) or after it was
	// released (-> ready). Both edges exist in §9.1.
	TaskBlockedDependency: {TaskReady, TaskRunning, TaskCancelled, TaskFailed, TaskSuperseded},
	TaskBlockedConflict:   {TaskReady, TaskRunning, TaskCancelled, TaskFailed, TaskSuperseded},
	TaskBlockedInput:      {TaskReady, TaskRunning, TaskCancelled, TaskFailed, TaskSuperseded},

	TaskVerifying: {TaskRunning, TaskReviewRequired, TaskDone, TaskMerging, TaskFailed, TaskCancelled},

	TaskReviewRequired: {TaskRunning, TaskMerging, TaskDone, TaskFailed, TaskCancelled},

	TaskMerging: {TaskDone, TaskRunning, TaskFailed},

	// Operator retry is the only edge out of failed.
	TaskFailed: {TaskReady, TaskCancelled},

	TaskDone:       {},
	TaskCancelled:  {},
	TaskSuperseded: {},
}

var attemptTransitions = map[AttemptState][]AttemptState{
	// Two edges out of queued that the linear runner flow does not suggest:
	//
	//   -> running: an interactive session (a human, or an agent working through MCP) has
	//      no workspace to prepare and no harness to start — it already has the repository
	//      open. Principle §4.12 requires humans and agents to use the same task model, so
	//      forcing an interactive claim through runner bookkeeping states would make the
	//      shared model a fiction.
	//
	//   -> failed_retryable: a runner can fail between taking the claim and preparing a
	//      workspace (no capacity, no matching model profile, disk full). That failure is
	//      retryable, so the task returns to the queue rather than being stranded until its
	//      lease expires.
	//   -> paused_conflict: the claim collided with someone else's territory before any work
	//      began. Without this edge the pause is silently dropped and the attempt sits in
	//      `queued`, which reads as "about to start" when it is actually blocked.
	//
	// What stays illegal from here is queued -> succeeded: an attempt cannot report success
	// without ever having run.
	AttemptQueued: {
		AttemptPreparingWorkspace, AttemptRunning, AttemptPausedConflict, AttemptCancelled,
		AttemptFailedRetryable, AttemptFailedTerminal, AttemptStale,
	},

	AttemptPreparingWorkspace: {
		AttemptStartingHarness, AttemptFailedRetryable, AttemptFailedTerminal,
		AttemptCancelled, AttemptStale,
	},

	AttemptStartingHarness: {
		AttemptRunning, AttemptFailedRetryable, AttemptFailedTerminal,
		AttemptCancelled, AttemptStale,
	},

	AttemptRunning: {
		AttemptWaitingApproval, AttemptWaitingInput, AttemptPausedConflict,
		AttemptSucceeded, AttemptFailedRetryable, AttemptFailedTerminal,
		AttemptCancelled, AttemptStale,
	},

	AttemptWaitingApproval: {AttemptRunning, AttemptCancelled, AttemptFailedTerminal, AttemptStale},
	AttemptWaitingInput:    {AttemptRunning, AttemptCancelled, AttemptFailedTerminal, AttemptStale},
	AttemptPausedConflict: {
		AttemptRunning, AttemptCancelled, AttemptFailedTerminal,
		AttemptFailedRetryable, AttemptStale,
	},

	AttemptSucceeded:       {},
	AttemptFailedRetryable: {},
	AttemptFailedTerminal:  {},
	AttemptCancelled:       {},
	AttemptStale:           {},
}

// CanTransitionTask reports whether from -> to is legal.
func CanTransitionTask(from, to TaskStatus) bool {
	if from == to {
		return true // idempotent re-assertion of the current state
	}
	for _, allowed := range taskTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// AssertTaskTransition returns ErrIllegalTransition when from -> to is not permitted.
func AssertTaskTransition(from, to TaskStatus) error {
	if !CanTransitionTask(from, to) {
		return fmt.Errorf("%w: task %s -> %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// CanTransitionAttempt reports whether from -> to is legal.
func CanTransitionAttempt(from, to AttemptState) bool {
	if from == to {
		return true
	}
	for _, allowed := range attemptTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// AssertAttemptTransition returns ErrIllegalTransition when from -> to is not permitted.
func AssertAttemptTransition(from, to AttemptState) error {
	if !CanTransitionAttempt(from, to) {
		return fmt.Errorf("%w: attempt %s -> %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// TaskStatusForBlocker maps a blocking reason onto the specific blocked status.
func TaskStatusForBlocker(reason string) TaskStatus {
	switch reason {
	case "dependency":
		return TaskBlockedDependency
	case "input":
		return TaskBlockedInput
	default:
		return TaskBlockedConflict
	}
}

// TaskStatusAfterAttempt maps a terminal attempt state onto the task status that should
// follow it, given how many attempts the task has already consumed.
//
// The retry budget is deliberately evaluated here rather than at the call site so that the
// scheduler, the API, and the reconciler cannot disagree about when a task is exhausted.
func TaskStatusAfterAttempt(attempt AttemptState, attemptsUsed, maxAttempts int) TaskStatus {
	switch attempt {
	case AttemptSucceeded:
		return TaskVerifying
	case AttemptCancelled:
		return TaskCancelled
	case AttemptFailedTerminal:
		return TaskFailed
	case AttemptFailedRetryable, AttemptStale:
		if attemptsUsed >= maxAttempts {
			return TaskFailed
		}
		return TaskReady
	default:
		return TaskRunning
	}
}
