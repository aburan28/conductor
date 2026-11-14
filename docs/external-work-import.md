# Discover and import external coding work

Conductor's existing `integrate` command shows where it writes MCP and hook
configuration. `conductor integrate all --print --json` previews those changes,
and `conductor doctor --json` reports detected tools and configuration paths.

To review saved local conversations from Codex, Claude Code, and Cursor:

```sh
conductor sessions discover --since 168h --output external-work.json
conductor sessions import --input external-work.json --project my-project codex:<uuid>
conductor sessions import --input external-work.json --project my-project codex:<uuid> --apply
```

`discover` lists existing configuration files and saved conversation metadata.
It checks the user's config directories, `CODEX_HOME` when set, and the
current Git worktree for project configuration. Run it in each repository
whose project configuration you want to inspect.
Its JSON inventory contains the source, identifier, local record path, last
modification time, and (for Codex) local title and working directory. It reads
Codex's local SQLite thread index through the `sqlite3` command, using the
indexed `updated_at` field to fetch only records in the requested lookback.
For Claude Code and Cursor it reads only filenames and file metadata. It also
checks the current user's process table for a
Codex `resume <uuid>` or Claude `--resume <uuid>` invocation matching a saved
record. It stores only the matching PID and identifier, never process arguments
or conversation text. It does not open conversation transcripts. The
inventory is created with mode `0600` and refuses to overwrite an existing
file. Treat it as private: it includes local paths and possibly Codex titles.
Use `--since 0` to scan all saved records.

## Periodic local scan

The separate background scanner refreshes an owner-only inventory every 15
minutes by default. Its scan reads the same metadata as `discover`; it does
not import tasks or register live Conductor sessions. The latest inventory
lives at `$CONDUCTOR_STATE_DIR/external-work/latest.json`, or
`~/.conductor/external-work/latest.json` when that variable is unset. The
directory is mode `0700`, the file is mode `0600`, and each refresh replaces
the derived inventory atomically. It does not alter source transcripts or
checkpoint copies. `started_at` and `scanned_at` record the start and completion
of the scan, so `scan_duration_ms` in `--status --json` shows how long a refresh
actually took. The completion timestamp can lag the start under disk pressure.

```sh
conductor sessions scan --workspace /path/to/repository
conductor sessions scan --status --json
conductor sessions install-scan --workspace /path/to/repository --interval 15m
conductor sessions install-scan --workspace /path/to/repository --interval 15m --install --activate
conductor sessions install-scan --uninstall
```

`install-scan` previews a user-level macOS launchd agent or Linux systemd
timer by default. `--install --activate` writes and enables that schedule,
including one scan at login or activation. The schedule records an absolute
path to the Conductor binary and the chosen workspace; keep that binary in a
stable location. Its `--since` flag defaults to seven days and can be changed
without changing the scan interval. Reinstall with new flags to change the
cadence or workspace.

The import command first previews the exact selected identifiers. `--apply`
creates **private proposed review tasks**, never live Conductor sessions or
ready work. Their titles are generic and their external references are stable
`codex:<uuid>`, `claude:<uuid>`, or `cursor:<uuid>` values. Local paths,
conversation text, and Codex titles stay out of the Conductor task. Repeating
an import skips a task with the same external reference. The reviewer should
inspect the source conversation locally, write the actual objective and
scope, and only then make a task ready for dispatch.

Saved conversations are not necessarily running or unfinished. Codex's local
index can mark a thread unarchived; Claude and Cursor transcripts have file
modification times. Those signals alone establish neither a live agent nor
unfinished work. A matching explicit resume process is labeled
`process_alive_at_scan` with its PID and scan time; it can be idle or have exited
since the scan. Other records say `saved; running state unknown`. Cursor and
Codex Desktop windows without an explicit resume UUID remain unknown. Conductor
cannot retroactively supervise a process that was not launched through its
integration. `conductor sessions save all` remains the separate command for
capturing resumable interactive processes, and `conductor sessions export`
exports Conductor's own registered session history.
