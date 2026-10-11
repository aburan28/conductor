# Conductor × measured bounds

How dispatch reads the ecbench bounds protocol -- a frontier of measured ECDLP method costs and
the verdicts of challenges against it -- as routing facts, so a goal can route "beat this
bound" work and escalate when a challenge has gone several epochs without an advance.

Status: the reader (`internal/bounds`), the facts (`policy.BoundsFacts`, rendered as
`bounds.*`), the `bounds:` block of `.conductor/project.yaml` and two rules in
`.conductor/dispatch.yaml` are **implemented and tested** against a real frontier document.
What is not yet done is the same thing not yet done for cairn: nothing attaches the facts at
the three places a `policy.Facts` is built (§5).

This is the shape of [cairn-integration.md §4.3](cairn-integration.md) applied to a second
source, and it is worth saying why the two are the same kind of thing before saying how they
differ.

---

## 1. Why a bound document is a fact

Conductor's central property is that a `when` expression reads deterministic facts -- scopes,
attempt history, budget position -- never a prompt. A fact that comes from outside the ledger
has to earn its place by being as deterministic as the ledger.

The bounds protocol (aburan28/crypto, `docs/bounds/README.md`) writes four kinds of sealed
record, two of which matter here:

| record | schema | what it says |
| --- | --- | --- |
| **frontier** | `ecbench.frontier/v1` | per domain (problem, curve family, unit, tier, resource envelope), the measured bounds no other bound dominates: built by `ecbench frontier build` from committed bound records, never edited, regenerated and checked in CI |
| **verdict** | `ecbench.verdict/v1` | one paired session judged against a frozen challenge in a named epoch: `advances`, `trade`, `matches`, `regresses` or `inadmissible`, with the axes it moved on and the level (`exponent` or `constant`) when the advance includes operations |

Both are **content-addressed**. A record's id is a prefix plus twelve hex digits of the SHA-256
of its own bytes with the id field blank; a changed byte is a different record, and two
sessions that fit the same inputs write the same bytes. A frontier is re-derivable by anyone
holding the bound records, and a verdict by anyone holding the session: run `ecbench challenge
verdict` again on the same session and you get the same `ECVD1h…`.

That is at least as deterministic as a cairn ledger entry, and more checkable from here: the
reader verifies the seal of every document before reading anything from it, byte for byte, the
way the harness's own `read_sealed` does. It does not -- cannot -- re-fit the bound from its
sessions; that is `ecbench bound check`, and the other repository's CI runs it on every record.
The seal check catches a hand-edited or truncated document; the re-derivation catches a wrong
one. Conductor relies on the first and trusts the second to the CI that owns the records.

Two rules of the protocol carry straight through into the facts:

- **Unknown is not zero.** A verdict axis one arm did not report is left out of the comparison;
  a challenge nobody has run has no epochs. The facts render absence as nil (§3), never as `0`.
- **Wall time never enters.** Every figure is in the harness's counted unit. Nothing here reads
  a clock, and a rule cannot be written against one.

## 2. Configuration

```yaml
# .conductor/project.yaml
bounds:
  frontier: ../crypto/docs/bounds/frontier.json     # one ecbench.frontier/v1 document
  verdicts: ../crypto/research/*/verdict.json       # a glob of ecbench.verdict/v1 documents
```

Paths are relative to the repository root unless absolute. The glob is `filepath.Match`
syntax: one `*` per path element and no `**`, which is why the shipped pattern names the
`research/<topic>/verdict.json` layout the protocol's own walkthrough uses.

**Absence is not an error and yields nothing known.** No `bounds:` block, a frontier path
with no file behind it, or a glob matching nothing each produce a store whose facts are all
absent -- the same shape as "this project reads no bounds", deliberately, so that routing in a
worktree that has not generated the document falls back to the ordinary ladder rather than
acting on a guess about a frontier.

**Presence with a problem is an error, and still nothing known.** A document that cannot be
read, does not seal, carries another schema, names an outcome the reader does not know, or
names a leader bound it does not list is a misconfiguration somebody has to fix, and the reader
says which file and why. One broken verdict under the glob fails the whole set, for the reason
the harness's own loader gives: a set summarised without one of its verdicts may be missing
the one that advanced, and a frontier built from a directory with a broken record is wrong, not
smaller. The caller holding the error gets a nil store, whose facts are absent.

The same verdict under two paths -- a committed copy beside a research copy -- is read once:
equal ids are equal bytes.

## 3. The facts

`policy.BoundsFacts` renders eight names. Two are presence flags and are plain booleans, so
`!bounds.known` reads naturally; the other six are **nil unless their flag is true**, and the
evaluator treats a comparison against nil as false, so no threshold is ever met by a fact
nobody supplied. `TestAbsentBoundsFactsSatisfyNoThreshold` is the guard.

