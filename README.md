# Conductor

A coordination control plane for teams where humans and coding agents work the same
repository at the same time.

Coding agents got fast; coordination did not. On a three-person team all running Claude Code,
Codex, or OpenCode against one repo, the expensive failures are not bad code — they are two
people building the same thing, two branches rewriting the same file, two migrations racing
the same table, and nobody able to answer "what is actually in flight right now?"

The obvious fix — share the chats — is the wrong fix. Prompts and model output are the most
private thing a developer produces. **Conductor never shares them.** It shares intent and
territory: who holds what, what shape of work they are doing, and where two efforts are about
to collide.

The full architecture is in [docs/DESIGN.md](docs/DESIGN.md). This README is how to run it and
what is actually built.

---

## What it does

```
$ conductor check --summary "add retry-aware model routing" --scope dir:internal/router

block_conflict: scope conflict on dir:internal/router

  alice holds dir:internal/router for T-1 (write_exclusive).
  Wait for it, split your scope, or join their task.
```

That is the whole product in one command. Everything else exists to make that answer correct,
fast, and safe to trust.

It also catches the harder case — two people describing the same work in different words, with
no overlapping files yet:

```
$ conductor check --summary "Build team invitation flow: send invite emails, accept invitations"

suggest_join: similar work already in flight

  T-3 (owner alice) looks like the same work. Join it, or narrow your scope.
```

Alice wrote "Implement the team invite flow with email invitations and acceptance". The server
never saw either sentence in a form it can read back: both were reduced to HMAC'd token sets
under a per-tenant key and compared with MinHash. Detection without disclosure.

### Privacy is structural, not procedural

A teammate looking at Alice's private task sees:

```
T-2      ready      alice      (private)
         dir:internal/api
```

Enough to not collide. Nothing about what it is. And the schema has no column for a prompt,
the event payload passes an allowlist, and the harness stream adapters drop assistant text at
the parse boundary before it can reach the store. Three tests assert this mechanically:
`TestNoTranscriptFieldsInSharedTypes`, `TestNoTranscriptColumnsInSchema`, and
`TestEventTypeHasNoContentField`.

---

## Quickstart

Requires Go 1.25+, Docker (for Postgres), and git.

```bash
make db-up                                  # Postgres on :55432
make build                                  # bin/conductord, bin/conductor, bin/conductor-mcp

cd /path/to/your/repo
conductor init                              # scaffold .conductor/ policy files

export DATABASE_URL="postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable"
conductord bootstrap --org acme --project myrepo --principal $USER --repo .
# prints a token and the exact `conductor login` line to run

conductord &                                # API, SSE, dashboard, scheduler on :8080
conductor login --endpoint http://localhost:8080 --token cdt_… --project myrepo
conductor dashboard                         # prints a ready-to-open link
```

Prove the whole execution loop with no API key and no vendor CLI installed:

```bash
conductor task create --title "Try Conductor" --scope path:README.md
conductor worker --dry-run succeed --once -v
```

The built-in fake harness claims a task, creates a worktree, edits a file, runs your required
checks, commits, and submits evidence — exercising every coordination path with a deterministic
stand-in for a model.

Or run the scripted demo, which reproduces the scenario above end to end:

```bash
make e2e
```

---

## Daily use

```bash
conductor check --summary "…" --scope dir:internal/api    # before you edit. exit 3 = stop
conductor task claim --next                               # take work and its territory
conductor wrap claude                                     # register a session + heartbeat, then launch
conductor presence --watch                                # who is live, on what
conductor conflicts                                       # what is contested and what to do
conductor task handoff T-42 --to codex --next "write tests"
```

Every command takes `--json`.

### Mounting the MCP tools in an agent

```json
{
  "mcpServers": {
    "conductor": {
      "command": "conductor-mcp",
      "env": { "CONDUCTOR_TOKEN": "cdt_…", "CONDUCTOR_PROJECT": "myrepo" }
    }
  }
}
```

Nine tools: `conductor_check_conflicts`, `coord_start_work`, `coord_get_work`,
`coord_expand_scope`, `coord_report_progress`, `coord_publish_result`, `coord_finish_work`,
`coord_handoff`, `coord_project_status`. Heartbeats are deliberately *not* an MCP tool — a
model should never spend tokens telling the server it is still alive.

---

## How it holds together

```
 Claude Code · Codex · OpenCode · human sessions
        │                    │
   MCP tools           conductor CLI / wrap
        └────────┬───────────┘
                 ▼
      control plane (conductord)
   ledger · leases · reservations · conflict graph · presence
                 │
          PostgreSQL (source of truth)
                 │
      scheduler ─┴─ adaptive router
                 │
   harness drivers → isolated git worktrees
```

Four mechanisms do the real work:

**Transactional claims.** `SELECT … FOR UPDATE SKIP LOCKED` makes duplicate dispatch
structurally impossible rather than unlikely. Two schedulers racing the same ready queue
cannot select the same row, so replicas need no leader election.

