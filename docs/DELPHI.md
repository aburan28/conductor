# Delphi

Delphi delegates a prompt to a frontier model through a provider **batch API**:
Anthropic Message Batches or the OpenAI Batch API. Batches cost half the interactive
price and finish within 24 hours, usually within minutes. That suits work that does not
need an answer this second: a second opinion on a design, a review of a long document,
an overnight sweep of questions, a sub-problem an agent wants a stronger model to think
about while it carries on.

It runs in two forms:

- **Hosted by conductord** (this page). Members and agents submit through the
  Conductor UI, API, or `conductor delphi`. Anthropic can use the server's
  project-scoped workload identity, while other projects and OpenAI use project
  vault keys. A background loop polls the providers. Spend lands in the usage
  ledger, and a linked task's timeline is told when the answer is in.
- **Standalone**: `cmd/delphi`, one binary with a local file-backed queue and
  credentials from the environment (API key or Anthropic workload identity
  federation), for use without a control plane. Run `delphi` with no arguments
  for its usage.

Both are built on `internal/delphi`: one provider-neutral engine, with providers in
`internal/delphi/anthropic` and `internal/delphi/openai` on the official SDKs.

## Connect

### Standalone Delphi with Anthropic workload identity

Build or install both `conductor` and the standalone `delphi` binary on the
laptop. After registering the local issuer and federation rule as described in
[ANTHROPIC_WIF.md](ANTHROPIC_WIF.md), launch the entire Delphi process through
the WIF wrapper:

```sh
conductor wif exec \
  --provider local \
  --issuer https://oidc.YOUR-DOMAIN.example/conductor \
  --subject conductor:laptop:YOUR-LAPTOP \
  --rule-id fdrl_YOUR_RULE \
  --organization-id YOUR_ORGANIZATION_UUID \
  --service-account-id svac_YOUR_SERVICE_ACCOUNT \
  --workspace-id wrkspc_YOUR_WORKSPACE \
  -- delphi run -prompt "Give a concise answer." -out ./delphi-run
```

`delphi run` uses `claude-mythos-5-1` by default; `-model` overrides it.
The wrapper creates a short-lived signed assertion in a private temporary file
and refreshes it while Delphi runs. The Anthropic Go SDK exchanges that
assertion for an access token and authenticates Message Batches with a bearer
token. No API key is needed. The federation rule needs the
`workspace:developer` OAuth scope and access to the selected workspace;
Mythos availability is a separate model entitlement.

For a persistent local queue, wrap `delphi serve` the same way. Queue work with
`delphi ask -prompt "..."` and read a finished response with `delphi get -wait
ID`. The `conductor delphi ask` command below uses the hosted `conductord`
service. A WIF wrapper on the laptop only changes hosted authentication if it
also starts that `conductord` process as described below.

### Hosted Delphi

Open the Conductor dashboard and choose **Delphi** in the navigation. Pick a
model-catalog profile or enter a provider and model ID, set effort and the
output-token ceiling, enter a question, then select **Launch batch**. The page
shows progress and offers **Read answer** when the batch succeeds or refuses. It keeps
question drafts and fetched answers in browser memory only; navigation or reload
discards them. The server stores request metadata but not question or answer text.
Contributors and higher project roles can launch and read answers; observers
and reviewers see metadata.

For an Anthropic project using workload identity, configure the issuer and
federation rule in [ANTHROPIC_WIF.md](ANTHROPIC_WIF.md). Set the **Conductor
project ID** from Settings, then start the server through the WIF wrapper on
the same machine as the server:

```sh
export CONDUCTOR_DELPHI_WIF_PROJECT_ID='YOUR_CONDUCTOR_PROJECT_ID'
conductor wif exec \
  --provider local \
  --issuer https://oidc.YOUR-DOMAIN.example/conductor \
  --subject conductor:laptop:YOUR-LAPTOP \
  --rule-id fdrl_YOUR_RULE \
  --organization-id YOUR_ORGANIZATION_UUID \
  --service-account-id svac_YOUR_SERVICE_ACCOUNT \
  --workspace-id wrkspc_YOUR_WORKSPACE \
  -- conductord --config ./leader.yaml
```

