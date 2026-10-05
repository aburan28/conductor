package quota

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options configure one collection pass. Zero values mean "this machine, now".
type Options struct {
	Getenv  func(string) string
	Now     func() time.Time
	Home    string // defaults to the user's home directory
	Machine string // defaults to the host name

	// Cursor collection (opt-in; see cursor.go).
	HTTPClient *http.Client
	CursorURL  string
	// SkipNetwork leaves out every collector that would leave the machine.
	SkipNetwork bool
}

func (o Options) withDefaults() Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Home == "" {
		o.Home, _ = os.UserHomeDir()
	}
	if o.Machine == "" {
		o.Machine, _ = os.Hostname()
	}
	if o.CursorURL == "" {
		o.CursorURL = CursorSummaryURL
	}
	return o
}

// CollectorStatus is what one collector found, for `conductor doctor`.
type CollectorStatus struct {
	Name     string     `json:"name"`
	Harness  string     `json:"harness"`
	Kind     SourceKind `json:"source_kind"`
	Enabled  bool       `json:"enabled"`
	Readings int        `json:"readings"`
	Newest   *time.Time `json:"newest,omitempty"`
	Error    string     `json:"error,omitempty"`
	Note     string     `json:"note,omitempty"`
}

// Result is a collection pass: the latest reading of every window found, and what each
// collector did.
type Result struct {
	Snapshots  []Snapshot        `json:"snapshots"`
	Collectors []CollectorStatus `json:"collectors"`
}

// Disabled reports whether the operator turned quota tracking off (CONDUCTOR_QUOTA=off).
func Disabled(getenv func(string) string) bool {
	switch strings.ToLower(getenv("CONDUCTOR_QUOTA")) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// cursorRefresh is how often the Cursor endpoint is asked; between asks the stored reading
// stands. Five minutes is finer than a monthly window needs and coarse enough that a
// dozen wrapped sessions do not look like a scraper.
const cursorRefresh = 5 * time.Minute

// Collect reads every local source, stores what it found, and returns the latest reading of
// every window on this machine. It never fails: a collector that cannot read reports why in
// its status and contributes nothing.
func Collect(ctx context.Context, opts Options) Result {
	o := opts.withDefaults()
	now := o.Now().UTC()
	var res Result
	var fresh []Snapshot

	run := func(st CollectorStatus, fn func() ([]Snapshot, error)) {
		snaps, err := guard(fn)
		if err != nil {
			st.Error = err.Error()
		}
		st.Readings = len(snaps)
		for _, s := range snaps {
			if st.Newest == nil || s.ObservedAt.After(*st.Newest) {
				t := s.ObservedAt
				st.Newest = &t
			}
		}
		res.Collectors = append(res.Collectors, st)
		fresh = append(fresh, snaps...)
	}

	for _, dir := range AccountDirs("codex", o.Home, o.Getenv) {
		dir := dir
		run(CollectorStatus{Name: "codex-rollout", Harness: "codex", Kind: KindLocalFile, Enabled: true,
			Note: AccountLabel("codex", dir, o.Home)}, func() ([]Snapshot, error) {
			return ReadCodex(dir, AccountLabel("codex", dir, o.Home), now), nil
		})
	}
	for _, dir := range AccountDirs("claude", o.Home, o.Getenv) {
		dir := dir
		run(CollectorStatus{Name: "claude-transcript", Harness: "claude", Kind: KindLocalFile, Enabled: true,
			Note: AccountLabel("claude", dir, o.Home)}, func() ([]Snapshot, error) {
			return ReadClaudeTranscripts(dir, AccountLabel("claude", dir, o.Home), now), nil
		})
	}

	stored := Load(now)
	cursor := CollectorStatus{Name: "cursor-usage-summary", Harness: "cursor", Kind: KindUndocumented}
	if cookie := CursorCookie(o.Getenv); cookie != "" && !o.SkipNetwork {
		cursor.Enabled = true
		account := strings.TrimSpace(o.Getenv("CONDUCTOR_QUOTA_CURSOR_ACCOUNT"))
		if account == "" {
			account = "default"
		}
		due := true
		for _, s := range stored {
			if s.Harness == "cursor" && s.Account == account && s.Machine == o.Machine && now.Sub(s.ObservedAt) < cursorRefresh {
				due = false
			}
		}
		if due {
			run(cursor, func() ([]Snapshot, error) {
				return FetchCursor(ctx, o.HTTPClient, o.CursorURL, cookie, account, now)
			})
		} else {
			cursor.Note = "cached; asked at most every " + cursorRefresh.String()
			res.Collectors = append(res.Collectors, cursor)
		}
	} else {
		cursor.Note = "opt-in: set CONDUCTOR_QUOTA_CURSOR_COOKIE (undocumented endpoint)"
		res.Collectors = append(res.Collectors, cursor)
	}

	for i := range fresh {
		fresh[i].Machine = o.Machine
	}
	_ = Save(fresh) // a read-only home still gets a reading for this pass

	statusline := CollectorStatus{Name: "claude-statusline", Harness: "claude", Kind: KindDocumented, Enabled: true}
	for _, s := range stored {
		if s.Source == "claude-statusline" {
			statusline.Readings++
			if statusline.Newest == nil || s.ObservedAt.After(*statusline.Newest) {
				t := s.ObservedAt
				statusline.Newest = &t
			}
		}
	}
	if statusline.Readings == 0 {
		statusline.Note = "no readings yet: `conductor quota statusline install` (Pro/Max logins only)"
	}
	res.Collectors = append(res.Collectors, statusline)

	// Stored readings from this machine (status line, manual, an earlier Cursor ask) merge
	// with the fresh ones; the newest per window wins.
	var mine []Snapshot
	for _, s := range stored {
		if s.Machine == "" {
			s.Machine = o.Machine
		}
		if s.Machine == o.Machine {
			mine = append(mine, s)
		}
	}
	res.Snapshots = Latest(append(mine, fresh...))
	return res
}

// guard runs a collector, turning a panic into an error: a format change in some tool's log
// must never take down the session that is reading it.
func guard(fn func() ([]Snapshot, error)) (snaps []Snapshot, err error) {
	defer func() {
		if r := recover(); r != nil {
			snaps, err = nil, fmt.Errorf("collector panicked: %v", r)
		}
	}()
	return fn()
}

// AccountDirs lists the state directories of every login of a harness on this machine:
// the one the environment selects, the default, and the ~/.<harness>-NAME and
// ~/.conductor/accounts/<harness>/NAME conventions. Only directory names are looked at.
func AccountDirs(harness, home string, getenv func(string) string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	switch harness {
	case "claude":
		add(getenv("CLAUDE_CONFIG_DIR"))
	case "codex":
		add(getenv("CODEX_HOME"))
	}
	if home == "" {
		return out
	}
	add(filepath.Join(home, "."+harness))
	if matches, err := filepath.Glob(filepath.Join(home, "."+harness+"-*")); err == nil {
		sort.Strings(matches)
		for _, m := range matches {
			add(m)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(home, ".conductor", "accounts", harness)); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(home, ".conductor", "accounts", harness, e.Name()))
			}
		}
	}
	return out
}
