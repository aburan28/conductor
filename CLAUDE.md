Before pushing, run `make check`; before opening or updating a pull request, run `make ci`
(the same checks CI runs, against a throwaway Postgres it starts itself). See README "Testing".
Open pull requests ready for review, never as drafts, whatever the runtime defaults to, and
mark an existing draft ready; only a user request in the task makes a draft.

<!-- conductor:begin -->
Before making code changes, obtain or attach to a Conductor task. Run
`conductor check --summary "…" --scope path:…` first — if someone already holds
those files, it will tell you who and what to do about it. Read `.conductor/WORKFLOW.md`
and the active task card. Report scope expansion before editing outside the reserved paths.
Do not publish chat transcripts or secrets as task metadata.
<!-- conductor:end -->