| name | type | definition | nil when |
| --- | --- | --- | --- |
| `bounds.known` | bool | a frontier document was read | never (false) |
| `bounds.frontier_id` | string | the frontier's sealed id, `ECFR1h…` | `!bounds.known` |
| `bounds.domain_count` | number | domains the frontier compares within | `!bounds.known` |
| `bounds.frontier_entries` | number | bounds on the frontier across every domain: those with `is_frontier`, which nothing dominates -- not every bound the document lists | `!bounds.known` |
| `bounds.challenge_known` | bool | the task names a challenge, and at least one verdict against it was read | never (false) |
| `bounds.challenge_epochs` | number | distinct epochs holding a verdict | `!bounds.challenge_known` |
| `bounds.epochs_without_advance` | number | distinct epochs later than the last `advances` verdict; every epoch seen when nothing has advanced yet | `!bounds.challenge_known` |
| `bounds.last_outcome` | string | the latest verdict's outcome: `advances`, `trade`, `matches`, `regresses` or `inadmissible` | `!bounds.challenge_known` |

Three definitions deserve a sentence each.

**`bounds.known` is about the project, not the task.** The frontier is the established cost of
every method in every domain -- a property of the world the project works in, like the budget
is a property of the project -- so every task sees the same one. A rule meant for bounds work
only guards itself with `task.labels has "bounds"`, and for the hold rule that guard is
load-bearing (§4). The cairn facts are all task-scoped because cairn has no project-level
object; the frontier is one.

**Epochs are counted, never subtracted.** A challenge's epochs are whatever integers nobody has
used for it before (`challenge spec --epoch N` derives the targets from the nonce and `N`), so
the difference between two epoch numbers says nothing about how many sessions were run; the
number of distinct epochs holding a verdict does. An `inadmissible` verdict counts: the epoch
was spent, whatever the session turned out to be worth.

**Two verdicts in one epoch** are ordered by id, which is deterministic for a given set of
files and nothing more. `bounds.last_outcome` is the later one's; an `advances` among them
still resets `bounds.epochs_without_advance` to zero, whichever sorts last.

The reader exposes more than the facts render -- per domain, the ops and memory leaders'
bound, method and value, and per verdict the axes it advanced on and the level it moved. Those
are Go fields on `bounds.Frontier` and `bounds.Challenge`, not `when` names, for the reason
§4.3 of the cairn note gives for leaving `cairn.epochs_since_frontier_moved` undeclared: a
name should be added when a rule needs it, not before, because a declared fact that nothing
ever reads is noise in `policy lint` and a declared fact that nothing can populate is worse.

## 4. How a task names its challenge, and the rules

A task carries the challenge id (`ECCH1h…`) in `task.external_ref` -- the slot a cairn task uses
for its objective id -- and the label `bounds`. The caller that builds `policy.Facts` passes
`task.ExternalRef` to `Store.Facts`; a task with no ref, or one naming a challenge with no
verdicts, gets the frontier facts and no challenge facts.

The two rules in `.conductor/dispatch.yaml`:

```yaml
- id: bounds-stalled-escalate
  when: bounds.challenge_known && bounds.epochs_without_advance >= 3
  require: { tier: T3 }

- id: bounds-unknown-frontier-hold
  when: task.labels has "bounds" && !bounds.known
  prefer: { tag: local }
```

The first is the rule this whole thing exists for: a challenge that has gone three epochs
without an advance has shown what the cheap end of the ladder finds, and the next epoch gets
the frontier model. `bounds.challenge_known &&` is belt and braces, as `cairn.known &&` is in
the cairn rules -- an absent fact satisfies no threshold -- kept because it says out loud what
the rule is about.

The second holds bounds work at the cheap end of the ladder when there is no frontier to read:
without the document, "beat this bound" has no bound in it. Here the label guard is not
decoration. `bounds.known` is false for *every* task in a project without a frontier, so the
rule without its guard would prefer local on everything. Dispatch has no verb to refuse work
and should not grow one for this: a missing document is a configuration gap, never evidence
about the bound.

Both rules lint clean against the shipped policy (`TestRepositoryDispatchPolicyLintsClean`),
and the escalation rule is exercised end to end -- three verdict files, none an advance, to a
`when` expression that fires -- in `TestNeverAdvancedCountsEveryEpoch`.

## 5. Not built

- **Attaching the facts.** `policy.Facts` is built in three places
  (`internal/coord/dispatch.go`, `internal/runner/runner.go`, `internal/router/router.go`) and
  none of them sets `Cairn` or `Bounds` yet. When that is done it should be done for both at
  once, from the project's loaded `config.Bundle` -- `bounds.Load(bundle.Root, …)` once per
  routing decision is a file read and a hash, cheap enough -- and the facts recorded on the
  decision so that "why did this route to T3" has the epoch count in it.
- **A `conductor doctor` line.** Doctor reports the harnesses a project declares; it could
  report whether the configured frontier resolves and seals, which is the one thing an
  operator cannot tell from a quiet rule.
- **The frontier as a cairn objective.** The protocol notes that cairn holds the same frontier
  as a ratchet objective per domain. When a node serves it, `cairn.frontier_score` and
  `bounds.frontier_entries` describe one object from two sides, and should agree or one of
  them is wrong.
