package quota

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Login is one account of one tool, with its tightest window.
type Login struct {
	Harness  string   `json:"harness"`
	Account  string   `json:"account"`
	Headroom *float64 `json:"headroom_percent,omitempty"` // 100 minus the highest window; nil when unknown
	Level    Level    `json:"level"`
	Window   string   `json:"window,omitempty"` // the window that sets the headroom
	StateDir string   `json:"-"`
}

// Logins folds a machine's readings into one entry per login. A login's headroom is set by
// its tightest window: a weekly limit at 97% stops the session however empty the five-hour
// window is.
func Logins(snaps []Snapshot, t Thresholds, now time.Time) []Login {
	byKey := map[string]*Login{}
	var order []string
	for _, s := range snaps {
		k := s.Harness + "\x00" + s.Account
		l := byKey[k]
		if l == nil {
			l = &Login{Harness: s.Harness, Account: s.Account, Level: LevelUnknown}
			byKey[k] = l
			order = append(order, k)
		}
		if l.StateDir == "" {
			l.StateDir = s.StateDir
		}
		if lvl := t.Level(s, now); lvl.Rank() > l.Level.Rank() {
			l.Level = lvl
		}
		if pct, ok := s.Percent(now); ok {
			head := 100 - pct
			if head < 0 {
				head = 0
			}
			if l.Headroom == nil || head < *l.Headroom {
				l.Headroom, l.Window = Float(head), s.Window
			}
		}
	}
	out := make([]Login, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Harness != out[j].Harness {
			return out[i].Harness < out[j].Harness
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// Resumable harnesses are the ones a checkpoint can be resumed into.
var Resumable = map[string]bool{"claude": true, "codex": true, "opencode": true}

// Suggestion is where a session should continue, and the command that does it.
type Suggestion struct {
	Harness  string   `json:"harness"`
	Account  string   `json:"account"`
	Headroom *float64 `json:"headroom_percent,omitempty"`
	Native   bool     `json:"native"` // same harness: the conversation itself resumes
	Args     []string `json:"args"`   // after `conductor checkpoint resume <id>`
	Why      string   `json:"why"`
}

// Command renders the full resume command for a checkpoint id.
func (s Suggestion) Command(checkpointID string) string {
	if checkpointID == "" {
		checkpointID = "latest"
	}
	parts := append([]string{"conductor", "checkpoint", "resume", checkpointID}, s.Args...)
	for i, p := range parts {
		if strings.ContainsAny(p, " \t'\"$") {
			parts[i] = "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
		}
	}
	return strings.Join(parts, " ")
}

// Suggest picks the login with the most headroom to continue a session that is running out
// on (harness, account). The same tool on another account comes first while it is below the
// warning threshold, because the conversation itself resumes there; otherwise the most
// headroom wins, wherever it is. Logins that exist on this machine but have never reported
// are a last resort, named as such. home is the user's home directory.
func Suggest(snaps []Snapshot, harness, account string, t Thresholds, now time.Time, home string, getenv func(string) string) (Suggestion, bool) {
	t = t.Normalize()
	var known []Login
	seen := map[string]bool{harness + "\x00" + account: true}
	for _, l := range Logins(snaps, t, now) {
		if !Resumable[l.Harness] || seen[l.Harness+"\x00"+l.Account] {
			continue
		}
		seen[l.Harness+"\x00"+l.Account] = true
		if l.Headroom != nil && *l.Headroom > 0 && l.Level != LevelExhausted {
			known = append(known, l)
		}
	}
	sort.SliceStable(known, func(i, j int) bool { return *known[i].Headroom > *known[j].Headroom })

	pick := func(l Login, why string) (Suggestion, bool) {
		if l.StateDir == "" {
			l.StateDir = defaultStateDir(l.Harness, l.Account, home)
		}
		return Suggestion{
			Harness: l.Harness, Account: l.Account, Headroom: l.Headroom, Native: l.Harness == harness,
			Args: resumeArgs(harness, l, home), Why: why,
		}, true
	}
	below := func(l Login) bool { return 100-*l.Headroom < t.Warn }
	for _, l := range known {
		if l.Harness == harness && below(l) {
			return pick(l, "same tool, another login; the conversation resumes as it was")
		}
	}
	for _, l := range known {
		if below(l) {
			return pick(l, "the most headroom on this machine; resumes from the checkpoint's continuation")
		}
	}
	if len(known) > 0 {
		return pick(known[0], "every login is past the warning threshold; this one has the most left")
	}
	// Nothing has reported; offer a login that exists, same tool first.
	for _, h := range []string{harness, "claude", "codex"} {
		for _, dir := range AccountDirs(h, home, getenv) {
			label := AccountLabel(h, dir, home)
			if seen[h+"\x00"+label] {
				continue
			}
			return pick(Login{Harness: h, Account: label, Level: LevelUnknown, StateDir: dir},
				"no usage reading for this login yet; check it before relying on it")
		}
	}
	return Suggestion{}, false
}

func defaultStateDir(harness, account, home string) string {
	if account == "default" || account == "" {
		return filepath.Join(home, "."+harness)
	}
	if p := filepath.Join(home, "."+harness+"-"+account); dirExists(p) {
		return p
	}
	return filepath.Join(home, ".conductor", "accounts", harness, account)
}

// resumeArgs builds the flags `conductor checkpoint resume` needs to land on a login. The
// --account flag resolves a name the way checkpoint resume does (~/.<harness>-NAME if it
// exists, else ~/.conductor/accounts/<harness>/NAME); when that would name a different
// directory — the default login, or a directory outside both conventions — --state-dir
// names it exactly.
func resumeArgs(from string, l Login, home string) []string {
	var args []string
	if l.Harness != from {
		args = append(args, "--harness", l.Harness)
	}
	switch {
	case l.Account == "default" && l.Harness != from:
		// A different harness starts from its default state; nothing to add.
	case l.Account != "default" && !strings.HasPrefix(l.Account, "dir-") &&
		filepath.Clean(l.StateDir) == filepath.Clean(defaultStateDir(l.Harness, l.Account, home)):
		args = append(args, "--account", l.Account)
	default:
		args = append(args, "--state-dir", l.StateDir)
	}
	return args
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
