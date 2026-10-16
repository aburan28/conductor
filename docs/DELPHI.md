# Delphi

Delphi delegates a prompt to a frontier model through a provider **batch API**:
Anthropic Message Batches or the OpenAI Batch API. Batches cost half the interactive
price and finish within 24 hours, usually within minutes. That suits work that does not
need an answer this second: a second opinion on a design, a review of a long document,
an overnight sweep of questions, a sub-problem an agent wants a stronger model to think
about while it carries on.

It runs in two forms:

- **Hosted by conductord** (this page). Members and agents submit through the API or
  `conductor delphi`. Keys come from the project vault, and a background loop polls the
  providers. Spend lands in the usage ledger, and a linked task's timeline is told when
  the answer is in.
- **Standalone**: `cmd/delphi`, one binary with a local file-backed queue and keys from
  the environment, for use without a control plane. Run `delphi` with no arguments for
  its usage.

Both are built on `internal/delphi`: one provider-neutral engine, with providers in
`internal/delphi/anthropic` and `internal/delphi/openai` on the official SDKs.

## Connect

```sh
conductor secrets set ANTHROPIC_API_KEY      # and/or OPENAI_API_KEY (maintainers)
```

If an enabled inference endpoint of kind `anthropic` or `openai` names a different
secret (`conductor models endpoints add anthropic --key-secret NAME`), delphi uses
that one.

| provider | API | default model |
| --- | --- | --- |
| `anthropic` | Message Batches | `claude-mythos-5-1` |
| `openai` | Batch API over `/v1/responses` | none: give `--model` |

## Use

```sh
ref=$(conductor delphi ask --task T-42 --effort high "Is this reduction sound? ...")
conductor delphi show "$ref"               # state, tokens, cost
conductor delphi answer --wait "$ref"      # the answer, fetched from the provider
conductor delphi ask --wait --file question.md --provider openai --model <id>
conductor delphi list --state running
```

- **`--id REF`** names the request. Submitting the same request under the same ref again
  is a no-op, so retries are safe. A different request under a used ref is refused.
- **`--label k=v`** stores small tags with the delegation, such as ids, run numbers or
  paths. Labels are limited to 16, at 200 bytes each. They are for ids, not content.
- **`--task T`** links a task. When the answer is in, the task's timeline gets a
  `delphi.finished` event carrying the status and token counts, not the answer.

The API lives under `/v1/projects/{project}/delphi`:

| Route | What it does |
| --- | --- |
| `POST /` | Submit. The response is `201` (submitted), `200` (already submitted) or `202` (the provider's reply was lost; see below). |
| `GET /` and `GET /{ref}` | Metadata. |
| `GET /{ref}/answer` | The text, fetched from the provider now. |
| `POST /{ref}/cancel` | Cancel the batch. |
| `POST /{ref}/reconcile` | Resolve a lost submit. Maintainers only. |

Observers can read metadata. Contributors can submit, read answers and cancel.

## What is stored, and what is not

Conductor's database never holds prompts or model output (DESIGN.md §12.4), and delphi
keeps to that:

- **The request text is never written to the database.** It goes from the API straight
  to the provider, and the row keeps only a SHA-256 of it so that a resubmission can be
  recognised.
- **The answer is never written to the database either.** It is fetched from the
  provider whenever someone asks for it. The provider keeps results for about 29 days
  (Anthropic), or until the batch output file expires (OpenAI). Fetch answers you need
  to keep within that window.
- **The row (`delphi_delegations`) holds metadata only:**
  - ref, provider, model and batch id;
  - state and status;
  - token counts and cost;
  - error type and a short provider error message;
  - the refusal category;
  - labels.

Because the server keeps no text, **it never resubmits on its own**. A request that
expired, errored transiently or went missing is resubmitted by whoever holds its text. A
new ref is needed for that, because the old one is finished.

For the same reason, every request is submitted as a batch of one the moment it
arrives. The batch discount applies per request, so holding requests back to group them
would save nothing and would mean holding their text.

## Never billing twice

- **The intent is recorded first.** A row is written in state `submitting` before the
  provider is called.
- **A clear failure frees the ref.** If the provider answers the submit with an error,
  the row is deleted and the error is returned.
- **An unknown outcome is held.** If the outcome is unknown, for example because the
  connection dropped after the request was sent, the row stays `submitting` and the
  request is never sent again.
  - OpenAI batches carry the row's label in their metadata, and the loop finds them
    again.
  - Anthropic batches carry no metadata, so a maintainer reconciles the row: find the
    batch in the Anthropic console, created at that time, then run
    `conductor delphi reconcile REF --adopt BATCH_ID`, or `--abandon` if no batch
    exists.

## Spend

When a batch ends, its tokens are added to `usage_buckets` with source `delegation`,
harness `delphi`, and the provider and model. The cost uses batch list prices: Claude
prices are built in, and OpenAI prices come from `DELPHI_OPENAI_PRICES` in conductord's
environment (`{"model":[in,out]}`, $ per MTok). A model with no known price is recorded
unpriced, because catalog list prices would overstate batch spend by half. Delegated
spend is not counted by project budgets, which read attempts.

## Server settings

`--delphi-interval` (`delphi.interval` in the config file) sets how often open
delegations are checked. The default is `1m`, and a negative value turns delphi off.
See [OPERATIONS.md](OPERATIONS.md#delphi).

## Not built yet

- **MCP tools.** The tool list is capped at 15 (DESIGN.md §4.9) and is full; agents use
  the CLI.
- **Budgets.** Project budgets do not count delegated spend.
- **Automatic resubmission.** It would need the server to hold request text, which this
  design rules out.
