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

### The CLI dispatches to the right model, by policy

`conductor capabilities` is about the sessions live *right now*. The other half is a repository
policy that says which concrete model each kind of work should go to — declared in
`.conductor/dispatch.yaml`, versioned with the code, and hashed onto every attempt so a routing
decision is always explainable.

```yaml
# .conductor/dispatch.yaml
lanes:
  implement:
    role: implementer
    candidates:
      - model: ollama/qwen3:27b        # a local model for small, low-risk work
        harness: opencode
        tags: [local]
        when: task.estimated_files <= 3 && !task.security_sensitive
        max_concurrent: 1              # one GPU, one attempt at a time
      - model: claude-sonnet-5
        harness: claude
      - model: claude-opus-5           # escalation walks down the ladder on failure
        harness: claude

rules:
  - id: docs-local
    when: task.labels has "docs"
    prefer: { tag: local }
  - id: routing-changes
    when: task.paths any "internal/router/**"
    require: { tier: T3 }

defaults: { lane: implement, on_failure: escalate, max_escalations: 2 }
```

The `when` expressions read deterministic facts derived from the ledger — scopes, labels,
attempt history, budget position — never a prompt. `conductor route T-42` shows what the policy
would decide and why, before a token is spent:

```
$ conductor route T-42

T-42 would route to:

  claude-sonnet-5 on claude  (lane implement, effort medium, tier T2)

  Considered:
    ✗ ollama/qwen3:27b on opencode — condition not met: task.estimated_files <= 3 && !task.security_sensitive
    ✓ claude-sonnet-5 on claude
    · claude-opus-5 on claude

  Rationale:
    • lane implement (3 candidates)
    • chose claude-sonnet-5 on claude at effort medium
```

A dispatch candidate can never route *below* a hard floor: a security-sensitive task keeps its
T4 floor whatever the ladder says. `conductor policy lint` validates the whole file — unknown
facts, undefined lanes, malformed expressions — and `conductor models discover` finds the local
models (Ollama today) worth adding to the ladder.

### Budgets bound the team, not the person

With `budget.member.monthly_tokens` set, every member gets the same token allowance over a
rolling 30-day window — and it is *transferable*. Alice heading into a slack week hands her
headroom to Bob mid-refactor, in one command, with no admin in the loop:

```
$ conductor budget share bob 2m --note "finishing the router refactor"

Shared 2m tokens with bob.

  you    1.1m remaining
  bob    3.4m remaining
```

A member whose window balance is spent cannot claim new work (HTTP 402, exit-code fail from
the CLI) — until a teammate shares theirs. Balances are pure arithmetic over two ledgers
(attempt spend and budget grants), so there is no counter to drift and no way to mint tokens:
grants are checked against the giver's live balance under the same lock the claim path takes.
`conductor budget` shows everyone's position; `conductor budget grants` is the transfer
history; a `budget.shared` event lands on the team stream. As everywhere else, amounts and
identities are shared — what the tokens were spent *on* is not.

### The team pools capacity, and queues when it is full

Coworkers connect to each other through Conductor: a teammate joins the same control plane and
their machines and sessions become shared capacity — a *swarm*. `conductor swarm` rolls up who
is contributing what, and who has budget to spare:

```
$ conductor swarm

Capacity: 2 runner(s), 3 session(s) accepting work, 5 free slot(s), 1 waiting in queue

  WHO            KIND     STATE       LOAD        BUDGET LEFT
  alice          session  working     ready       3.4m
  rachel         runner   online      1/4         1.1m
  bob            session  online_idle ready       0

Share budget with a teammate: conductor budget share <who> <tokens>
```

A teammate joins from the link `conductor invite <them>` prints — `conductor join "<link>"`
(`conductor swarm join "<link>"` is the same thing) — and then contributes interactive capacity
with `conductor wrap`, autonomous capacity with `conductor worker`, or spare tokens with
`conductor budget share`.

When too many sessions or attempts are running at once, new work does not fail — it takes a
place in an **admission queue** and waits. Set the caps in `.conductor/policies.yaml`:

