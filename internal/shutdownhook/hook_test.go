package shutdownhook

import (
	"strings"
	"testing"
	"time"
)

func TestLinuxPlan(t *testing.T) {
	p, err := Build(Options{Exe: "/usr/local/bin/conductor", Home: "/home/dev", GOOS: "linux", User: "dev", Interval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if p.OS != "linux" || len(p.Files) != 3 {
		t.Fatalf("plan = %+v", p)
	}
	var service string
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, "conductor-sessions.service") {
			service = f.Content
			if !strings.HasPrefix(f.Path, "/home/dev/.config/systemd/user/") {
				t.Errorf("service path = %s", f.Path)
			}
		}
	}
	if !strings.Contains(service, `ExecStop="/usr/local/bin/conductor" sessions save all`) {
		t.Errorf("service must capture on stop:\n%s", service)
	}
	if !strings.Contains(service, `ExecStart="/usr/local/bin/conductor" sessions save all`) {
		t.Errorf("service must capture on start:\n%s", service)
	}
	var timer string
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, ".timer") {
			timer = f.Content
		}
	}
	if !strings.Contains(timer, "OnUnitActiveSec=5min") {
		t.Errorf("timer cadence wrong:\n%s", timer)
	}
	if len(p.Enable) == 0 || !strings.Contains(strings.Join(p.Enable, "\n"), "enable --now") {
		t.Errorf("missing enable commands: %v", p.Enable)
	}
}

func TestDarwinPlan(t *testing.T) {
	p, err := Build(Options{Exe: "/opt/conductor", Home: "/Users/dev", GOOS: "darwin", Interval: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if p.OS != "darwin" || len(p.Files) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	plist := p.Files[0].Content
	if !strings.Contains(plist, "<string>/opt/conductor</string>") ||
		!strings.Contains(plist, "<string>sessions</string>") ||
		!strings.Contains(plist, "<integer>180</integer>") {
		t.Errorf("plist wrong:\n%s", plist)
	}
	if !strings.HasSuffix(p.Files[0].Path, "Library/LaunchAgents/dev.conductor.sessions.plist") {
		t.Errorf("plist path = %s", p.Files[0].Path)
	}
}

func TestUnsupportedOS(t *testing.T) {
	if _, err := Build(Options{Exe: "c", Home: "/h", GOOS: "plan9"}); err == nil {
		t.Fatal("expected an error for an unsupported OS")
	}
}

func TestSystemdDuration(t *testing.T) {
	cases := map[time.Duration]string{5 * time.Minute: "5min", 90 * time.Second: "90s", time.Hour: "60min"}
	for d, want := range cases {
		if got := systemdDuration(d); got != want {
			t.Errorf("systemdDuration(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestSystemdArgAndXMLEscape(t *testing.T) {
	if got := systemdArg(`/opt/my conductor/bin`); got != `"/opt/my conductor/bin"` {
		t.Errorf("systemdArg spaces = %q", got)
	}
	if got := systemdArg(`/a"b\c`); got != `"/a\"b\\c"` {
		t.Errorf("systemdArg escapes = %q", got)
	}
	if got := xmlEscape(`/a&b<c>d"e`); got != `/a&amp;b&lt;c&gt;d&quot;e` {
		t.Errorf("xmlEscape = %q", got)
	}
	// A path with an ampersand must not corrupt the generated plist.
	p, err := Build(Options{Exe: `/opt/a&b/conductor`, Home: "/Users/dev", GOOS: "darwin"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Files[0].Content, "<string>/opt/a&amp;b/conductor</string>") {
		t.Errorf("plist did not XML-escape the exe path:\n%s", p.Files[0].Content)
	}
}