`conductord` must be on `PATH`; use the usual server config and database.
The wrapper rotates a short-lived identity assertion file while the server
runs. For that one project, hosted Delphi uses Anthropic federation and never
opens its vault API key. Other projects continue to use their own vault keys;
OpenAI still uses its project vault key. Do not set `ANTHROPIC_API_KEY`,
`ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_PROFILE`, `ANTHROPIC_CUSTOM_HEADERS`,
`ANTHROPIC_BASE_URL`, or `ANTHROPIC_IDENTITY_TOKEN` on
the server process: hosted WIF rejects those overrides. The Anthropic service
account must belong to the selected Anthropic workspace, and that workspace
must have access to the requested model. If the server runs remotely, its own
host needs an available WIF identity provider; a browser or laptop token does
not supply credentials to a remote server.

For projects using API keys instead, install keys in the project vault:

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

### Detached goal loop

The standalone `delphi` binary can pursue one written goal through bounded
plan, draft, and review rounds. Each stage is a provider batch request. A
reviewer's `revise` verdict feeds specific gaps into the next plan; `accept`
ends the loop. The result is labeled `self_reviewed`, since model review is
not independent verification. At the round limit an unaccepted draft is
labeled `needs_review`. Delphi does not alter its own code or expand the
configured limits. Anthropic Haiku 5.5 handles planning and drafting by
default, with Sonnet 5.5 reviewing; choose other provider/model IDs with the
flags shown below when a goal calls for them.

```sh
delphi goal start -goal-file ./goal.txt -out ./my-goal \
  -provider anthropic -model claude-haiku-5-5 \
  -reviewer-provider anthropic -reviewer-model claude-sonnet-5-5 \
  -rounds 3 -max-tokens 8192
delphi goal status -out ./my-goal
delphi goal stop -out ./my-goal
delphi goal run -out ./my-goal       # resume a stopped or interrupted run
```

`goal start` launches a background `delphi goal run` process through `nohup`
on Unix and writes its
progress to `goal.json`, individual provider requests and results to the run
directory, and operational messages to `runner.log`. `answer.md` contains the
last draft when the loop ends. Reusing the directory resumes stable request
IDs, so a completed stage is not submitted twice. Keep this directory private:
it contains the goal, plans, drafts, reviews, and provider answers.

For Anthropic workload identity, keep the WIF rotator alive with the runner.
Initialize the goal, then detach the **wrapper process** using your normal
service manager or `nohup`:

```sh
delphi goal init -goal-file ./goal.txt -out ./my-goal -rounds 3
nohup conductor wif exec --provider local \
  --issuer https://oidc.YOUR-DOMAIN.example/conductor \
  --subject conductor:laptop:YOUR-LAPTOP \
  --rule-id fdrl_YOUR_RULE --organization-id YOUR_ORGANIZATION_UUID \
  --service-account-id svac_YOUR_SERVICE_ACCOUNT \
  --workspace-id wrkspc_YOUR_WORKSPACE \
  -- delphi goal run -out ./my-goal \
  > ./my-goal/wif.log 2>&1 < /dev/null &
```

The wrapper refreshes the assertion for the whole run. `goal start` refuses
to detach when it detects an assertion file from a parent WIF wrapper, since
the parent would otherwise exit and stop refreshing that file. Provider
entitlement and workspace access still determine whether a model call succeeds.

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

## Tiers

Different work can go to different models. A **profile** is a model-catalog alias
(`.conductor/models.yaml`, the same catalog agents are routed by) that names an Anthropic
or OpenAI model and an effort:

```yaml
profiles:
  - alias: implementer          # cheap and fast, thinking as hard as it can
    harness: delphi
    provider: anthropic
    model: claude-haiku-5-5
    reasoning_effort: max
    billing: tokens
  - alias: frontier
    harness: delphi
    provider: anthropic
    model: claude-mythos-5-1
    reasoning_effort: high
    billing: tokens
```

```sh
conductor delphi profiles                         # the tiers delphi can use
conductor delphi ask --profile implementer --file spec.md
conductor delphi ask --profile frontier --effort max "..."   # flags override the profile
```

