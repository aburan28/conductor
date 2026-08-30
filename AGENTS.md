# Conductor

Go service. Coordination control plane for teams where humans and coding agents
work the same repository at the same time. Entry points live in `cmd/`
(`conductor`, `conductord`, `conductor-mcp`); everything else is under `internal/`
(`api`, `client`, `config`, `coord`, `db`, `domain`, `harness`, `mcp`, `privacy`,
`resource`, `router`, `runner`, `scheduler`, `selector`, `taskcard`, `web`,
`worktree`). Build/test with `make`. Design doc: `docs/DESIGN.md`.

## Token discipline

Context budget is a shared resource. Treat it that way.

1. **Delegate before you read in bulk.** Non-trivial multi-step research or
   implementation goes through the `task` tool (subagent). Subagents run in a
   fresh context and return a bounded result, so the bulk never enters this
   conversation. Reach for this whenever a step would pull in more than a
   couple of files or more than a few hundred lines of tool output.

2. **Offload low-judgment bulk to Ollama.** Before reading a large, noisy
   artifact verbatim into context — multi-thousand-line logs, CI dumps, long
   transcripts, big diffs — pass it through the `ollama-offload` skill
   (`/home/coder/.claude/skills/ollama-offload/scripts/ollama_offload.py`).
   Only the bounded summary comes back. CPU-only, ~1 min per 40 KB, so use it
   for size, not for small inputs or correctness-critical judgments.

3. **Read surgically.** Prefer `Glob` and `Grep` over listing and scanning.
   Use `Read` with `offset` + `limit` for known regions of large files instead
   of pulling the whole file. Do not use `cat` / `head` / `tail` / `sed` /
   `awk` from Bash for file inspection — the dedicated tools are cheaper
   because they stream less into context.

These are defaults, not dogma. Small inputs and correctness-critical reads
still belong in context directly.

<!-- conductor:begin -->
Before making code changes, obtain or attach to a Conductor task. Run
`conductor check --summary "…" --scope path:…` first — if someone already holds
those files, it will tell you who and what to do about it. Read `.conductor/WORKFLOW.md`
and the active task card. Report scope expansion before editing outside the reserved paths.
Do not publish chat transcripts or secrets as task metadata.
<!-- conductor:end -->
