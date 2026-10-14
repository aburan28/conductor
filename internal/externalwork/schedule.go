package externalwork

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ScheduleFile struct {
	Path    string
	Content string
	Mode    os.FileMode
}
type SchedulePlan struct {
	Files   []ScheduleFile
	Label   string
	Service string
	Timer   string
}
type ScheduleOptions struct {
	Exe, Home, Workspace, StateDir, GOOS string
	Interval, Since                      time.Duration
}

// BuildSchedule produces a per-user launchd or systemd schedule; it does not activate it.
func BuildSchedule(o ScheduleOptions) (SchedulePlan, error) {
	if o.Exe == "" || !filepath.IsAbs(o.Exe) {
		return SchedulePlan{}, fmt.Errorf("scanner binary must be an absolute path")
	}
	if o.Home == "" || o.Workspace == "" || !filepath.IsAbs(o.Workspace) {
		return SchedulePlan{}, fmt.Errorf("home and workspace are required")
	}
	if o.Interval < time.Minute {
		return SchedulePlan{}, fmt.Errorf("scan interval must be at least one minute")
	}
	if o.Since < 0 {
		return SchedulePlan{}, fmt.Errorf("lookback must be nonnegative")
	}
	args := []string{o.Exe, "sessions", "scan", "--since", o.Since.String(), "--workspace", o.Workspace}
	if o.StateDir != "" {
		args = append(args, "--state-dir", o.StateDir)
	}
	switch o.GOOS {
	case "darwin":
		const label = "dev.conductor.external-work"
		path := filepath.Join(o.Home, "Library", "LaunchAgents", label+".plist")
		var lines strings.Builder
		for _, a := range args {
			fmt.Fprintf(&lines, "    <string>%s</string>\n", xmlEscape(a))
		}
		content := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array>
%s  </array>
  <key>RunAtLoad</key><true/>
  <key>StartInterval</key><integer>%d</integer>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
</dict></plist>
`, label, lines.String(), int(o.Interval.Seconds()))
		return SchedulePlan{Files: []ScheduleFile{{Path: path, Content: content, Mode: 0o644}}, Label: label}, nil
	case "linux":
		base := filepath.Join(o.Home, ".config", "systemd", "user")
		service := "conductor-external-work.service"
		timer := "conductor-external-work.timer"
		quoted := make([]string, len(args))
		for i, a := range args {
			quoted[i] = systemdArg(a)
		}
		serviceBody := "[Unit]\nDescription=Conductor external work metadata scan\n\n[Service]\nType=oneshot\nExecStart=" + strings.Join(quoted, " ") + "\n"
		timerBody := "[Unit]\nDescription=Periodically scan local coding work\n\n[Timer]\nOnBootSec=2min\nOnUnitActiveSec=" + strconv.Itoa(int(o.Interval.Seconds())) + "s\nAccuracySec=30s\n\n[Install]\nWantedBy=timers.target\n"
		return SchedulePlan{Files: []ScheduleFile{{Path: filepath.Join(base, service), Content: serviceBody, Mode: 0o644}, {Path: filepath.Join(base, timer), Content: timerBody, Mode: 0o644}}, Service: service, Timer: timer}, nil
	default:
		return SchedulePlan{}, fmt.Errorf("no background scan schedule for %s", o.GOOS)
	}
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}
func systemdArg(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