- Every enabled `anthropic` or `openai` profile is a tier, one per alias. Under one alias,
  a profile with `harness: delphi` wins over agent profiles (`harness: claude`, ...), so a
  delegation tier can differ from the agent model of the same name. The agent router
  ignores `harness: delphi` profiles.
- `reasoning_effort: none` sends no effort.
- The delegation records its `profile`, so spend can be read per tier.

## Task cards: the director and mechanical tiers through delphi

The owner's research repositories (crypto-autoresearcher, crypto, cryptanalysis) split an
experiment between a director (Sonnet) that writes a frozen task card and a mechanical
executor that runs it. Delphi carries the director's two model calls at batch price;
`tools/task_card.py` in those repositories does the rest:

```sh
python3 tools/task_card.py request compile --design spec.md --bind tools/x.py --out card.yaml > compile.md
conductor delphi ask --profile director --task T-42 --wait --file compile.md > reply.md
python3 tools/task_card.py ingest reply.md --expect card --out card.yaml      # only if it lints
python3 tools/task_card.py run card.yaml                                      # no model
python3 tools/task_card.py request assess card.yaml > assess.md
conductor delphi ask --profile director --task T-42 --wait --file assess.md > reply.md
python3 tools/task_card.py ingest reply.md --expect assessment --card card.yaml --out assessment.yaml
```

- A delegation is one turn with no tools, so `request` writes every fact into the prompt
  (commit, file digests, the contract, the receipt) and `ingest` refuses a reply that does
  not lint or whose outcome the facts contradict.
- The mechanical tier spends no tokens here: `task_card.py run` executes the card and writes
  its receipt. `--profile executor.mechanical` (Claude Haiku 5.5, effort low) is for
  mechanical work that is text, not commands.
- `--profile director` resolves to the `harness: delphi` director profile in
  `.conductor/models.yaml`: Claude Sonnet 5.5 at high effort. The agent profile of the same
  alias stays at medium for routed sessions.
- With `--task`, each delegation's `delphi.finished` event lands on the task's timeline.

## Fallback on refusal

A model's safeguards can decline a request (status `refused`, with a category such as
`cyber`). That is common on security research. `--fallback` resubmits the same request to
the next target in order until one answers:

```sh
conductor delphi ask --wait --profile frontier \
    --fallback "openai:<model>@high,anthropic:claude-opus-5-5" --file probe.md
```

- A target is `provider:model[@effort]` or a profile name. The default chain is
  `$CONDUCTOR_DELPHI_FALLBACK`.
- Each resubmission is a new delegation, `REF-fb1`, `REF-fb2`, and so on. It records
  `fallback_of` (the ref that refused), and the server accepts `fallback_of` only for a
  delegation that was refused.
- When the provider changes, `params` and effort are dropped, because they were written
  for the other API; the target's own `@effort` applies.
- The CLI does the resubmitting, so `--fallback` needs `--wait`. Holding the request text
  is the caller's part; the server keeps none (below).

The standalone binary falls back on its own, because it holds the text: `delphi run
-profile frontier`, `-fallback ...`, `$DELPHI_FALLBACK`, or a profile's `fallback` list in
`~/.delphi/profiles.json` (`$DELPHI_PROFILES`):

```json
{"implementer": {"provider": "anthropic", "model": "claude-haiku-5-5", "effort": "max"},
 "frontier": {"provider": "anthropic", "model": "claude-mythos-5-1", "effort": "high",
              "fallback": ["openai:<model>@high", "implementer"]}}
```

The request is resent under the same id. Its result lists the models that declined in
`fallback_from`, and a target whose provider has no key is skipped and noted there.
Resubmitting the original request after a fallback is still recognised as the same
request.

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
- **Automatic resubmission**, including server-side fallback. It would need the server to
  hold request text, which this design rules out. The CLI and the standalone binary do it
  instead.
- **Fallback chains in the catalog.** `models.yaml` profiles have no fallback list yet;
  the chain comes from `--fallback` or `$CONDUCTOR_DELPHI_FALLBACK`.