**Fencing epochs.** Every claim gets a strictly higher epoch. A worker that was paused, lost
its lease, and woke up later presents a stale epoch and is rejected — it can keep writing in
its own worktree, but it can never publish. Expiry alone does not close that window; the epoch
does.

**Reservations under an advisory lock.** Territory is per-resource (file, directory, glob,
migration lane, table, API route, symbol), and acquisition takes a per-project advisory lock so
check-then-insert cannot interleave. Without it, two agents each see a clear field and both
plant a flag.

**Merge risk from observed diffs.** Runners report the paths git says changed, so the conflict
graph is built from what agents are *doing*, not only what they declared. That is what turns a
merge-time disaster into a minute-five warning.

---

## What is built, and what is not

Implemented and exercised by tests:

- Task ledger with the full DESIGN.md §9 state machines, enforced in Go and in Postgres.
- Atomic claims, expiring leases, fencing epochs, reclamation, retry budgets.
- Scope reservations across all nine resource types, with the complete §11.3 conflict matrix.
- Privacy-preserving duplicate detection (HMAC + MinHash), field-level visibility projections.
- Conflict graph: scope overlap, duplicate intent, merge risk, with join/wait/split advice.
- Presence, event log with gapless per-aggregate sequencing, SSE stream, live dashboard.
- REST API, MCP gateway, CLI, session wrapper with heartbeat sidecar.
- Scheduler: reconcile, session reaping, stall detection, dependency gating, budget events.
- Adaptive router: hard floors, tiers, escalation, de-escalation, budget guard.
- Harness drivers for Claude Code, Codex, OpenCode, a generic templated `exec` driver, and a
  deterministic in-process fake.
- Isolated git worktrees, scope-drift detection, runner-attested validation, evidence manifests,
  handoff bundles, portable Markdown task cards.

Not built, and where the design says it goes:

- **Runner over HTTP.** `conductor worker` talks to Postgres directly, which suits the
  single-host deployment of §28.1. The §28.2 shape — a laptop runner speaking only outbound
  HTTPS to a shared control plane — needs an HTTP-backed implementation of the same loop.
- **Planner and reviewer services** (§14, §15.3). The contracts, validation rules, and
  `reviewer.*` routing are in place; nothing yet invokes a model to decompose an objective or
  review a diff.
- **Codex App Server driver** (§16.3). The Codex driver shells out to `codex exec --json`
  rather than binding the bidirectional JSON-RPC App Server.
- **OIDC** (§25.1). Authentication is bearer tokens hashed at rest; there is no identity
  provider integration.
- **Merge queue, PR integration, tracker sync, symbol/tree-sitter indexing** (§29, §30 phase 5).
- **Codex and OpenCode model ids** are declared but disabled in `.conductor/models.yaml`. The
  design forbids hardcoding provider model names that have not been verified, so an operator
  fills those in.

One deliberate deviation from the design document: it recommends TypeScript (§28.1). This is
Go, at the repository owner's direction. The tradeoff is real — the Claude Agent SDK and
OpenCode SDK are TypeScript, so their drivers here are CLI-based rather than SDK-based.

---

## Testing

```bash
make unit     # pure logic, no database
make test     # everything; integration tests skip without DATABASE_URL
make db-up && make test
```

The suite proves the invariants rather than asserting them in prose. Notably:

| Test | Proves |
|---|---|
| `TestConcurrentClaimsYieldExactlyOneLease` | 24 concurrent claims → exactly one winner |
| `TestStaleFenceIsRejected` | a reclaimed worker cannot heartbeat, release, or publish |
| `TestConcurrentOverlappingReservationsSerialize` | 12 racing migration reservations → one winner |
| `TestClaimNextDoesNotDoubleDispatch` | 6 scheduler replicas, 8 tasks, no double dispatch |
| `TestReclamationReleasesReservations` | a dead session frees its territory |
| `TestPrivateTaskHidesIntentButKeepsTerritory` | private work still prevents collisions |
| `TestNoTranscriptColumnsInSchema` | the database has nowhere to put a prompt |
| `TestMatrixMatchesDesign` | all 25 cells of the §11.3 conflict matrix |
| `TestSecurityFloorIsAbsolute` | budget pressure cannot downgrade a security-sensitive task |

---

## Configuration

Policy lives in the repository, versioned with the code it governs, and every attempt records
the hash of the files in force when it ran — so a result can always be explained by the rules
that produced it.

| File | What it controls |
|---|---|
| `.conductor/project.yaml` | lease TTLs, heartbeat cadence, visibility defaults, isolation |
| `.conductor/policies.yaml` | conflict matrix, duplicate thresholds, hard routing rules, budgets |
| `.conductor/models.yaml` | model aliases (roles), capability floors, concrete profiles |
| `.conductor/WORKFLOW.md` | the prose contract every agent reads; required checks; protected scopes |

---

## License

Not yet chosen. DESIGN.md §35 notes that Apache-2.0 components from OpenAI Symphony are
compatible with reuse here.