```yaml
concurrency:
  max_active_sessions: 6          # across the whole team
  max_sessions_per_principal: 2   # so one person cannot take every slot
  max_concurrent_attempts: 4
```

Past the cap, `conductor wrap` parks with its place in line (`waiting for a session slot,
position 2…`) and starts the moment a slot frees up; the scheduler grants tickets in arrival
order, and a granted slot that stops heartbeating is handed on rather than held forever.
`conductor queue` shows the whole line. As everywhere else, a ticket carries identity, a kind,
and a model name — never what the work is about.

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
make up
```

That is the whole thing: Postgres on `:55432`, the binaries in `bin/`, the control plane
(API, SSE, dashboard, scheduler) serving `127.0.0.1:8080` in the background — log and
pidfile under `.conductor/runtime/` — and your CLI login saved at `~/.conductor/credentials`.
No token to copy, no second terminal.

```bash
conductor status                             # what is in flight
conductor dashboard                          # prints a ready-to-open link
make down                                    # stop the control plane (make db-down also stops Postgres)
```

### Manual, or on another repository

```bash
make db-up && make build                     # Postgres + bin/conductord, bin/conductor, bin/conductor-mcp

cd /path/to/your/repo
conductor init                               # scaffold .conductor/ policy files

export DATABASE_URL="postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable"
conductord bootstrap --org acme --project myrepo --principal $USER --repo .
# saves your login at ~/.conductor/credentials — no copy-paste
# (--no-login skips that; --endpoint saves a different URL than http://localhost:8080)

conductord &                                 # or: make serve, same thing in the foreground
conductor dashboard                          # prints a ready-to-open link
```

### Adding your coworkers

The fastest way is one link. `conductor invite` mints a teammate their own token and bundles
the endpoint, project, and token into a single join link; they redeem it with `conductor join`
(or by opening it in a browser) and they are in:

```bash
$ conductor invite rachel --role maintainer --expires 7d

Invited rachel as maintainer on myrepo.

Send them this link, once, over a channel you trust:

  https://conductor.team/#project=myrepo&token=cdt_QaltIz7t…

They run:  conductor join "<link>"     (or open it in a browser)

The token expires 2026-09-03T19:37:59Z.
```

```bash
# on the teammate's machine
conductor join "https://conductor.team/#project=myrepo&token=cdt_QaltIz7t…"
# Joined https://conductor.team as rachel. Then: conductor wrap claude / conductor worker.
```

The token rides in the URL **fragment** (after `#`), which a browser never sends to the server —
so the credential stays out of every request line and access log, unlike a query-string link.
The same link opens the web dashboard: it reads the fragment on load, then strips it from the
address bar. If the endpoint you are logged in against is loopback (`127.0.0.1`), `invite` warns
that a teammate cannot reach it and shows how to expose the control plane and pass a public
`--endpoint`.

The longer form still works, and is what a script or CI wants:

```bash
conductor member add rachel --role contributor   # prints a `conductor login …` line, once
conductor member list
conductor member remove rachel                   # also revokes their tokens
conductor token create --save                    # rotate your own
```

