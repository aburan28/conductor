# Conductor × cairn

How the two systems fit, what each is missing that the other has, and the order to build it in.

Status: §4.1 (harness declaration) and the MCP-config merge it depends on are **implemented and
tested**. The `cairn-search` lane exists in `.conductor/dispatch.yaml`, a `cairn` harness is
declared in `.conductor/project.yaml`, and `conductor doctor` reports it.

**The worker path is proven end to end.** Driving the exact argv the exec driver builds, against
the exact MCP config the runner generates, an agent ran the full cairn loop on an isolated
ledger: `list_objectives` → `get_objective` → compute a candidate → `score_candidate` →
`accept` (n=837799, 524 Collatz steps), with the ledger still holding one entry afterwards
because scoring records nothing. Six turns, ~$0.10.

What has *not* run is conductor's own half: no control plane has been up, so no task was
claimed, no lease taken, no attempt recorded. That needs Postgres. §4.2 onward is still design.

Getting there surfaced a defect in cairn's MCP surface, now fixed — see §4.5.

---

## 1. The shape of the fit

cairn is a research network where verified results are the unit of account. Objectives are
funded questions carrying a hash-pinned verifier; an agent submits an artifact, the pinned
checker decides, and payment follows the checker rather than anyone's opinion.

The documented agent loop is:

```
list_objectives → get_objective → generate → score_candidate ×N → submit_claim
```

**`generate` is the one step with no cairn code behind it.** cairn owns no compute loop. There
is no job queue for local work, no worker pool, no parallelism over candidates, no retries, and
no scheduler. `cairn propose` is a sequential for-loop over artifact files the caller supplies;
it filters, it does not generate. `work_assignment` is HMAC arithmetic that tells a node which
slice of the search space is its own — it runs nothing and records nothing.

Conductor is the other half of that sentence. It is a control plane for dispatching agent work:
a task ledger with leases and fencing epochs, a policy-driven router with model ladders and
escalation, per-member token budgets, an admission queue, and runners spread across machines.

Neither system overlaps the other. cairn verifies and accounts; conductor dispatches and
governs. The seam between them is `generate`.

### 1.1 The evidence that this was already intended

`cairn/src/compute.rs` contains `WorkerCapability` (queue depth, active requests, max
concurrent, memory, estimated latency, attestation level), a `CapabilityScheduler::select()`
that picks the least-loaded worker clearing a requirement, and sealed `RequestEnvelope`s over
X25519 + ChaCha20-Poly1305. Its module doc states the purpose plainly: the building blocks
needed to *ask an inference-capable worker to produce a candidate artifact*.

That file is not compiled. There is no `mod compute;` in `cairn/src/lib.rs`, and no caller
outside its own `#[cfg(test)]` block. It is an orphan with tests that never run.

It is, in other words, a specification for conductor written inside cairn and then abandoned.
The correspondence is close enough to be worth tabulating:

| `cairn/src/compute.rs` (dead) | Conductor (live, tested) |
| --- | --- |
| `WorkerCapability{queue_depth, active_requests, max_concurrent, estimated_latency_ms}` | session capability advertisement; `coord_capabilities`; `conductor swarm` load column |
| `CapabilityScheduler::select` — min by `(queue+active, latency, worker_id)` | router candidate selection; `coord_delegate` offers work to the cheapest live session clearing a capability floor |
| `AttestationLevel` | runner-attested validation, evidence manifests |
| `RequestEnvelope::seal` | mTLS daemon peering (a different trust model — see §5.2) |

**The recommendation that follows from this: do not build a dispatch engine inside cairn.
Delete `compute.rs` or leave it dead, and let conductor be that layer over a process boundary.**

---

## 2. What each side gains

**cairn gains an execution layer it deliberately never built.** Concurrency control (cairn has
no `--jobs` flag and no thread pool), a model ladder so cheap local models do bulk search and
frontier models are reserved for stalled objectives, budget ceilings, and a durable record of
which attempts were made against which objective.

**Conductor gains a grader that is not an opinion.** Its own README names the gap: the planner
and reviewer services are unbuilt — "nothing yet invokes a model to decompose an objective or
review a diff." A cairn verifier is hash-pinned, deterministic, sandboxed, and returns
`accept` / `reject` / `unavailable` where `unavailable` explicitly does not mean rejection.
For objective-shaped work that is a strictly better merge gate than a reviewer model.

**Both gain something neither can build alone: spend governed by verified return.** Conductor
knows what an attempt cost. cairn knows what a result was worth, and knows it in a way nobody
can fake. Closing that loop is §4.3 and it is the reason to do any of this.

---

## 3. What does *not* transfer

