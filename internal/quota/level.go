package quota

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Level is how close a window is to its limit.
type Level string

const (
	LevelUnknown   Level = "unknown"
	LevelOK        Level = "ok"
	LevelWarning   Level = "warning"
	LevelCritical  Level = "critical"
	LevelExhausted Level = "exhausted"
)

// Rank orders levels so "at least warning" is a comparison.
func (l Level) Rank() int {
	switch l {
	case LevelOK:
		return 1
	case LevelWarning:
		return 2
	case LevelCritical:
		return 3
	case LevelExhausted:
		return 4
	}
	return 0
}

// AlertLevels are the levels that raise a warning, lowest first.
var AlertLevels = []Level{LevelWarning, LevelCritical, LevelExhausted}

// Thresholds are the percentages at which a window is worth a warning.
type Thresholds struct {
	Warn     float64 `json:"warn_percent" yaml:"warnPercent"`
	Critical float64 `json:"critical_percent" yaml:"criticalPercent"`
}

// DefaultThresholds warn at 80% and act at 95%: far enough from 100 that a checkpoint and
// a switch fit in before a busy session burns the rest.
var DefaultThresholds = Thresholds{Warn: 80, Critical: 95}

// Normalize fills gaps with the defaults and keeps the pair ordered and inside (0, 100].
func (t Thresholds) Normalize() Thresholds {
	if t.Warn <= 0 || t.Warn > 100 {
		t.Warn = DefaultThresholds.Warn
	}
	if t.Critical <= 0 || t.Critical > 100 {
		t.Critical = DefaultThresholds.Critical
	}
	if t.Critical < t.Warn {
		t.Critical = t.Warn
	}
	return t
}

// Level classifies a reading at now.
func (t Thresholds) Level(s Snapshot, now time.Time) Level {
	t = t.Normalize()
	if s.LimitReached && !s.Reset(now) {
		return LevelExhausted
	}
	pct, ok := s.Percent(now)
	switch {
	case !ok:
		return LevelUnknown
	case pct >= 100:
		return LevelExhausted
	case pct >= t.Critical:
		return LevelCritical
	case pct >= t.Warn:
		return LevelWarning
	}
	return LevelOK
}

type thresholdsFile struct {
	Quota Thresholds `yaml:"quota"`
}

// LoadThresholds resolves the thresholds in force, highest precedence first:
// CONDUCTOR_QUOTA_WARN / CONDUCTOR_QUOTA_CRITICAL, the user's ~/.conductor/quota.yaml, the
// repository's .conductor/project.yaml, the defaults. A file that does not parse is skipped:
// a typo in a config file must not stop a warning from firing.
func LoadThresholds(repoRoot string, getenv func(string) string) Thresholds {
	if getenv == nil {
		getenv = os.Getenv
	}
	t := Thresholds{}
	merge := func(o Thresholds) {
		if o.Warn > 0 {
			t.Warn = o.Warn
		}
		if o.Critical > 0 {
			t.Critical = o.Critical
		}
	}
	read := func(path string) {
		body, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var f thresholdsFile
		if yaml.Unmarshal(body, &f) == nil {
			merge(f.Quota)
		}
	}
	if repoRoot != "" {
		read(filepath.Join(repoRoot, ".conductor", "project.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		read(filepath.Join(home, ".conductor", "quota.yaml"))
	}
	envPct := func(key string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(getenv(key)), "%"), 64)
		if err != nil {
			return 0
		}
		return v
	}
	merge(Thresholds{Warn: envPct("CONDUCTOR_QUOTA_WARN"), Critical: envPct("CONDUCTOR_QUOTA_CRITICAL")})
	return t.Normalize()
}

// Mark records that a level was raised for a window: when, and which window it was (its
// reset time, when known). It is the unit of "once per window per level".
type Mark struct {
	At       time.Time  `json:"at"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// rearmSlack absorbs the jitter in a reset time computed from a relative "resets in N
// seconds": two readings of one window can disagree by a few seconds, never by minutes.
const rearmSlack = 10 * time.Minute

// NewWindow reports whether a reading belongs to a later window than the one prev was raised
// for, so the level may be raised again. With no previous mark it is always new.
func NewWindow(prev *Mark, cur Snapshot, now time.Time) bool {
	if prev == nil || prev.At.IsZero() {
		return true
	}
	// The window that was alerted has ended.
	if prev.ResetsAt != nil && !prev.ResetsAt.After(now) {
		return true
	}
	if prev.ResetsAt != nil && cur.ResetsAt != nil {
		return cur.ResetsAt.After(prev.ResetsAt.Add(rearmSlack))
	}
	// Without both reset times, a full window length since the alert is the only evidence
	// that this is a different window.
	return now.Sub(prev.At) >= cur.Duration()
}

// Alert is a level newly reached for a window.
type Alert struct {
	Level    Level    `json:"level"`
	Snapshot Snapshot `json:"snapshot"`
}

// Decide returns the level to raise for a reading, if any, given the marks already raised
// for its window (by level). Only the highest level newly reached is returned; Raise lists
// every level that should be marked, so jumping from 50% to 100% raises "exhausted" once and
// never "warning" afterwards in the same window.
func Decide(marks map[Level]*Mark, cur Snapshot, t Thresholds, now time.Time) (raise Level, mark []Level) {
	lvl := t.Level(cur, now)
	if lvl.Rank() < LevelWarning.Rank() {
		return "", nil
	}
	for _, l := range AlertLevels {
		if l.Rank() > lvl.Rank() {
			break
		}
		if NewWindow(marks[l], cur, now) {
			mark = append(mark, l)
		}
	}
	if len(mark) == 0 || mark[len(mark)-1] != lvl {
		// The current level was already raised in this window. A lower level that re-armed
		// beside it is marked quietly: announcing "warning" after "exhausted" would be noise.
		return "", mark
	}
	return lvl, mark
}