A joined teammate contributes to the swarm — `conductor wrap` for interactive work, `conductor
worker` for autonomous work, `conductor budget share` for spare tokens (see below).

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
conductor serve qwen                                      # local vLLM for OpenCode (also: flash, glm53)
conductor wrap opencode --model vllm/qwen3.8-27b
conductor presence --watch                                # who is live, on what
conductor conflicts                                       # what is contested and what to do
conductor task handoff T-42 --to codex --next "write tests"
conductor capabilities                                    # which models are live, and how hard they can think
conductor task assign T-42 --require-tier T4 --require-effort xhigh
conductor inbox                                           # work offered to this session
conductor budget                                          # the team's token budget this window
conductor budget share rachel 500k                        # give a teammate part of your allowance
conductor pause                                           # freeze every agent terminal on this machine
conductor resume                                          # wake them; closed terminals are reopened
conductor sessions save all                               # keep every session resumable, even after a reboot
conductor usage --by day,harness                          # tokens and cost over time, across claude/codex/opencode
conductor sessions export                                 # the project's session history, as JSON
conductor sessions install-hook                          # capture every session at shutdown (systemd/launchd)
conductor backup push | pull | status                    # copy this machine's resume records to/from S3
conductor integrate cursor                                # wire a coding tool to this project (MCP + hooks)
conductor route T-42                                      # what would this route to, and why — before spending a token
conductor dispatch T-42                                   # send work to a model by policy, through the queue
conductor models                                          # the model catalog; `models discover` finds local ones
conductor policy lint                                     # validate .conductor/ policy and dispatch rules
conductor swarm                                           # the team's pooled capacity and who has budget to share
conductor queue                                           # the admission line when the team is at capacity
```

Every command takes `--json`.

### Connecting your coding tool

One command wires Conductor's MCP tools — and, where the tool supports them, pre-edit hooks —
into whatever you drive:

```bash
conductor integrate claude        # Claude Code: .mcp.json + PreToolUse/SessionStart hooks
conductor integrate cursor        # Cursor: .cursor/mcp.json + a rules file
conductor integrate codex         # Codex: ~/.codex/config.toml
conductor integrate opencode      # OpenCode: opencode.json + a pre-tool plugin
conductor integrate all           # every tool this machine has
```

Also supported: `windsurf`, `vscode`, `zed`, `gemini`. Each merges into the tool's own config
without disturbing anything else already there, and `--print` shows exactly what it would write
before it writes it. `conductor doctor` reports which tools are connected.

Every write is idempotent and never puts a bearer token into a project file that could be
committed: stdio configs need no token (the `conductor-mcp` binary reads your saved login), and
HTTP configs reference the token through the tool's own `${env:CONDUCTOR_TOKEN}` syntax.

**Two transports.** The `conductor-mcp` binary speaks MCP over stdio, for any tool that launches
a local process:

```json
{ "mcpServers": { "conductor": {
  "command": "conductor-mcp", "args": ["--project", "myrepo"] } } }