Worth stating plainly, because conductor's headline feature is mostly irrelevant here.

**Scope-conflict detection is git-territory-shaped.** Reservations are over `dir:`, `file:`,
and the other resource types in the §11.3 matrix; the whole apparatus assumes contributors are
editing a shared working tree. cairn work is objective- and artifact-shaped. Two agents
searching the same objective are not editing the same files and will never produce a merge
conflict. What transfers is the dispatch/capability/budget/queue half of conductor, not the
territory half.

**Coordination-free overlap is a cairn design choice, not an oversight.** cairn declares slice
overlap a non-error: `docs/coordination.md` and `AGENTS.md` are explicit that a heterogeneous
fleet needs no scheduler, because the open network is adversarial and any coordination
primitive is something to attack. Conductor cannot change that, and should not try.

The distinction that keeps both true: **conductor coordinates one operator's own fleet.** Inside
a trusted swarm, deduplicating work across slices is a free efficiency gain. Across the open
network, cairn's assumption holds and conductor is simply absent. An integration that blurs
this — one that makes cairn correctness depend on conductor having scheduled something — is
wrong and will not be accepted upstream.

---

## 4. Build order

### 4.1 Register cairn as a harness — done

`harness.BuildRegistry` already ends in `default: reg.Register(NewExecDriver(name, cfg))`, and
`NewExecDriver`'s comment promises that "a new agent runtime needs a config block, not a pull
request." `HarnessConfig` carries `yaml:` tags on every field.

**That promise is currently unkept.** `BuildRegistry` is called in exactly two places
(`cmd/conductor/worker.go:113`, `cmd/conductor/setup.go:177`) and both pass
`harness.DefaultHarnessConfigs()` — a hardcoded map of `claude`, `codex`, `opencode`. No YAML
is ever consulted, so the `default:` branch is unreachable in practice and no custom harness
can be declared without editing Go.

The change was therefore to load harness configs from project YAML and merge them over the
defaults (`cmd/conductor/harnesses.go`). It is small, independently useful — it is the
documented behaviour — and every later step depended on it. A cairn worker is now
configuration, in `.conductor/project.yaml` under `harnesses:`.

**A second gap sat behind it.** `RunSpec` carries `MCPConfigPath` and `{mcp_config}` is a
supported placeholder, but `Runner.writeMCPConfig` generated a config containing exactly one
server — `conductor` — so a dispatched agent had coordination tools and nothing else. Pointing
the lane at cairn would have produced an agent with no `score_candidate`: the lane would launch,
burn tokens, and accomplish nothing.

Harness entries therefore take an `mcp_servers:` block, merged into the generated attempt config
alongside conductor's own. Two properties are load-bearing and covered by tests in
`internal/runner/mcpconfig_test.go`:

- **The conductor entry is written last and cannot be displaced.** A repository config that
  declared a server named `conductor` would otherwise be able to point a dispatched agent at a
  coordination channel nobody is reading — it would report progress into the void and still look
  healthy.
- **A harness that declares nothing gets exactly what it got before.** Repo-work agents have no
  use for cairn's tools and do not receive them.

Declaring these per harness rather than per project is deliberate: the harness is the runtime
identity, and "which tools does this runtime need" is a property of the runtime, not the repo.

### 4.2 A search lane, and conductor owning the candidate stream

The `cairn-search` lane is in `.conductor/dispatch.yaml`. Bulk search goes to a local vLLM
model at `max_concurrent: 1` (one GPU, one attempt); escalation walks up only when attempts
fail. `task.external_ref` holds the cairn objective id.

There is a gap on the cairn side this fills exactly. `partition::Assignment::covers(item_id)`
decides whether a candidate falls in this node's slice — and **it has no caller anywhere in
cairn outside its own tests.** The partition API is complete; the filtering step it exists for
was never implemented, because cairn leaves the candidate stream to the agent.

Mapping one conductor attempt to one cairn `node_id` makes the slice a unit of dispatch, and
puts the missing caller in the layer that owns the loop.

Constraint to respect: `node_id` must be **stable across the commit and the reveal**. cairn's
`submit_claim` is two calls — a commitment, then a reveal in a strictly later epoch — and
outstanding commitments are keyed on the submitter name. An attempt id that changes between
the two halves strands the commitment permanently, and a commitment nobody opens is never paid.
Key the submitter on the task, not the attempt.

### 4.3 Reward-aware routing — implemented

`policy.KnownFacts` is the closed list of names a `when` expression may read; the linter rejects
anything else. It is all task/attempt/budget shaped today.

Adding cairn-derived facts — `cairn.frontier_score`, `cairn.reward_remaining`,
`cairn.epochs_since_frontier_moved`, `cairn.settled` — makes routing answer questions no
LLM-shaped control plane can:

