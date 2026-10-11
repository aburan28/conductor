# Session portability: checkpoints

A coding-agent session is pinned to one machine, one account, and one working directory,
because that is where the harness keeps the conversation: Claude Code under
`~/.claude/projects/<cwd slug>/<session>.jsonl`, Codex under
`~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl`, OpenCode in its own database. When the
account hits its usage limit, the machine is terminated, or the work should continue in a
different tool, the conversation is stranded where it is.

A **checkpoint** is one file that makes the session portable:

| Layer | Contents | What it enables |
|---|---|---|
| `native/<harness>/…` | the harness's own transcript, verbatim (Claude Code: plus its per-session directory of subagent transcripts and spilled tool results) | the **same harness** reopens the **same conversation** under another login or on another machine |
| `workspace/` | `commits.bundle` (commits not on any remote), `tracked.patch` (`git diff --binary HEAD`), `untracked/…` (non-ignored untracked files) | the code the conversation refers to travels with it |
| `CONTINUATION.md` | a harness-neutral distillation: the original request, the conversation log (user text, assistant text, one line per tool call), files touched, the last exchange, and any note left at checkpoint time | a **different harness** takes the work over from a prompt |
| `manifest.json` | ids, timestamps, repository state, counts, a sha256 for every member | listing without opening; verification before trusting |

Bundles are gzip tar archives (`.ckpt`) with the manifest as the first member.

## Where it sits in the privacy model

A checkpoint contains the conversation, which the control plane must never hold
(DESIGN.md §12.4; `project.yaml` says `privacy.transcriptStorage: local_only`). So:

- Checkpoints are written only by the CLI, on the user's own machine, to
  `~/.conductor/checkpoints/` (0600 files in a 0700 directory, next to the credentials).
- No type in `internal/checkpoint` is sent to the control plane. `internal/domain`,
  `internal/api`, and `internal/db` do not import it, and the existing privacy tests
  (`TestNoTranscriptFieldsInSharedTypes`, `TestNoTranscriptColumnsInSchema`) are unchanged.
- The `coord_checkpoint` MCP tool runs only in the stdio gateway, on the harness's machine.
  The HTTP gateway, which lives in the control plane, refuses it and points at the CLI.