```

Or point an HTTP-capable client straight at the control plane — no local binary, which is what a
teammate on a shared server wants:

```json
{ "mcpServers": { "conductor": { "type": "http",
  "url": "https://conductor.team/mcp",
  "headers": { "Authorization": "Bearer ${CONDUCTOR_TOKEN}", "X-Conductor-Project": "myrepo" }
} } }
```

`conductord` serves the Streamable HTTP transport at `/mcp` (and `/mcp/{project}`), negotiating
protocol revisions `2024-11-05` through `2025-06-18`, with per-session ids and the same bearer
auth every other client uses — the gateway holds no private path into the store, over either
transport.

Eleven tools: `conductor_check_conflicts`, `coord_start_work`, `coord_get_work`,
`coord_expand_scope`, `coord_report_progress`, `coord_publish_result`, `coord_finish_work`,
`coord_handoff`, `coord_delegate`, `coord_capabilities`, `coord_project_status`. Heartbeats are
deliberately *not* an MCP tool — a model should never spend tokens telling the server it is
still alive. And where a harness supports pre-edit hooks, `conductor integrate` installs
`conductor hook pre-tool`, which calls the same conflict check before every edit and blocks the
tool call (exit 2, with the holder named) when someone else holds the file — enforcement, not
just advice.

### Token usage across harnesses

Every harness meters itself and none of them compare notes. Claude Code writes a usage
block on each message of its transcript, Codex logs a running total after every response,
OpenCode stores tokens and cost on every message — each on one machine, each for itself.
Conductor reads those logs and keeps one ledger.

```bash
conductor usage                          # last 7 days, by harness
conductor usage --by day,harness         # a daily series per harness
conductor usage --by model --since 30d   # which models carry the load
conductor usage --by principal           # who used what
conductor usage sync                     # report this directory's unwrapped sessions
```

A session launched through `conductor wrap` reports as it runs: the sidecar re-reads the
harness's own log once a minute, folds it into hourly buckets — one per harness session and
model — and sends only what changed, with a final flush at exit (`CONDUCTOR_USAGE=off`
disables it). Sessions that were not wrapped are reported after the fact with `conductor
usage sync`, which reads the same logs for the current directory. Runner attempts land in
the same ledger from their progress reports. Buckets carry absolute counts, so re-reporting
replaces rather than adds, and a restarted collector cannot double-count.

Cost is what the harness reported where it reports one (OpenCode); otherwise Conductor
estimates it from the organization's model catalog at list price, cache reads at a tenth,
and marks the row `catalog`. A model the catalog does not know stays unpriced rather than
guessed.

What crosses the wire is numbers, a model name, and an hour. The readers decode only the
usage fields of each record; there is no struct field the transcript text could land in,
and OpenCode's export is asked to redact before Conductor even sees it. Team totals by day,
harness, and model are visible to every member. Per-session detail is your own unless you
maintain the project, and other people's model names follow `publishModelIdentity`, exactly
as they do in presence. The dashboard shows usage over time by harness and model, alongside the task board, fleet, swarm, and admission queue.

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

Pausing is for stepping away; saving is for the terminals themselves going away.

```bash
conductor sessions save all     # keep every live session resumable — nothing is stopped
conductor sessions list         # what this machine knows: saved, paused, running
```

A running session's record normally vanishes with its process — right after a crash, wrong
after a reboot with three conversations open. `conductor sessions save all` marks every
session on the machine as deliberately kept. The sessions keep running; if a terminal is
closed or the machine restarts, the record stays, listed as `saved`, and `conductor resume`
reopens the conversation exactly as it reopens a paused session whose terminal was closed. A
saved session you quit yourself is forgotten, as it should be, and a reopened one starts a
fresh record — save again if it should survive the next reboot too. Saving is per-machine and
touches nothing on the server; `conductor sessions export` is the other direction — the
project's whole session history, everyone's, as a JSON file.

### Surviving a shutdown, and the machine itself

You should not have to remember to run `save` before a reboot. Three layers make a shutdown
non-destructive:

1. **Wrapped sessions save themselves.** `conductor wrap` catches the `SIGTERM` a shutdown
   sends (and the `SIGHUP` a closed terminal or dropped SSH connection sends) and marks its
   record kept-for-resume *before* the harness is killed. The harness has already written its
   own transcript, so all that must be preserved is how to reopen it. Nothing to run first.

2. **A machine-wide capture hook** covers bare sessions (no sidecar to catch the signal) and
   unclean shutdowns. `conductor sessions install-hook` generates a systemd user
   service+timer (Linux) or a launchd agent (macOS) that runs `conductor sessions save all` at
   logout/shutdown and periodically — so even a machine that dies without a clean shutdown
   loses at most one interval of state. It writes the unit files and prints the one command to
   enable them; it never starts a system service for you.

3. **Off-host backup**, for when the machine itself does not come back — a terminated cloud
   instance takes its disk, and the local `~/.conductor/sessions` records with it. Point
   Conductor at an S3 bucket and the resume records travel too:

   ```bash
   export CONDUCTOR_BACKUP_S3_BUCKET=my-team-conductor
   export CONDUCTOR_BACKUP_S3_REGION=us-east-1
   export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…   # or an instance role's env

   conductor backup push        # bundle this machine's records to S3 (a manifest + a snapshot)
   conductor backup status      # where they go, and the latest snapshot
   conductor backup pull        # on a fresh instance: restore them, then `conductor resume`
   ```

   `conductor sessions save all` pushes automatically once a bucket is configured, the
   shutdown hook and the `wrap` SIGTERM handler push on the way down, and `conductor resume`
   pulls first when a machine has no local records — so a replaced instance resumes where the
   old one left off. Objects are keyed by machine under a prefix; set `CONDUCTOR_MACHINE_ID`
   to a stable id if hostnames are not (autoscaled hosts), or to another machine's id to
   adopt its sessions. The S3 client is dependency-free (SigV4 signed over the standard
   library) and works against any S3-compatible store — real S3, MinIO, R2 — via
   `CONDUCTOR_BACKUP_S3_ENDPOINT`. As everywhere else, only coordination metadata travels:
   how to reopen a session, never a transcript. `CONDUCTOR_BACKUP=off` disables it.

Wrapped sessions stay honest with the team while paused: the sidecar keeps heartbeating as
`waiting_for_input`, so presence shows a parked session that is not offered work, rather than
a mystery that stopped moving. A relaunched wrap registers a fresh session with the same
capability flags it was started with.

**VS Code:** integrated terminals are ordinary ptys, so pausing and in-place resume already
work there. Reopening a *closed* session into VS Code needs the companion extension in
[`integrations/vscode`](integrations/vscode) — VS Code offers no command-line way to open an
integrated terminal running a command, so `conductor resume` hands the session to the
extension via a `vscode://` URI (carrying only a record id, never a command) and the
extension opens the terminal in the session's working directory. Which sessions lived in
VS Code is recorded from `TERM_PROGRAM` at save time; without the extension installed,
resume simply falls back to the terminal chain above. The extension also adds
`Conductor: Pause All Agent Sessions` and `Conductor: Resume All Agent Sessions` to the
command palette.

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
- Shareable per-member token budgets: a rolling-window allowance each member can transfer to
  a teammate, enforced at claim time and settled entirely by ledger arithmetic.