```yaml
- id: cairn-stalled-escalate
  when: task.labels has "cairn" && cairn.epochs_since_frontier_moved >= 8
  require: { tier: T3 }

- id: cairn-exhausted-stop
  when: task.labels has "cairn" && cairn.reward_remaining <= 0
  # nothing left to earn; stop spending tokens on it
```

This preserves conductor's central property — `when` expressions read deterministic facts, never
a prompt — because a cairn ledger fact is about as deterministic as a fact gets: it is
re-derivable by anyone with the log, and `cairn audit` proves it.

**Built:** `policy.CairnFacts` renders `cairn.known`, `cairn.objective_id`,
`cairn.frontier_score`, `cairn.reward_remaining` and `cairn.settled`;
`internal/cairn` reads them from `GET /objectives`; two rules in `.conductor/dispatch.yaml`
use them. Verified against a running node, not a mock.

Three things this got right that are worth not undoing:

**Absent facts render as nil, never zero.** The evaluator already treats a comparison against
nil as false ("a missing fact must not satisfy a threshold"), so an ordinary repository task
matches none of these rules. Had absence rendered as `0`, the entirely reasonable rule
`cairn.reward_remaining <= 0` would have fired on *every task in the project* and taken the
whole lane quiet for a reason nobody could see. `CairnFacts.Known` exists solely to hold that
line, and `TestAbsentCairnFactsSatisfyNoThreshold` is the guard.

**Read over HTTP, never over MCP.** A cairn node takes its ledger's exclusive write lock at
startup (§5.4). A router that opened the log to ask a question would be refused — or worse,
would be competing for the lock with the search worker that needs it. `cairn serve` answers
without contending.

**An unreachable node is `Known: false`, not a guess.** Routing while the ledger is down falls
back to the ordinary ladder. The one distinction preserved is between "this node does not have
that objective" (no error — it may not have synced) and "this endpoint returned something I do
not understand" (an error, because that is a misconfiguration somebody has to fix). Collapsing
those two was a real bug the tests caught: any JSON object at all unmarshals into a slice-typed
field and leaves it nil, so a wrong endpoint reported "no such objective" and looked healthy.

**Not built:** `cairn.epochs_since_frontier_moved`, the fact the escalation rule most wants. No
cairn endpoint reports it; deriving it means walking the log for the frontier claim's epoch. It
is deliberately *not* declared, because a fact the system can never populate is worse than a
missing one — `policy lint` blesses the rule that reads it, and the rule then silently never
fires.

### 4.4 cairn as a required check

Conductor runs required checks per attempt as `sh -c` in the worktree
(`internal/runner/runner.go:691`). A check whose command is a `cairn score_candidate`
invocation turns a merge gate into a pinned verifier's verdict.

One rule: map `unavailable` to *retry later*, never to failure. cairn is emphatic that a
verifier which could not run says nothing about the artifact, and a conductor check that treats
a missing toolchain as a rejection would import exactly the confusion cairn's status enum
exists to prevent.

---

### 4.5 The defect the first run found

The first end-to-end attempt failed in the most expensive way available: the agent ran, the
tools connected, the calls succeeded, and it reported **"the objective log is empty"** against a
ledger holding a funded objective. It then stopped, correctly, having accomplished nothing.

The cause was in cairn, not in the wiring. `Server::call_tool` attached

```json
"structuredContent": { "citations": [] }
```

to *every* successful tool result — the citation-capability mechanism, which hands back
unforgeable capabilities beside claim ids so a prompt-injected citation cannot be laundered
through prose. Sound idea. But none of cairn's ten tools declares an `outputSchema`, and the
protocol pairs `structuredContent` with one; a client seeing that field is entitled to treat it
as *the* result and ignore the text block beside it. Which is what happened: the agent read
`{"citations": []}` off `list_objectives` and concluded the network had nothing to work on.

The fix is to send the field only when there is a capability to carry
(`cairn/src/mcp.rs`, with a regression test). Answering "no objectives" and answering nothing
must not look the same on the wire.

Two things worth keeping from this. First, it is the same class of bug cairn's own
`scripts/mcp-smoke.sh` exists to catch — a server whose stdout is well-formed but whose meaning
is wrong — and the smoke test missed it because it asserts on `content` and never looks at
`structuredContent`. Second, it is invisible to unit tests on both sides: only a real client
choosing between two fields shows it. An integration is worth building partly because it is a
test neither system can run alone.

## 5. Constraints that will bite

### 5.1 cairn's write path is serial and lock-coupled

