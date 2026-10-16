# Inference endpoints and model discovery

Models ship constantly. A catalog that names `claude-sonnet-5` or `gpt-5.3-codex` in a file is
out of date the week after someone writes it, and nobody notices until a task routes to a model
the provider has retired, or never routes to the better one it released.

Conductor asks the providers instead. You register the **inference endpoints** a project uses
(Anthropic, OpenAI, a local Ollama, a vLLM box, an OpenAI-compatible gateway). Conductor lists
the models each one serves, on a schedule and on demand, and keeps the catalog pointed at what
is actually there.

Discovery reads model **names** from each provider's list endpoint, nothing else. It sends no
prompt and receives no completion.

## Connect an endpoint

```bash
printf '%s' "$ANTHROPIC_API_KEY" | conductor secrets set ANTHROPIC_API_KEY   # the key stays in the vault
conductor models endpoints add anthropic        # preset: address + the usual key secret
conductor models endpoints add openai
conductor models endpoints add ollama           # local, keyless
conductor models endpoints add gateway --kind openai_compatible \
    --url https://llm.corp.example/v1 --key-secret GATEWAY_KEY
conductor models discover --register            # find Ollama / LM Studio / vLLM on this machine
```

The same is **Fleet → Inference endpoints → Add endpoint** in the dashboard.

| Kind | Speaks | Lists with |
|---|---|---|
| `anthropic` | Anthropic API | `GET /v1/models`, `x-api-key` |
| `openai` | OpenAI API | `GET /v1/models`, bearer key |
| `openai_compatible` | vLLM, LM Studio, llama.cpp, LiteLLM, OpenRouter, Fireworks, a gateway | `GET /v1/models`, bearer key if any |
| `ollama` | Ollama's native API | `GET /api/tags` |

Presets exist for `anthropic`, `openai`, `openrouter`, `fireworks`, `ollama`, `lmstudio` and
`vllm`. A preset supplies an address and the name its provider's key usually lives under. It
never supplies a model.

An endpoint holds a **name** for its key (`api_key_secret`), never the key. The key is opened
from the project secret vault (`conductor secrets`; see [OPERATIONS.md](OPERATIONS.md#the-secret-key) for the key that seals it) for one request and is not stored on the
endpoint, shown in a list, or written to an audit record. If the secret does not exist yet, the
endpoint is saved and reports the missing secret by name; set it and refresh.

```bash
conductor models endpoints                       # state, model count, last refresh, last error
conductor models endpoints refresh [NAME]        # ask now
conductor models endpoints set NAME --key-secret OTHER | --url URL | --disable
conductor models endpoints rm NAME
conductor models available [--endpoint NAME] [--new]
```

New endpoints are listed immediately. Afterwards `conductord` refreshes every enabled endpoint
every hour (`--model-discovery-interval`, negative to turn the background refresh off).
Maintainers may refresh on request; requests inside five seconds of the last refresh are
answered from the stored result.

A failed refresh (bad key, endpoint down, a 5xx) is recorded on the endpoint and **drops
nothing**: the models it reported before stay, and the catalog keeps its model. A model an
endpoint stops listing is marked *missing*, not deleted, so when it appeared and when it left
stay on record. Appearances and retirements after an endpoint's first listing are written to
the audit log (`model.discovered`, `model.retired`).

## Let the catalog follow the provider

A profile in `.conductor/models.yaml` can name the models it wants instead of one id:

```yaml
profiles:
  - alias: worker.general
    harness: claude
    provider: anthropic
    model: claude-sonnet-5            # fallback until the first listing, or if nothing matches
    select:
      endpoint: anthropic             # a name from `conductor models endpoints`
      match: ["claude-sonnet-*"]      # globs; * crosses "/" so "vendor/model" ids work
      exclude: ["*-preview"]          # optional
      prefix: ""                      # optional; prepended, e.g. "vllm/" or "ollama/"
    reasoning_effort: medium
    capabilities: [code_edit, tests, multi_file]
```

After each refresh Conductor picks the **newest** model the endpoint serves that matches, and
writes it into the catalog profile. Everything that reads the catalog (the router, session
capability matching, usage and cost) is unchanged; policies still name aliases, never models.
When the provider ships `claude-sonnet-5-6`, `worker.general` on `claude` moves to it within
the hour, and the audit log records `model.resolved` with the old and new id.

"Newest" is the provider's release time when it reports one for every qualifying model
(Anthropic and OpenAI do; Ollama reports when you pulled the model). Otherwise ids are compared
by version, treating digit runs as numbers, so `…-5-10` outranks `…-5-9`. That ordering is only
meaningful inside one model family: a selector spanning several families should be split into a
profile per family, or narrowed with `match` and `exclude`.

Safeguards:

- A profile whose endpoint is down, disabled, removed, or serves nothing that matches **keeps
  its last model**. It never ends up without one.
- A profile that has never resolved and has no fallback `model` is registered but not routable.
- Adopting a new model is a deliberate opt-in: only profiles that carry a `select` follow
  discovery. Profiles that name a `model` are untouched.
- A profile's `select.endpoint` refers to an endpoint of the project that loaded the file
  (`conductord bootstrap`). Endpoint names are per project.

## Choose a model yourself

Fleet → Model catalog shows a dropdown on every profile, filled from what the endpoints serve
(the profile's own endpoint first, models first seen this week marked *new*). Picking one
**pins** the profile; the CLI does the same:

```bash
conductor models pin worker.general claude claude-sonnet-5-5
conductor models unpin worker.general claude     # follow discovery again, immediately
```

A pinned profile is left alone by both discovery and `conductord bootstrap` until it is
unpinned. For a profile that names its model outright, unpinning hands control back to the
models file at the next bootstrap. For harnesses that namespace models (`vllm/…`, `ollama/…`),
give the profile a `select.prefix` so the dropdown pins the namespaced id; otherwise it pins
the bare id the endpoint reported.

## Who may do what

| Action | Role |
|---|---|
| See endpoints, the models they serve, and the catalog | any member |
| Add, change, refresh or remove an endpoint; pin or unpin | maintainer and above |

Endpoint changes and pins are audited (`inference.create|update|delete`, `inference.refresh`,
`model.pinned|unpinned|resolved`) with identifiers only.

## What discovery will and will not connect to

`conductord` makes these requests, so the address is restricted:

- `https` to any host; plain `http` only to `localhost`, loopback, private (RFC 1918 / ULA)
  addresses and single-label or `.local` names, which is how a local model server is reached.
  A key is never sent in the clear across the internet.
- Link-local and unspecified addresses (cloud metadata services live at `169.254.169.254`) are
  refused. The check runs on the address actually dialled, so a hostname that resolves there is
  refused too.
- Redirects are not followed, so a key cannot be carried to another host.
- Each request is bounded: 20 seconds, 8 MiB of response, 20 pages, 5000 models.
- Errors report the HTTP status and a short reason, never the response body.

An endpoint is reached **from the control plane**, not from your laptop. `localhost` in an
address means the machine running `conductord`. If your Ollama runs on a different machine from
the control plane, register it by an address the control plane can reach. `conductor models
discover` probes *this* machine, which is the control plane's too when you ran `conductor up`.

## How it fits together

```
conductord ── every hour / on request ──▶ provider GET /v1/models   (names only, key from the vault)
     │
     ├─ discovered_models   what each endpoint serves; retired models kept, marked missing
     └─ model_profiles      profiles with a `select` get `model` = newest match
                                  │
                      router · selector · usage   (unchanged: they read the catalog)
```
