// Package quota tracks how close each subscription login is to its usage limit.
//
// Every coding tool sold on a subscription meters it in rolling windows — Claude Code's
// five-hour session and weekly limits, Codex's primary and secondary windows, Cursor's
// monthly included usage — and every one of them stops the session when a window runs out.
// The tools each show the number somewhere, but only to the person looking at that tool.
// This package reads what each tool exposes locally (docs/USAGE_LIMITS.md lists every
// source and how far each can be trusted), normalises it into one Snapshot per window per
// login, and decides when a reading is worth a warning.
//
// A snapshot carries numbers and labels only. The login is named by its state directory
// ("default", "work"), never by an email or a token, and nothing a collector reads beyond
// the rate-limit fields themselves is kept.
package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SourceKind says how far a reading can be trusted, in the classification of
// docs/USAGE_LIMITS.md.
type SourceKind string

const (
	// KindDocumented is a field the vendor documents (Claude Code's status line payload).
	KindDocumented SourceKind = "documented"
	// KindLocalFile is a file the tool writes whose shape is observable but not a documented
	// interface (Codex rollouts, Claude Code's limit-reached transcript records).
	KindLocalFile SourceKind = "local_file"
	// KindUndocumented is a vendor web endpoint the vendor does not document (Cursor).
	KindUndocumented SourceKind = "undocumented"
	// KindManual is a number the user fed in with `conductor quota report`.
	KindManual SourceKind = "manual"
)

// ValidKind reports whether k is one of the kinds above.
func ValidKind(k SourceKind) bool {
	switch k {
	case KindDocumented, KindLocalFile, KindUndocumented, KindManual:
		return true
	}
	return false
}

// Canonical window names. A model-specific bucket is "<limit id>:<window>".
const (
	Window5h      = "5h"
	WindowWeekly  = "weekly"
	WindowDaily   = "daily"
	WindowMonthly = "monthly"
	WindowSpend   = "spend"
)

// Snapshot is one reading of one window of one login of one tool.
type Snapshot struct {
	Harness       string     `json:"harness"`
	Account       string     `json:"account"`
	Machine       string     `json:"machine,omitempty"`
	Window        string     `json:"window"`
	WindowMinutes int        `json:"window_minutes,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	Used          *float64   `json:"used,omitempty"`
	Limit         *float64   `json:"limit,omitempty"`
	Unit          string     `json:"unit,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	LimitReached  bool       `json:"limit_reached,omitempty"`
	Plan          string     `json:"plan,omitempty"`
	Source        string     `json:"source"`
	SourceKind    SourceKind `json:"source_kind"`
	ObservedAt    time.Time  `json:"observed_at"`

	// StateDir is the harness state directory the reading came from. It is machine-local
	// (a path can carry a user name) and never serialized: it exists so a resume command
	// can be built on this machine.
	StateDir string `json:"-"`
}

// Key identifies the window a snapshot describes, for "latest reading wins" merges.
func (s Snapshot) Key() string {
	return s.Machine + "\x00" + s.Harness + "\x00" + s.Account + "\x00" + s.Window
}

// Percent is the share of the window used, as best it can be told at now: the reported
// percentage, else used/limit, else 100 when the limit is reported hit. A reading whose
// window has already reset counts as 0. ok is false when nothing says how much is used.
func (s Snapshot) Percent(now time.Time) (pct float64, ok bool) {
	if s.Reset(now) {
		return 0, true
	}
	switch {
	case s.UsedPercent != nil:
		pct, ok = *s.UsedPercent, true
	case s.Used != nil && s.Limit != nil && *s.Limit > 0:
		pct, ok = *s.Used / *s.Limit * 100, true
	}
	if s.LimitReached && pct < 100 {
		pct, ok = 100, true
	}
	if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 {
		return 0, false
	}
	return pct, ok
}

// Reset reports whether the window this reading describes has ended since it was taken.
func (s Snapshot) Reset(now time.Time) bool {
	return s.ResetsAt != nil && !s.ResetsAt.After(now)
}

// Duration is the window's length: what the source said, else what the window name implies,
// else five hours (the shortest window any subscription uses, so the safest guess for
// re-arming a warning).
func (s Snapshot) Duration() time.Duration {
	if s.WindowMinutes > 0 {
		return time.Duration(s.WindowMinutes) * time.Minute
	}
	name := s.Window
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	switch name {
	case WindowWeekly:
		return 7 * 24 * time.Hour
	case WindowDaily:
		return 24 * time.Hour
	case WindowMonthly, WindowSpend:
		return 30 * 24 * time.Hour
	}
	return 5 * time.Hour
}

// WindowName maps a window length to its canonical name.
func WindowName(minutes int) string {
	switch {
	case minutes <= 0:
		return "window"
	case minutes == 300:
		return Window5h
	case minutes == 1440:
		return WindowDaily
	case minutes == 10080:
		return WindowWeekly
	case minutes >= 40000 && minutes <= 46000:
		return WindowMonthly
	case minutes%1440 == 0:
		return strconv.Itoa(minutes/1440) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	}
	return strconv.Itoa(minutes) + "m"
}

// Latest keeps the newest reading per window, ordered by harness, account, machine, window.
func Latest(snaps []Snapshot) []Snapshot {
	best := map[string]Snapshot{}
	for _, s := range snaps {
		if cur, ok := best[s.Key()]; !ok || s.ObservedAt.After(cur.ObservedAt) {
			best[s.Key()] = s
		}
	}
	out := make([]Snapshot, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	Sort(out)
	return out
}

// Sort orders snapshots for display.
func Sort(snaps []Snapshot) {
	sort.Slice(snaps, func(i, j int) bool {
		a, b := snaps[i], snaps[j]
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		return a.Window < b.Window
	})
}

// AccountLabel names a login by its state directory, the convention `conductor checkpoint
// resume --account` already uses: the harness's default directory is "default",
// ~/.<harness>-NAME and ~/.conductor/accounts/<harness>/NAME are NAME. Anything else is
// "dir-" plus a short hash of the path, so a path that carries a user name never leaves the
// machine. home is the user's home directory.
func AccountLabel(harness, stateDir, home string) string {
	if stateDir == "" {
		return "default"
	}
	dir := filepath.Clean(stateDir)
	if home != "" {
		if dir == filepath.Join(home, "."+harness) {
			return "default"
		}
		if filepath.Dir(dir) == home {
			if name, ok := strings.CutPrefix(filepath.Base(dir), "."+harness+"-"); ok && name != "" {
				return name
			}
		}
		if filepath.Dir(dir) == filepath.Join(home, ".conductor", "accounts", harness) {
			return filepath.Base(dir)
		}
	}
	sum := sha256.Sum256([]byte(dir))
	return "dir-" + hex.EncodeToString(sum[:])[:10]
}

// Float is a convenience for building snapshots.
func Float(v float64) *float64 { return &v }

// Time is a convenience for building snapshots.
func Time(t time.Time) *time.Time { t = t.UTC(); return &t }
