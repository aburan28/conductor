# delphi operator tools

Scripts that drive the `delphi` CLI (the batch-delegation binary) from the
outside: a staged goal pipeline, a live view, and a shim that lets the
`openai` provider use OpenRouter's free models.

They are operator tooling, not part of the `conductor` build and not imported
by `internal/delphi`. Everything here talks to the binary and to the state
directory (`$DELPHI_HOME`, default `~/.delphi`).

| file | what it does |
|---|---|
| `delphi-goal` | staged pipeline: `frame` (frontier) → `expand` (free, one per direction) → `critique` → `synthesize` → `GOAL.md` |
| `delphi-pipeline` | the older single-stage version: ideas → one free expansion each |
| `on_result.py` | the hook `serve -on-result` runs; advances the stage machine, writes `GOAL.md`, emits `problem.md` |
| `delphi-goal-emit` | turns a finished run into a repo-shaped problem statement with a ledger skeleton and an evidence gate |
| `delphi-watch` | live dashboard: queue/flights, batch events, pipeline progress, `serve.log` tail |
| `delphi-follow` | prints each answer as it lands (`--all`, `--root`, `--stage`, `--raw`) |
| `shim.py` | OpenAI Files+Batches API in front of OpenRouter chat completions, so `--provider openai` can reach `:free` models |

## Install

```sh
install -m 700 delphi-* ~/.local/bin/
install -m 700 on_result.py ~/.delphi/hooks/on_result.py
python3 shim.py &          # or a launchd/systemd unit
```

`serve` must run with the hook:

```sh
delphi serve -on-result "$HOME/.delphi/hooks/on_result.py" -flush-every 2m
```

## Configuration

| variable | default | meaning |
|---|---|---|
| `DELPHI_BIN` | `delphi` on `PATH` | the binary the scripts call |
| `DELPHI_HOME` | `~/.delphi` | state directory |
| `PIPELINE_MAX_IDEAS` | 6 | breadth of the fan-out |
| `PIPELINE_FREE_PROFILE` | `free` | profile used for expansion |
| `PIPELINE_DEEP_PROFILE` | `frontier` | profile used for framing/critique |
| `DELPHI_GOAL_DEEP` | 1 | `0` sends every stage to the free tier |
| `GOAL_MAX_TOKENS` / `GOAL_EFFORT` | 64000 / medium | framing budget; reasoning models will otherwise spend the whole budget thinking and return empty text |
| `OPENROUTER_API_KEY` | — | required by `shim.py`; `~/.delphi/openrouter.env` is read if unset |

## Notes

`shim.py` exists because the batch API has no free tier: delphi speaks
Files+Batches, OpenRouter speaks chat completions. If a provider-native free
mode lands (see the `claude/delphi-free-mode` work on `internal/delphi`), the
shim becomes redundant and the rest of these scripts keep working unchanged.

`on_result.py` refuses to advance a stage on empty text. A reasoning model that
hits `max_tokens` with zero output is reported as `STALLED.txt` rather than
silently producing an empty brief — that failure cost a full pipeline run once.
