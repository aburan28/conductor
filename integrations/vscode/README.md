# Conductor Agent Sessions — VS Code extension

Companion to the `conductor` CLI's `pause` and `resume` commands, for people whose agent
terminals live inside VS Code.

Pausing already works without this extension: a VS Code integrated terminal is an ordinary
pty, so `conductor pause` freezes sessions running there like any others, and
`conductor resume` wakes them in place. What VS Code cannot do without help is **reopen** a
session whose terminal tab (or window) was closed — there is no command-line way to open an
integrated terminal running a command. This extension supplies that missing piece.

## What it does

- **Handles resume handoffs.** When `conductor resume` finds a paused session that was
  running in a VS Code integrated terminal (recorded from `TERM_PROGRAM` at pause time) and
  this extension is installed, it fires
  `vscode://conductor.conductor-sessions/resume?id=<record-id>` instead of opening tmux or a
  GUI emulator. The extension reads the session record from `~/.conductor/sessions/`, opens
  an integrated terminal in the session's working directory, and runs the harness's own
  conversation-resume invocation (`claude --continue`, `codex resume --last`,
  `opencode --continue`) — under `conductor wrap` with the original capability flags when the
  session was wrapped.
- **Command palette:** `Conductor: Pause All Agent Sessions` and
  `Conductor: Resume All Agent Sessions` run the CLI from inside VS Code.

The URI deliberately carries only a record id, never a command line. The command is
reconstructed from the record file on disk, which is owner-only (0600) — a hostile
`vscode://` link cannot inject anything a local process could not already do.

The record is deleted only after the terminal is actually open (ownership passes from the
CLI to the extension with the handoff), so a handoff that goes nowhere — extension
uninstalled mid-flight, window closed — leaves the session paused and retryable.

## Install

The extension is plain JavaScript with no build step. Package and install it locally:

```bash
cd integrations/vscode
npx @vscode/vsce package        # produces conductor-sessions-0.1.0.vsix
code --install-extension conductor-sessions-0.1.0.vsix
```

`conductor resume` detects the extension via `code --list-extensions` and falls back to its
normal terminal chain when it is absent, so installing (or removing) it never breaks resume.

## Settings

| Setting | Default | Purpose |
|---|---|---|
| `conductor.cliPath` | `conductor` | Path to the CLI binary, if it is not on VS Code's PATH. |
| `conductor.stateDir` | *(empty)* | Conductor state directory, mirroring the CLI's `CONDUCTOR_STATE_DIR`. Empty means `~/.conductor`. |

## Notes

- With several VS Code windows open, the URI lands in the last-focused window; the terminal
  still opens in the session's recorded working directory, so the session is correct even if
  the window is not the one it originally lived in.
- `TERM_PROGRAM` detection for *bare* (unwrapped) sessions reads `/proc/<pid>/environ` and is
  therefore Linux-only; sessions launched through `conductor wrap` record it on every
  platform. A session without a recorded terminal program simply resumes through the CLI's
  normal terminal chain.
