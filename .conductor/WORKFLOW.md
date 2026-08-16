---
version: 1
planner: planner.frontier
reviewer: reviewer.strong
required_checks:
  - go vet ./...
  - go test ./...
protected_scopes:
  - migration:primary
  - path:.github/workflows/release.yml
  - dir:internal/privacy
merge_policy:
  default: pull_request
  human_approval_for:
    - cryptography_sensitive
    - security_sensitive
    - schema_or_migration
---

# Project workflow

This file is repository-owned policy. Its content hash is recorded on every attempt, so a
result can always be explained by the rules that were in force when it ran.

## Before you edit

Obtain or attach to a Conductor task. An edit-capable session without a valid claim is a
policy violation, not a shortcut.

```bash
conductor task claim T-42          # or: conductor task claim --next
```

Declare intent first if you do not yet have a task; the control plane will tell you whether
someone is already doing this work:

```bash
conductor intent check --summary "Add retry-aware model routing" \
  --scope dir:internal/router --mode write
```

## Scope

Reserve only what you will actually modify. Directory reservations block more than path
reservations, so prefer paths. Report scope expansion **before** editing outside the
reserved set — the runner's Git watcher will catch it either way, but catching it late
pauses your attempt.

`migration:*` and every entry under `protected_scopes` are `protected_exclusive`: exactly
one writer, project-wide, no exceptions.

## Evidence

Every result must include the exact validation commands and their exit statuses. A model
saying "tests pass" is not evidence. Completion requires a commit or diff plus a validation
record produced by the runner.

Required checks for this project are declared in the frontmatter above.

## Privacy

Do not publish private chats, hidden reasoning, credentials, or unrelated terminal output as
task metadata. Progress summaries are visible per task visibility policy — keep them
structural.

If work is sensitive, file it with `visibility: private`. Other members will still be
blocked from colliding with your reserved scopes; they will not learn what you are doing.

## Handoff

Stopping mid-task? Hand off cleanly so the lease and reservations release immediately:

```bash
conductor task handoff T-42 --to codex --note "policy evaluator done, tests pending"
```

An abandoned session is reclaimed after the lease TTL, but its worktree is retained for
recovery and adoption requires a fresh epoch plus re-validation.

## Never

- Never bypass a claim because it is "a one-line fix".
- Never extend another principal's reservation on their behalf.
- Never resolve a conflict as ignored without recording why.
- Never force-push a branch you do not own.
- Never commit secrets, tokens, or `.env` files.
