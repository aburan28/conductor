package quota

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Notifier shows a desktop notification where the platform has a standard way to, and does
// nothing elsewhere. It never blocks for long and never fails loudly: a notification is a
// courtesy, and the session is the thing that matters.
type Notifier struct {
	GOOS     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) error
}

// DefaultNotifier is the notifier for this machine.
func DefaultNotifier(getenv func(string) string) Notifier {
	return Notifier{
		GOOS: runtime.GOOS, Getenv: getenv, LookPath: exec.LookPath,
		Run: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run()
		},
	}
}

// Command returns the program and arguments that would show the notification, or ok=false
// when this machine has no way to (or the user turned it off with CONDUCTOR_QUOTA_NOTIFY=off).
func (n Notifier) Command(title, body string) (name string, args []string, ok bool) {
	switch strings.ToLower(n.Getenv("CONDUCTOR_QUOTA_NOTIFY")) {
	case "off", "0", "false", "no":
		return "", nil, false
	}
	switch n.GOOS {
	case "darwin":
		if _, err := n.LookPath("osascript"); err != nil {
			return "", nil, false
		}
		script := "display notification " + appleString(body) + " with title " + appleString(title)
		return "osascript", []string{"-e", script}, true
	case "linux", "freebsd", "openbsd", "netbsd":
		if n.Getenv("DISPLAY") == "" && n.Getenv("WAYLAND_DISPLAY") == "" {
			return "", nil, false // a headless box or an SSH session: nobody to show it to
		}
		if _, err := n.LookPath("notify-send"); err != nil {
			return "", nil, false
		}
		return "notify-send", []string{"--app-name=Conductor", title, body}, true
	}
	return "", nil, false
}

// Notify shows the notification if it can, giving up after two seconds.
func (n Notifier) Notify(title, body string) {
	name, args, ok := n.Command(title, body)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = n.Run(ctx, name, args...)
}

// appleString quotes s as an AppleScript string literal.
func appleString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Describe is the one-line description of an alert used in notifications and the terminal:
// `claude login "work" is at 96% of its 5h window (resets 15:40)`.
func Describe(a Alert, now time.Time) string {
	s := a.Snapshot
	var what string
	switch a.Level {
	case LevelExhausted:
		what = fmt.Sprintf("has hit its %s limit", s.Window)
	default:
		pct, _ := s.Percent(now)
		what = fmt.Sprintf("is at %s of its %s window", FormatPercent(pct), s.Window)
	}
	line := fmt.Sprintf("%s login %q %s", s.Harness, s.Account, what)
	if s.ResetsAt != nil {
		line += " (resets " + FormatReset(*s.ResetsAt, now) + ")"
	}
	return line
}

// FormatPercent renders a percentage without spurious precision.
func FormatPercent(p float64) string {
	if p >= 10 || p == math.Trunc(p) {
		return strconv.Itoa(int(math.Round(p))) + "%"
	}
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

// FormatReset renders a reset time as a clock time today, or a weekday and time within the
// week, or a date beyond it, in local time.
func FormatReset(t, now time.Time) string {
	lt, ln := t.Local(), now.Local()
	switch {
	case lt.Year() == ln.Year() && lt.YearDay() == ln.YearDay():
		return lt.Format("15:04")
	case lt.Sub(ln) < 6*24*time.Hour:
		return lt.Format("Mon 15:04")
	}
	return lt.Format("Jan 2 15:04")
}

// FormatIn renders how long until t: "3h12m", "45m", "2d4h", or "now".
func FormatIn(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}
