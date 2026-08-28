// Package shutdownhook generates the OS integration that captures agent sessions when a
// machine goes down — a clean shutdown, or the periodic capture that covers a crash or a
// terminated cloud instance.
//
// `conductor wrap` already saves its own session on SIGTERM/SIGHUP, so a wrapped session
// survives a reboot with no setup. This covers the rest: bare sessions (no sidecar to catch
// the signal), and the belt-and-suspenders of a periodic snapshot so a machine that dies
// without a clean shutdown still loses at most one interval of state. Both run
// `conductor sessions save all`, which also pushes to S3 when off-host backup is configured.
//
// The generation is pure and testable; installing (writing the files) and enabling (running
// systemctl/launchctl) are separate steps the CLI does, so nothing here touches a live
// system.
package shutdownhook

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File is one artifact to write, with the mode it should have.
type File struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// Plan is everything needed to install (or remove) the hook on one OS.
type Plan struct {
	OS      string
	Files   []File
	Enable  []string // commands the user runs to activate the hook
	Disable []string // commands to deactivate it
	Notes   []string // caveats worth printing
}

// Options parameterizes generation. Exe is the conductor binary path, Home the user's home,
// GOOS the target platform, Interval the periodic-capture cadence.
type Options struct {
	Exe      string
	Home     string
	GOOS     string
	User     string
	Interval time.Duration
}

const (
	unitName  = "conductor-sessions"
	agentName = "dev.conductor.sessions"
)

// Build produces the install plan for the target OS.
func Build(opts Options) (Plan, error) {
	if opts.Exe == "" {
		opts.Exe = "conductor"
	}
	if opts.Home == "" {
		return Plan{}, fmt.Errorf("shutdownhook: no home directory")
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	switch opts.GOOS {
	case "linux":
		return linuxPlan(opts), nil
	case "darwin":
		return darwinPlan(opts), nil
	default:
		return Plan{}, fmt.Errorf("shutdownhook: no shutdown hook for %s; `conductor sessions save all` still works by hand, and wrapped sessions save themselves on shutdown", opts.GOOS)
	}
}

// linuxPlan uses a systemd *user* service whose ExecStop runs at logout/shutdown, plus a
// timer that captures periodically. The service is oneshot + RemainAfterExit so systemd
// tracks it as active and therefore runs ExecStop when the user session or machine stops.
func linuxPlan(opts Options) Plan {
	base := filepath.Join(opts.Home, ".config", "systemd", "user")
	service := fmt.Sprintf(`[Unit]
Description=Conductor — keep agent sessions resumable across shutdown
Documentation=https://github.com/adamburan/conductor

[Service]
Type=oneshot
RemainAfterExit=yes
# Capture once when the service starts (login / boot) and again as it stops (logout /
# shutdown). The stop capture is the one that matters for a reboot. The binary path is
# double-quoted so an install path with spaces does not break argument tokenization.
ExecStart=%s sessions save all
ExecStop=%s sessions save all

[Install]
WantedBy=default.target
`, systemdArg(opts.Exe), systemdArg(opts.Exe))

	timer := fmt.Sprintf(`[Unit]
Description=Conductor — periodic session capture (covers a crash or a hard shutdown)

[Timer]
OnBootSec=2min
OnUnitActiveSec=%s
AccuracySec=30s

[Install]
WantedBy=timers.target
`, systemdDuration(opts.Interval))

	captureService := fmt.Sprintf(`[Unit]
Description=Conductor — capture agent sessions now

[Service]
Type=oneshot
ExecStart=%s sessions save all
`, systemdArg(opts.Exe))

	notes := []string{
		"The systemd *user* manager runs ExecStop at logout; for a headless host that must capture on full shutdown, also run: loginctl enable-linger " + shellQuote(opts.User),
	}
	return Plan{
		OS: "linux",
		Files: []File{
			{Path: filepath.Join(base, unitName+".service"), Content: service, Mode: 0o644},
			{Path: filepath.Join(base, unitName+"-capture.service"), Content: captureService, Mode: 0o644},
			{Path: filepath.Join(base, unitName+".timer"), Content: timer, Mode: 0o644},
		},
		Enable: []string{
			"systemctl --user daemon-reload",
			"systemctl --user enable --now " + unitName + ".service",
			"systemctl --user enable --now " + unitName + ".timer",
		},
		Disable: []string{
			"systemctl --user disable --now " + unitName + ".timer " + unitName + ".service",
		},
		Notes: notes,
	}
}

// darwinPlan uses a launchd LaunchAgent. launchd has no reliable "run at shutdown" for an
// agent, so capture on macOS is periodic (StartInterval) plus RunAtLoad at login — and
// wrapped sessions still save themselves on the SIGTERM macOS sends at shutdown.
func darwinPlan(opts Options) Plan {
	plistPath := filepath.Join(opts.Home, "Library", "LaunchAgents", agentName+".plist")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>sessions</string>
    <string>save</string>
    <string>all</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>StartInterval</key><integer>%d</integer>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
</dict>
</plist>
`, xmlEscape(agentName), xmlEscape(opts.Exe), int(opts.Interval.Seconds()))

	return Plan{
		OS:    "darwin",
		Files: []File{{Path: plistPath, Content: plist, Mode: 0o644}},
		Enable: []string{
			"launchctl unload " + shellQuote(plistPath) + " 2>/dev/null || true",
			"launchctl load " + shellQuote(plistPath),
		},
		Disable: []string{"launchctl unload " + shellQuote(plistPath)},
		Notes: []string{
			"macOS launchd has no reliable run-at-shutdown for agents, so capture here is periodic (every " + opts.Interval.String() + ") plus at login.",
			"Sessions started with `conductor wrap` also save themselves on the SIGTERM macOS sends at shutdown, so they are captured immediately regardless of the interval.",
		},
	}
}

// systemdDuration renders a duration the way systemd timers spell it (e.g. "5min", "30s").
func systemdDuration(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dmin", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// systemdArg double-quotes a path for a systemd ExecStart/ExecStop line so a path containing
// spaces survives systemd's argument tokenization. systemd unescapes standard C escapes
// inside double quotes, so a backslash and a double-quote are escaped.
func systemdArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// xmlEscape escapes text for a plist <string> body.
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t'\"\\$") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
