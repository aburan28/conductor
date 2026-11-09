# Cost-aware model orchestration

This is the owner's routing target for Conductor and the crypto-autoresearcher
harness. A model name in this table is a preferred role, not proof that a
provider, credential, runtime, or review gate is ready. Dispatch must record the
resolved provider, model, policy, price or capacity basis, and reason for each
escalation.

| Work | First eligible route | Escalation or independent check |
| --- | --- | --- |
| Public, bounded drafting and mechanical work | Available local capacity or an allowed free model | Haiku 5.5 when the cheap route is unavailable or fails its checks |
| Frozen experiment execution and artifact collection | Haiku 5.5 or a verified cheaper executor with the same required tools | Independent Sonnet 5.5 validation after the producer's snapshot |
| Routine coding and evidence review | Sonnet 5.5 | Opus 5.5 for complicated code or judgment |
| Difficult options and research judgments | Opus 5.5, then Fable 5.1 as the problem warrants | Mythos 5.1 for the hardest decisions or unresolved disagreement |
| Claim-changing cryptographic review | The research harness's required independent review policy | Its nondegradable breakthrough policy when triggered; price never lowers this floor |

The order is a workload policy, not a claim that a model is intrinsically more
capable than every model below it. A failed response does not automatically
justify an expensive retry: classify provider failure, missing tool support,
failed validation, and genuine judgment uncertainty separately. Put a per-task
spend ceiling on paid routes, and report when the ceiling prevents a route.

The implemented Conductor `implement` lane now tries an approved OpenRouter
free model for public low-tier tasks, then the `worker.fast`, `worker.general`,
and `planner.frontier` aliases. The merged catalog has both local vLLM and
Anthropic profiles under these aliases; an alias does not guarantee a specific
provider or a live local endpoint. A router floor can skip a cheaper candidate.
This does not implement automatic question classification or the research
harness's experiment route; those are delivery gates below.

## Provider paths and readiness

| Provider | Current path | Gate before use |
| --- | --- | --- |
| Anthropic | Claude agent harness or Delphi Message Batches | Probe the requested model and authentication. A Claude Code Haiku 5.5 probe on this laptop failed on 2026-10-17 because the CLI OAuth session could not refresh; that agent route is not live-ready until login is repaired. Standalone Delphi already supports workload federation; hosted Delphi still uses its configured project credential. |
| Local vLLM | OpenCode agent harness | Require a running endpoint and exact model match. OpenCode being installed does not establish endpoint readiness. The implementation lane does not name local vLLM, but the tier router can still choose an enabled local catalog profile; disable it on a runner without the endpoint until runtime-aware filtering exists. |
| OpenRouter free models | OpenCode agent harness | `openrouter/poolside/laguna-xs-2.1:free` returned a synthetic answer and used a file-write tool in an isolated directory on 2026-10-17. Its provider says free-tier inputs and outputs may be used for model improvement. The implementation lane tries it only for a task explicitly labeled `public-free-ok`, with at most three estimated files and no security or cryptography sensitivity, subject to the router's tier floor. The free quota may be exhausted; failed attempts escalate under the dispatch policy. |
| OpenCode Console (Zen) Muse Spark | Delphi `opencode` preset (OpenAI-compatible, `OPENCODE_API_KEY`) or OpenCode with Console credentials | On 2026-10-30 the live model list (`https://opencode.ai/inference/v1/models`) held paid `muse-spark-1.3` and `muse-spark-1.2` ($1.25 in / $4.25 out per MTok) and no free Muse Spark; the earlier `muse-spark-1.3-contributor-free` is gone. Console's docs say its free models answer only inside OpenCode, not from other clients such as Delphi, and that some free models may use prompts to improve the model. Muse Spark's native API there is Responses; Console translates other APIs, and `/inference/v1/chat/completions` answers an unauthenticated call with 401, so Delphi's Chat Completions path exists, though no keyed call has been made from here yet. |
| Fireworks | OpenCode agent harness or the research harness's existing adapter | Probe the exact model and tool behavior; the research harness currently has a configured Fireworks backend. |
| Abliteration | OpenCode has a local login and lists three models; research harness provider configuration is in an uncommitted checkout | Confirm that this is the provider the owner meant by “obliteration,” then coordinate with the owner of those changes. Its API is metered after a small preview, so it needs a price and spend cap before automatic routing. Do not treat uncommitted bindings as deployed. |

Conductor's model discovery presets for OpenRouter and Fireworks list model
names; they do not provide completion transport. The OpenCode agent needs its
provider configuration and credential independently. A missing token price is
unknown, not zero. Capacity-billed local models need a live endpoint probe and
the task's capability floor; Conductor does not yet enforce model-level endpoint
readiness. The OpenRouter free price in `models.yaml` is a
dated catalog value, not a guarantee that a future request stays free.

## Research execution contract

The crypto-autoresearcher harness remains the authority for research task
eligibility and evidence. Its Coordinator freezes the protocol, inputs,
commands, budgets, stop rules, artifact paths, and dependencies. An Executor
running Haiku may carry out a fully specified command and record observations;
implementation, debugging, or interpreting an unexpected result stays on the
implementation tier. A producer cannot validate its own claim.

A claim-relevant run requires a snapshot archive, an independent Validator or
Reviewer task, and a Coordinator ledger archive before state changes. Routine
validation can use Sonnet only if its concrete binding satisfies the policy's
requested effort and capabilities. The nondegradable breakthrough review must
retain its declared `max` requirement. Conductor attempt completion alone is
not an experiment verdict.

## Delivery gates

1. Bind and probe each proposed provider/model before enabling its profile.
   Keep rate limits, price source and check date, credential source, data-use
   class, and tool capabilities with the binding. Never infer a zero price from
   an absent price field.
2. Add question routing to Conductor with a stated budget, a cheap-first
   eligible path, and explicit escalation reasons. Preserve an override for
   a requested model or policy floor.
3. Make `review_required` an executable dependency for Haiku-produced research
   artifacts. Test an eligible frozen run, an ineligible debugging run, provider
   failure, budget exhaustion, and independent review before acceptance.
4. Verify a live Anthropic WIF call and one provider call for each enabled
   external route. Keep credentials out of task metadata, logs, and commits.
5. Compare realized spend, latency, failure rate, and review corrections on a
   bounded set of matched tasks before calling the new routing more efficient.

Anthropic [prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
can reduce repeated-prefix cost in Delphi batches. Cache breakpoints belong at
the end of a stable, sufficiently long prefix. Batch hits are best effort; use
the returned `cache_creation_input_tokens` and `cache_read_input_tokens` to
verify them. A Console chart is an aggregate display, not a request receipt.

Provider references: [Anthropic model pricing](https://platform.claude.com/docs/en/about-claude/pricing),
[OpenRouter free model policy](https://openrouter.ai/pricing),
[Poolside free model terms](https://openrouter.ai/poolside/laguna-xs-2.1%3Afree),
[OpenCode Zen models and data use](https://opencode.ai/docs/zen), and
[Fireworks API](https://fireworks.ai/docs/api-reference/post-responses).