- A checkpoint leaves the machine only as a file the user moves themselves, or **sealed**
  (AES-256-GCM, key from a passphrase via PBKDF2-SHA256) in the user's own S3 bucket.
  `conductor checkpoint push` refuses to upload plaintext. Beside the ciphertext the bucket
  holds only an index entry — id, creation time, harness, sealed size — for listing. The
  manifest itself names the conversation (the harness's title), the note, the working
  directory, and the machine, so it travels only inside the sealed bundle.
- A restore treats a bundle as untrusted: manifest identifiers must be plain identifiers,
  members are size-capped, and the working tree is written through `os.Root` without following
  any symbolic link and never under `.git`.
- The hook that captures after each turn reads only `session_id`, `cwd`, and
  `hook_event_name` from the hook payload and locates the transcript itself.

## When checkpoints are taken

| Trigger | Reason recorded | Notes |
|---|---|---|
| `conductor wrap` sidecar, every `CONDUCTOR_CHECKPOINT_INTERVAL` (default 2m) | `periodic` | skipped when nothing changed |
| `conductor wrap` on SIGTERM/SIGHUP and at harness exit | `signal`, `exit` | bounded to 20s so shutdown is never held up |
| Claude Code `Stop` and `PreCompact` hooks (`conductor integrate claude`) | `hook:Stop`, `hook:PreCompact` | `Stop` is rate-limited to one capture per 30s; `PreCompact` always captures a changed transcript |
| Claude Code `SessionEnd` hook | `hook:SessionEnd` | the final capture for bare sessions |
| OpenCode plugin on `session.idle` (`conductor integrate opencode`) | `hook` | |
| `conductor sessions save all` (and so the shutdown hook from `sessions install-hook`) | `exit` | every live session on the machine |
| `coord_checkpoint` from inside a session | `agent` | the agent's own note on where the work stands |
| `conductor checkpoint capture [--note …]` | `manual` | |

Every path dedupes on a content fingerprint (transcript + working tree + HEAD) and keeps
the newest `CONDUCTOR_CHECKPOINT_KEEP` (default 5) per session, never thinning one younger
than ten minutes. `CONDUCTOR_CHECKPOINT=off` disables all of it.

## Resuming

```
conductor checkpoint resume <id|file> [--dir DIR] [--clone] [--harness H] [--account NAME | --state-dir DIR] [--force] [--print]
```

1. **Working tree.** Into `--dir` (default: the current directory; `--clone` creates it from
   the recorded remote). Fetch the commit bundle, put the branch at the recorded commit (a
   diverged branch is left alone and the commit checked out detached), apply the patch
   with `git apply --3way`, write the untracked files. A dirty target is refused without
   `--force`.
2. **Conversation.** Same harness: the native transcript is installed where that harness
   looks, with the session's `cwd` rewritten to the new checkout —
   Claude Code: `$CLAUDE_CONFIG_DIR/projects/<new slug>/<id>.jsonl`, resumed by path
   (`claude --resume /abs/path.jsonl`), so a copy left behind on the original machine
   cannot make resume-by-id ambiguous;
   Codex: `$CODEX_HOME/sessions/…/rollout-…-<id>.jsonl` at its original relative path,
   resumed with `codex resume <id>`;
   OpenCode: `opencode import`, resumed with `opencode --session <id>`.
   Different harness: `CONTINUATION.md` is written to `.conductor/generated/continuations/`
   and the harness is started with a prompt that tells it to read that file first.
3. **Launch.** In the foreground, in the restored checkout, through `conductor wrap` when
   the checkpoint came from a wrapped session (so it is registered and keeps
   checkpointing), with `CONDUCTOR_RESUMED_FROM=<id>` so the lineage is recorded.

### Another login

`--account NAME` resolves to `~/.<harness>-NAME` when that directory exists (the common
convention for a second Claude Code login), otherwise `~/.conductor/accounts/<harness>/NAME`;
`--state-dir` names any directory. The directory is passed as `CLAUDE_CONFIG_DIR`,
`CODEX_HOME`, or `XDG_DATA_HOME`, which is where each harness keeps both its credentials and
its sessions, so the conversation is reopened by that directory's login. Log the second
directory in once (`CLAUDE_CONFIG_DIR=~/.claude-work claude /login`).

Whether a provider's terms permit continuing work under a second account after a usage
limit is between you and the provider; Conductor only moves the file.

### Another machine

```
# here
conductor checkpoint export latest --seal --out session.ckpt      # or: checkpoint push
# there
conductor checkpoint resume session.ckpt --dir ~/src/repo --clone  # or: checkpoint pull <id>
```

### Another harness

```
conductor checkpoint resume latest --harness codex
```

Codex cannot read a Claude Code transcript (and the reverse), so it starts from the
continuation. The native transcript is still in the bundle for a same-harness resume later.

## Formats this build understands

| Harness | Located by | Parsed records |
|---|---|---|
| Claude Code | `projects/<slug>/<id>.jsonl`; `sessions/<pid>.json` maps a live process to its session id | `user` / `assistant` lines (string or block content: `text`, `tool_use`, `tool_result`), `summary`, `ai-title`; sidechain and injected-context lines are skipped |
| Codex | `sessions/**/rollout-*-<id>.jsonl`; `session_meta.cwd` matches the directory | `event_msg` `user_message` / `agent_message`; `response_item` `function_call`, `local_shell_call`, outputs; `message` items as a fallback for rollouts without event messages |
| OpenCode | `opencode session list --format json`, `opencode export <id>` | `messages[].parts[]` of type `text` and `tool` |

The native formats are each harness's own and change between versions. Parsing is
tolerant (an unknown line is counted and skipped), and the continuation degrades to
whatever could be read; the native transcript is carried verbatim regardless.

## Commands

```
conductor checkpoint capture [--harness H] [--session ID] [--note …] [--all] [--no-workspace] [--force]
conductor checkpoint list [--all] [--session S]
conductor checkpoint show <id|file> [--continuation]
conductor checkpoint resume <id|file> …
conductor checkpoint export <id> [--out FILE] [--seal]
conductor checkpoint import <file>
conductor checkpoint push [id…|--all]          # requires CONDUCTOR_CHECKPOINT_KEY
conductor checkpoint pull <id> | --list
conductor checkpoint prune [--keep N]
conductor hook checkpoint                       # what the Claude Code hooks run
```

Every command takes `--json`. An id can be given in full, by its random tail, by any
unambiguous prefix, by session id, or as `latest`.
