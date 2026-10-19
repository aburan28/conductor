# quality.lint

Advisory structural checks on a **public task envelope** — the coordination text
teammates and agents already share: summary, objective, acceptance criteria,
scopes, and labels.

Findings never block merge. Every response includes `"advisory": true`. Treat
them as authoring hints, not gates.

## Why

Agents and humans file work with wildly uneven cards. Empty acceptance criteria,
over-broad `dir:.` scopes, and chat transcripts pasted into the summary make
routing and duplicate detection worse. `quality.lint` catches the cheap cases
offline and optionally asks a cheap T1 model for a second pass when a local or
OpenAI-compatible endpoint is configured.

## API

```http
POST /v1/projects/{project}/quality/lint
Authorization: Bearer …
Content-Type: application/json
```

### Request body

| field | type | notes |
| --- | --- | --- |
| `summary` | string | one-sentence intent |
| `objective` | string | optional success condition |
| `acceptance_criteria` | `[{text}]` | checkable outcomes |
| `scopes` | `[{resource, mode?}]` | `path:`, `dir:`, `migration:` |
| `labels` | string[] | free-form tags |
| `task` | string | optional task ref; loads missing fields from the ledger |

Do not send chats, transcripts, or secrets. Secret-like substrings are redacted
before heuristic checks and before any LLM enhance call.

### Response

```json
{
  "findings": [
    {"severity": "warning", "code": "missing_scopes", "where": "scopes",
     "message": "no scopes reserved; declare path: or dir: resources before editing"}
  ],
  "advisory": true,
  "engine": "heuristic",
  "redacted": false,
  "enhanced": false
}
```

`engine` is `heuristic` or `heuristic+llm`. Enhancement failures fall back to
heuristics silently so the endpoint stays useful offline.

## MCP

Tool name: `coord_quality_lint`

Same fields as the HTTP body. The gateway posts to the route above; it never
holds the envelope beyond the request.

## CLI

```sh
conductor quality lint \
  --summary "Add retry-aware model routing" \
  --objective "Route retries land on a cheaper eligible profile" \
  --criterion "go test ./internal/router/... passes" \
  --scope dir:internal/router \
  --label backend \
  --json
```

Or lint a stored task:

```sh
conductor quality lint --task T-42 --json
```

## LLM enhance (optional)

Heuristics always run. When configured, Conductor may call a cheap model at the
`verifier.cheap` / T1 class:

| variable | purpose |
| --- | --- |
| `CONDUCTOR_QUALITY_LINT_URL` | OpenAI-compatible base URL (e.g. `http://127.0.0.1:11434/v1`) |
| `CONDUCTOR_QUALITY_LINT_KEY` | optional bearer token |
| `CONDUCTOR_QUALITY_LINT_MODEL` | model id (default `verifier.cheap`) |

Servers can also inject an enhancer via `api.Options.QualityEnhance`. Nothing from
the enhance prompt or completion is stored in Conductor.

## Privacy

- No chats or transcripts are accepted or stored by this surface.
- Secret-like patterns are stripped from inputs before lint/enhance.
- Private tasks loaded by `task` are **redacted** for non-owners: only reserved
  scopes remain; title/objective/criteria are not sent to heuristics or the LLM.
- Progress and audit logs must not include the envelope body.

## What it is not

- Not a CI required check and not a merge policy input.
- Not a substitute for `conductor policy lint` (repo policy files) or for
  harness-level code review.
- Not Delphi (frontier batch delegation); enhance is interactive and optional.
