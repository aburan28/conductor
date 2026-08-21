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

### Work goes where the capability is

A session advertises what it is driving. Conductor resolves that against the organization's
model catalog — so a session cannot promote itself by asserting a tier — and work that needs a
particular ceiling is offered to a session that has one.

```
$ conductor capabilities

demo

  3 session(s), 2 accepting work
  ceiling: tier T4, reasoning effort xhigh

  alice        claude    online_idle
       claude-opus-5 · tier T4 · effort xhigh (running high)
  rachel       codex     working
       gpt-5-codex · tier T2 · effort medium
       on T-12

$ conductor task assign T-42 --require-tier T4 --require-effort xhigh

T-42 offered to alice (claude-opus-5 on claude).
  requirement: tier ≥ T4, effort ≥ xhigh
  1 of 3 live session(s) qualified

  Not chosen:
    rachel       tier T2 is below the required T4
```

A floor is a floor: an idle cheap session never wins a selection it does not qualify for.
Above the floor the *cheapest* qualifying session wins, so a frontier session is still there
when something actually needs it. From inside a run, an agent that hits work beyond its own
ceiling calls `coord_delegate` — the same continuation bundle as a handoff, plus a floor the
receiver must meet. If nothing live qualifies, the bundle is still written and the caller is
told what the ceiling actually is.

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

conductord &                                # API, SSE, dashboard, scheduler on 127.0.0.1:8080
conductor login --endpoint http://localhost:8080 --token cdt_… --project myrepo
conductor dashboard                         # prints a ready-to-open link
```

### Adding your coworkers

```bash
conductor member add rachel --role contributor   # prints a token, once
conductor member list
conductor member remove rachel                   # also revokes their tokens
conductor token create --save                    # rotate your own
```

`conductord` binds loopback by default and **refuses to serve a reachable address in
plaintext**, because bearer tokens would cross the network in the clear. To expose it:

```bash
conductord --addr 0.0.0.0:8080 --tls-cert cert.pem --tls-key key.pem
conductord --addr 0.0.0.0:8080 --behind-proxy      # your proxy terminates TLS
```

Clients using a private CA set `CONDUCTOR_CA_CERT=/path/to/ca.pem`. Failed authentication is
throttled per client; a correct token is never throttled, so one person mistyping theirs
cannot lock out an office behind a shared NAT.

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
conductor capabilities                                    # which models are live, and how hard they can think
conductor task assign T-42 --require-tier T4 --require-effort xhigh
conductor inbox                                           # work offered to this session
conductor pause                                           # freeze every agent terminal on this machine
conductor resume                                          # wake them; closed terminals are reopened
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

Eleven tools: `conductor_check_conflicts`, `coord_start_work`, `coord_get_work`,
`coord_expand_scope`, `coord_report_progress`, `coord_publish_result`, `coord_finish_work`,
`coord_handoff`, `coord_delegate`, `coord_capabilities`, `coord_project_status`. Heartbeats are
deliberately *not* an MCP tool — a model should never spend tokens telling the server it is
still alive.

### Pausing the wall of terminals

A person running three agents has three terminals. Standing up from that desk — a meeting, a
laptop lid, an office move — is one command, and sitting back down is one command, even if
some of those terminals no longer exist by then.

```bash
conductor pause     # freeze every interactive agent session on this machine
conductor resume    # wake them all; --list shows what is saved
```

`conductor pause` finds every interactive Claude Code, Codex, and OpenCode session — launched
through `conductor wrap` or bare — saves a record of how to revive each one under
`~/.conductor/sessions/`, and freezes it with `SIGSTOP`. The terminals stay open, stopped
mid-thought. Before signaling anything, each pid is re-identified against the process table,
because a `SIGSTOP` delivered to a recycled pid would freeze a stranger.

`conductor resume` wakes each session where it can and reopens it where it must:

- **Its terminal survived.** `SIGCONT`, in place. Wrapped sessions come back seamlessly —
  the wrap sidecar stopped only the harness, so the shell never reclaimed the terminal.
  Bare sessions were their shell's foreground job; the shell took the terminal back when they
  stopped, so if the keyboard is dead, `fg` in that terminal hands it over — resume says so.
- **Its terminal was closed.** A new terminal is opened — a window in your current tmux, the
  platform's terminal app, an installed emulator, or a detached tmux session named
  `conductor` as a last resort (`CONDUCTOR_TERMINAL="kitty --directory {cwd} sh -c {cmd}"`
  overrides the choice) — running the harness's own conversation-resume invocation:
  `claude --continue`, `codex resume --last`, `opencode --continue`. Each harness keeps its
  transcript in its own local state, so the conversation survives the terminal; Conductor
  never sees it. One caveat: `codex resume --last` is Codex's most recent conversation
  globally, not per-directory, so two revived Codex sessions can land on the same one —
  `codex resume` opens the picker for the other.

Wrapped sessions stay honest with the team while paused: the sidecar keeps heartbeating as
`waiting_for_input`, so presence shows a parked session that is not offered work, rather than
a mystery that stopped moving. A relaunched wrap registers a fresh session with the same
capability flags it was started with.

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
- Session capability advertisement and capability-aware assignment: sessions declare the model
  and reasoning effort they are running, the catalog decides what that is worth, and work with
  a capability floor is offered to a session that clears it.
- Harness drivers for Claude Code, Codex, OpenCode, a generic templated `exec` driver, and a
  deterministic in-process fake.
- Machine-local pause/resume: `conductor pause` freezes every interactive agent session on
  the machine and `conductor resume` revives them — in place, or in freshly opened terminals
  on each harness's own conversation-resume invocation.
- Isolated git worktrees, scope-drift detection, runner-attested validation, evidence manifests,
  handoff bundles, portable Markdown task cards.
- Member and token administration, TLS, a loopback-by-default bind, and auth throttling.
- A runner that reaches the control plane over HTTP and holds no database credential
  (§28.2), alongside the in-process backend for single-host use (§28.1).

Not built, and where the design says it goes:

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
make e2e      # scripted two-person scenario end to end
```

CI runs all of it against a real Postgres on every push, and fails if the integration tests
skip — a misconfigured database service would otherwise produce a silently green run.

`scripts/e2e.sh` asserts the MVP acceptance criteria of DESIGN.md §31 rather than printing
output for a human to eyeball: that a completed task carries a commit and runner-observed
validation, that the attempt ran in a per-task worktree, that the workflow and config hashes
and the model routing were recorded, and that presence exposes a branch and a heartbeat and
nothing resembling a conversation.

Two acceptance criteria are still unverified, both for the same reason: nothing here has ever
launched a real Claude Code, Codex, or OpenCode process. §31.5 (each harness registers and
publishes progress) and the live half of §31.6 are exercised only through the built-in fake.

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
| `TestPrivateTaskIsRedactedOverHTTP` | the projection survives serialization, not just unit tests |
| `TestNonMemberSeesNotFoundNotForbidden` | a 403 would confirm the project exists |
| `TestStaleFenceIsA409` | a stale worker gets "stop", not "retry" |
| `TestParseFlagsAcceptsFlagsAfterPositionals` | CLI flags after a positional are not silently dropped |
| `TestSimilarityIsStableAcrossKeys` | duplicate detection does not miss real collisions across tenant keys |
| `TestMCPWorkLifecycleAgainstLiveServer` | the MCP gateway works against the real API, not a stub |
| `TestQueuedAttemptCannotSucceed` | an attempt cannot report success without having run |

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