- Harness drivers for Claude Code, Codex, OpenCode, a generic templated `exec` driver, and a
  deterministic in-process fake.
- Shutdown-durable sessions: `conductor wrap` saves its own session on the SIGTERM/SIGHUP a
  shutdown or closed terminal sends; `conductor sessions install-hook` adds a systemd/launchd
  hook that captures bare sessions at shutdown and periodically; and a dependency-free,
  SigV4-signed S3 backend (`conductor backup push|pull`) carries the resume records off-host,
  so a terminated cloud instance resumes on its replacement.
- Machine-local pause/resume: `conductor pause` freezes every interactive agent session on
  the machine and `conductor resume` revives them — in place, or in freshly opened terminals
  on each harness's own conversation-resume invocation.
- Isolated git worktrees, scope-drift detection, runner-attested validation, evidence manifests,
  handoff bundles, portable Markdown task cards.
- Member and token administration, TLS, a loopback-by-default bind, and auth throttling.
- Daemon-to-daemon peering over mutual TLS: a private CA names every control plane in a
  mesh, each daemon dials its configured or DNS-discovered peers and keeps a live link
  table, and `conductor peers` reports it. Connectivity and identity only — no data is
  replicated across the link. `--peer-discover-dns` finds peers via a DNS SRV record so a
  mesh can grow without an operator hand-listing every daemon's address.
- A runner that reaches the control plane over HTTP and holds no database credential
  (§28.2), alongside the in-process backend for single-host use (§28.1).
- One-link onboarding: `conductor invite <handle>` mints a teammate their own token and prints
  a single join link (token in the URL fragment, off the wire); `conductor join <link>` redeems
  it, and the same link self-connects the web dashboard.
- One-command integration into eight coding tools (Claude Code, Cursor, Codex, OpenCode,
  Windsurf, VS Code, Zed, Gemini CLI): MCP config plus, where supported, pre-edit hooks that
  run the conflict check before every edit and block on a hard conflict.
- MCP over Streamable HTTP served by `conductord` itself, so an HTTP-capable client connects
  with a bearer token and no local binary — the same eleven tools as the stdio gateway.
- Repository dispatch policy: named lanes, ordered model ladders, `when`-gated candidates, and
  hard-floor-respecting escalation, evaluated by a small deterministic expression language,
  with `conductor route` to preview and `conductor policy lint` to validate.
- A pooled-capacity swarm view and per-member budget sharing across a team, and an admission
  queue that makes sessions and attempts wait for a slot instead of failing when the team is at
  capacity, granted in arrival order with heartbeat-expiry hand-off.
- A single-page dashboard (no build step, no external requests) with task board, fleet and
  swarm views, live usage charts, the admission queue, conflict radar, and a per-tool
  integration guide.

Not built, and where the design says it goes:

- **Planner and reviewer services** (§14, §15.3). The contracts, validation rules, and
  `reviewer.*` routing are in place; nothing yet invokes a model to decompose an objective or
  review a diff.
