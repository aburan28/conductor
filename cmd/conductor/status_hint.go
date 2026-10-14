package main

import (
	"fmt"
	"io"

	"github.com/aburan28/conductor/internal/coord"
)

// printStatusNextStep tells a user looking at an idle project what to do next. On a new
// project `conductor status` used to print the project name and a blank line, which reads as
// "something is broken" rather than "nothing has started yet".
func printStatusNextStep(w io.Writer, summary coord.StatusSummary) {
	if len(summary.Active) > 0 || len(summary.Ready) > 0 || len(summary.Conflicts) > 0 || len(summary.Presence) > 0 {
		return
	}
	fmt.Fprint(w, `Nothing in flight. Next:
  conductor task create --title "…" --scope path:FILE   file a piece of work
  conductor wrap claude                                  start a tracked Claude Code session (or codex, opencode)
`)
}
