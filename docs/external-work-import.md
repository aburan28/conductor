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
Codex's local SQLite thread index through the `sqlite3` command and only filenames and file metadata for
Claude Code and Cursor. It does not open their conversation transcripts. The
inventory is created with mode `0600` and refuses to overwrite an existing
file. Treat it as private: it includes local paths and possibly Codex titles.
Use `--since 0` to scan all saved records.

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
modification times. None of those signals alone establishes a live agent or
unfinished work, so the inventory says `running state unknown`. Conductor
cannot retroactively supervise a process that was not launched through its
integration. `conductor sessions save all` remains the separate command for
capturing resumable interactive processes, and `conductor sessions export`
exports Conductor's own registered session history.
