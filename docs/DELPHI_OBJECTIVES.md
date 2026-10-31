# Delphi objectives: hierarchical delegation with measured model equivalence

Status: design. Built so far: phase 1 (OpenAI-compatible endpoints), phase 2 (the registry,
`delphi calibrate` and `delphi registry`, with the director suites in crypto-autoresearcher),
free mode, and phase 3's router (`delphi route`, `-route`); `task_card.py lane` and the goal
loop still name fixed models. See `docs/DELPHI.md`. Owner decisions (2026-10-30): the engine
lives in Delphi; model equivalence rests on public benchmarks as a prior plus our own
calibration suites; OpenAI-compatible endpoints come first.

## Goal

Give Delphi a broad objective ("cut the index-calculus relation cost at n=73", "audit every
Haiku-run session this week") and have it decompose the objective into a tree, send each
node to the cheapest model that is measured to be good enough for that kind of work, check
the result independently, and escalate only what fails. Frontier models decide, core models
plan and review, directors write exact instructions and assess, mechanical models execute and
measure. Every routing choice cites a number, and every number cites its source.

Delphi proposes and executes. In the research repositories only the Coordinator changes
research state (crypto-autoresearcher AGENTS.md); an objective tree there produces evidence and
recommendations, never a hypothesis status.

## 1. The objective tree

```
Objective            broad statement, acceptance criteria, budget, owner
 └─ Sub-objective     narrower statement, its own criteria and budget share
     └─ Work item     one delegation: a family, a required level, inputs, a check
```

Every node records: id, parent, statement, **acceptance criteria that a tool or a second model
can check** (a printed line, an exit status, a test, a rubric with a pass threshold), task
family (§3), required level, budget (tokens and dollars), state (`open`, `delegated`,
`checking`, `accepted`, `escalated`, `failed`, `abandoned`), the delegation that answered it,
and the evidence (prompt and answer sha256, check output). Records are write-once events; a
node's state is the fold of its events, like Delphi's flights today.

Decomposition is itself a work item (family `plan`), done by a frontier or core model, and its
output is checked mechanically before any child runs: every child has criteria, a family, and a
budget that sums to no more than the parent's. A tree has a maximum depth and fan-out, and a
node that cannot be decomposed into checkable children is sent back up, not guessed at.

## 2. Delegates: what a work item can be sent to

| Delegate | Use | Exists today |
|---|---|---|
| Batch call (Anthropic Message Batches, OpenAI Batch/Responses) | single-turn reasoning at half price | yes, Delphi |
| Live call (any OpenAI-compatible endpoint: Fireworks, DeepSeek, Together, OpenRouter, vLLM GLM/Qwen) | open models, providers without batch | **yes, phase 1** (standalone `delphi`) |
| Task card (`tools/task_card.py lane`) | run exact commands, no model judgment | yes, research repositories |
| Agent harness (Codex, Claude Code through Conductor's harness) | edit a repository, multi-turn tool use | yes in Conductor, not reachable from Delphi |

## 3. Task families and the capability registry

A **task family** is a kind of work with its own way of being checked. Initial families:
`plan` (decomposition), `card-compile` (write a task card that lints), `assess` (classify an
execution against its record), `code-edit` (a patch that passes tests), `proof` (Lean that
compiles with no new axioms), `math` (a checkable derivation or computation), `extract`
(structured data from text), `summarize`, `review` (find defects in a diff).

The **capability registry** holds, per model and family:

- **Public evidence (the prior).** Records of public benchmark results: model id, benchmark,
  benchmark version or date, score, interval if published, source URL, retrieval date, and
  whether the number is self-reported by the vendor or independently measured. Candidate
  sources include SWE-bench Verified, LiveCodeBench, Aider polyglot, GPQA Diamond, Humanity's
  Last Exam, LiveBench, LMArena and Artificial Analysis; which ones are used, and how each maps
  onto a family (a declared weight, e.g. SWE-bench → `code-edit`), is configuration with its
  own review, never a constant in code. Nothing is imported without its source and date.
- **Calibration (the measurement).** For each family, a frozen suite of items with
  deterministic graders that already exist in the repositories: `task_card.py lint` and
  `ingest` for `card-compile`, `check-relay` agreement for `assess`, the repository's tests for
  `code-edit`, `lake build` plus the axiom audit for `proof`. Suites run through Delphi itself
  (batch or live), so measuring a model costs what using it costs. Each run records pass rate
  with a Wilson 95% interval, mean tokens, and cost per accepted answer.
- **Equivalence.** Two models are *equivalent on a family at margin δ* when a two one-sided
  test (TOST) on their calibration pass rates rejects a difference of δ or more (default δ = 5
  points, 90% intervals). Model A *dominates* B on a family when A is not worse beyond δ and
  costs less per accepted answer. A model with only public evidence is labelled
  `prior-only`; it may be routed only to families marked low-stakes until it is calibrated.
  A model never inherits a calibration it did not run (no "GLM-5.3 ≈ Sonnet" by analogy).
- **Output.** Per family, a table of models with pass rate and interval, cost per accepted
  answer, equivalence classes, and the date of every number; it is what the router reads and
  what a person reads to see why a node went where it did.

## 4. Delegation policy

For each work item: take the models whose calibrated pass rate on the item's family meets the
required level (lower interval bound ≥ level), choose the cheapest per accepted answer, prefer
batch when the item's deadline allows, and send it. Then check:

1. The node's own check (tool output or rubric).
2. For families marked high-stakes, an independent review by a model from a **different
   family of models** than the author's (a Claude-written answer is reviewed by GPT or GLM and
   vice versa), so one vendor's blind spot cannot pass itself.
3. On failure, refusal or reviewer disagreement: escalate one tier up, at most twice, then mark
   the node `failed` with every attempt attached. Escalations and their costs feed back into
   the registry as observations, not as silent re-scoring.

Budgets are enforced at every level; a node that would exceed its share escalates to its
parent for a decision instead of spending.

## 5. Provider matrix

| Provider | Path | Batch | Status |
|---|---|---|---|
| Anthropic (Haiku, Sonnet, Opus, Fable, Mythos) | native | yes | built |
| OpenAI (Luna, Terra, Sol; Codex models) | native Responses | yes | built |
| OpenAI Codex agent | Conductor harness | n/a | built in Conductor, delegate in phase 5 |
| Fireworks, Together, OpenRouter | OpenAI-compatible | live | **phase 1** |
| DeepSeek | OpenAI-compatible | live | **phase 1** |
| GLM, Qwen on vLLM / LM Studio | OpenAI-compatible | live | **phase 1** |
| Fireworks batch | native batch | yes | phase 6 |
| Google Gemini | native | yes | phase 6 |

## 6. Phases

1. **OpenAI-compatible endpoints** for standalone `delphi` (built: `internal/delphi/compat`).
2. **Capability registry**: schema, public-evidence importer with source and date on every
   record, calibration runner, equivalence tables (built: `internal/capability`); then the
   `card-compile` and `assess` suites in the research repositories, graded by `task_card.py`.
3. **Delegation policy**: route by the registry (built: `delphi route`, `-route`); then
   `task_card.py lane` and the goal loop use it instead of fixed models.
4. **Objective tree** in standalone `delphi` (`delphi objective start|status|resume`), durable
   like the goal loop, with decomposition checks, budgets, independent review and escalation.
5. **Control plane**: objectives as Conductor tasks with timelines; compatible endpoints on the
   control plane through an asynchronous worker (a live call must not hold an API request
   open); Codex and Claude Code harness delegates for `code-edit`.
6. **More providers**: Fireworks batch, Gemini.

## Open questions

- Which public sources to trust per family, and their weights (needs a review per source).
- δ and the required level per family; the research repositories' families likely want a
  tighter δ than `summarize`.
- Whether calibration suites live in Conductor (generic) or in each repository (domain items),
  or both with a shared schema.