- **Codex App Server driver** (§16.3). The Codex driver shells out to `codex exec --json`
  rather than binding the bidirectional JSON-RPC App Server.
- **OIDC** (§25.1). Authentication is bearer tokens hashed at rest; there is no identity
  provider integration.
- **Merge queue, PR integration, tracker sync, symbol/tree-sitter indexing** (§29, §30 phase 5).
- **Codex model ids** are still empty in `.conductor/models.yaml` until an operator names a
  verified Codex model. **OpenCode** is wired to local vLLM: Qwen 3.8 27B, GLM-5.3-Flash, and
  GLM-5.3 (`conductor serve qwen|flash|glm53`, then `conductor wrap opencode --model vllm/…`).

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
| `TestProbeUntrustedPeer` | a daemon certified by another CA can never pass as a peer |
| `TestDiscoverAddsPeersFromSRV` | a daemon joins the mesh via DNS alone, no `--peer` list required |
| `TestPeerInfoRequiresMeshCertificate` | a bearer token is not a peer credential; only a mesh certificate is |

---

## Peering daemons

A mesh is a set of `conductord` instances that know each other by certificate. One CA is
generated per mesh; every daemon holds a certificate signed by it and uses that single
certificate in two roles — served as its TLS certificate, and presented as its client
certificate when dialing peers. A peer link is therefore mutual TLS: each side proves it
holds a mesh-issued key, and the mesh CA is the only root either side trusts for it.

Peering carries connectivity and identity, nothing else. Each daemon probes its
configured peers (`GET /v1/peer/info`) and records state, round-trip time, and the
identity that answered; project members read that link table with `conductor peers`. No
tasks, scopes, or events cross the link — the database remains each daemon's own source
of truth.

```bash
# once per mesh: a CA, then one certificate per daemon
scripts/gen-peer-certs.sh laptop desktop

# per daemon (each points at the other)
conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/laptop/cert.pem --peer-key .conductor/certs/laptop/key.pem \
  --peer desktop=https://desktop.example.com:8443

conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/desktop/cert.pem --peer-key .conductor/certs/desktop/key.pem \
  --peer laptop=https://laptop.example.com:8443

conductor peers   # PEER ADDRESS STATE RTT LAST CHECK
```

Env equivalents: `CONDUCTOR_PEERS=name=url,…` (comma-separated) plus
`CONDUCTOR_PEER_CA`, `CONDUCTOR_PEER_CERT`, `CONDUCTOR_PEER_KEY`. Clients verify the
daemon with `CONDUCTOR_CA_CERT=.conductor/certs/ca.pem`. Peer URLs must be `https` —
a plaintext peer would put the mesh identity on an unauthenticated wire, and the daemon
refuses it.

### Joining a mesh without seed nodes

`--peer name=url` does not scale past a couple of daemons — every one needs every other
one's address by hand. `--peer-discover-dns` replaces that with one shared DNS SRV record
(RFC 2782): every daemon resolves it on each probe tick and treats each target as a peer
to dial, the same join-by-DNS pattern a Kubernetes headless service or Consul uses.

```bash
# publish an SRV record once, e.g. _conductor-mesh._tcp.mesh.internal pointing at every
# daemon's host:port — DNS round-robin, a headless Service, or a Consul/etc service catalog

conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/laptop/cert.pem --peer-key .conductor/certs/laptop/key.pem \
  --peer-discover-dns _conductor-mesh._tcp.mesh.internal

conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/desktop/cert.pem --peer-key .conductor/certs/desktop/key.pem \
  --peer-discover-dns _conductor-mesh._tcp.mesh.internal
```

No daemon names another by hand: a discovered address is dialed, and once it answers with
a certificate the mesh CA vouches for, it's identified by whatever name its own
`/v1/peer/info` reports — DNS only proposes where to look, never who to trust. `--peer` and
`--peer-discover-dns` compose freely (`conductor peers` marks a discovered link `(dns)`),
and a resolver hiccup only pauses discovery of *new* peers — it never drops one already
linked. Env equivalent: `CONDUCTOR_PEER_DISCOVER_DNS`.

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