`Node::reveal` runs the verifier subprocess **while holding the ledger write lock**, and the
objective's declared timeout may be up to 24 hours. The daemon holds one `Mutex<State>` shared
by the accept thread, the tick loop, and the checkpoint write. There is no tokio in the crate.

So: fan-out is real on the *generation* side and fictional on the *admission* side. Conductor
can run fifty searches in parallel; their submissions will still admit one at a time. Route
submissions through the spool (`POST /submit`, `cairn drain`) rather than expecting concurrent
reveals, and do not promise throughput the ledger cannot deliver.

`score_candidate` is the exception and the model to copy: it clones the registry and drops the
guard specifically so scoring never contends. It is free, records nothing, and is designed to be
the inner-loop fitness function. Hammer it.

### 5.2 Determinism in the settling path is non-negotiable

cairn's `VerifierRegistry::interactive()` exists to cap a *throwaway* registry used by
`score_candidate`, and its doc explains why the cap is never applied to the settling registry: a
ceiling that changed a settlement would make the same claim settle differently on two nodes.

Any executor conductor puts in the settling path must honour the objective's own declared
timeout. Only the record-nothing paths — `score_candidate`, `propose`, gossip re-score — may
impose their own bounds.

### 5.3 Two processes, not one tree

Go and Rust; Postgres and a single-writer hash-linked JSONL ledger; and a licensing asymmetry
that matters: **cairn is Apache-2.0 and public** (`aburan28/distributed-researcher`), while
conductor's license is "not yet chosen" and it carries Apache-2.0 components from elsewhere.

Vendoring in either direction is a mistake. The integration surface is MCP stdio, `POST /submit`
into the spool, and `cairn drain --queue` — all of which already exist and are already the
supported way in.

This note lives in the conductor repository for the same reason: it describes conductor
internals, and cairn is the public one.

### 5.4 One ledger admits one MCP server, and that caps the lane at one worker

`cairn mcp` takes the ledger's exclusive write lock **at startup**, not lazily at first write.
A second process against the same `--log` exits 2 immediately:

```
cairn mcp: cannot open ledger …/cairn.jsonl: another process is already writing …
Two writers fork a hash-linked log -- both would append entries claiming the same predecessor.
```

This is observed behaviour, not a reading of the source. It has three consequences the lane
has to respect:

1. **`max_concurrent: 1` on the cairn candidates is load-bearing, not a GPU courtesy.** A
   second concurrent attempt would launch an agent whose cairn server dies on startup — and
   because the agent still starts, it would look like a working attempt that mysteriously has
   no tools.
2. **An interactive session holding the same ledger blocks the lane entirely.** A developer
   with cairn wired into their own Claude Code against `cairn.jsonl` owns the lock; a
   dispatched worker on that machine gets nothing. Same in reverse.
3. **Scaling past one worker means giving each its own ledger.** Workers search against private
   logs and submit through the spool (`POST /submit`) to one node that owns the real ledger and
   admits with `cairn drain`. That is the shape cairn already built for exactly this reason —
   the spool exists because "admission is a rules question, not a transport question" — and it
   is the only path to a fan-out the lane can actually use.

So the first end-to-end run is single-worker by construction, and multi-worker search is a
spool-shaped design problem rather than a concurrency knob.

### 5.5 Sandboxing is asymmetric

cairn jails pinned verifier code (bubblewrap on Linux, seatbelt on macOS), probes the mechanism
by actually executing under it, and `CAIRN_REQUIRE_SANDBOX` fails closed. Conductor's isolation
is git worktrees — file-level, not OS-level; `project.yaml` says `sandbox: none`, and
`networkDefault: deny` is only enforced where the harness happens to support it.

A conductor-dispatched cairn worker is therefore *less* confined than cairn's own verifier
subprocess. That is acceptable for candidate generation, which runs the operator's own agent
against the operator's own machine. It would not be acceptable to move verification into
conductor, and that is another reason verification stays where it is.

---

## 6. Open questions

- **Submitter identity per attempt.** §4.2 requires a stable `node_id` across commit and reveal.
  Task-scoped is the obvious answer; whether each task gets its own cairn identity file, or one
  node identity is shared with the task ref as a suffix, is unresolved.
- **Do cairn tasks take scope reservations at all?** §3 argues the territory machinery does not
  apply. But "objective X, partition 3 of 8" *is* a territory, and conductor's resource types
  might express it. That would give a trusted swarm real deduplication — at the cost of a
  concept cairn deliberately does not have.
- **Where does the objective→task decomposition live?** Conductor's planner service is unbuilt.
  A cairn objective is already a well-specified unit of work, so the first version can skip the
  planner entirely — one objective, one task, one lane.
