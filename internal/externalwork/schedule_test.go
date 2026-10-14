package externalwork

import (
	"strings"
	"testing"
	"time"
)

func TestBuildScheduleDarwinAndLinux(t *testing.T) {
	o := ScheduleOptions{Exe: "/opt/conductor bin/conductor", Home: "/Users/dev", Workspace: "/repo & work", GOOS: "darwin", Interval: 15 * time.Minute, Since: 7 * 24 * time.Hour}
	p, err := BuildSchedule(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 1 || p.Label != "dev.conductor.external-work" {
		t.Fatalf("darwin plan: %#v", p)
	}
	for _, want := range []string{"<integer>900</integer>", "/repo &amp; work", "<string>scan</string>", "<string>--workspace</string>"} {
		if !strings.Contains(p.Files[0].Content, want) {
			t.Errorf("missing %q", want)
		}
	}
	o.GOOS = "linux"
	o.Home = "/home/dev"
	p, err = BuildSchedule(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 || p.Service == "" || p.Timer == "" {
		t.Fatalf("linux plan: %#v", p)
	}
	if !strings.Contains(p.Files[0].Content, `"/repo & work"`) || !strings.Contains(p.Files[1].Content, "OnUnitActiveSec=900s") {
		t.Fatal("linux service or timer lost scan arguments")
	}
}

func TestBuildScheduleRejectsRunawayCadence(t *testing.T) {
	o := ScheduleOptions{Exe: "/bin/conductor", Home: "/home/dev", Workspace: "/repo", GOOS: "linux", Interval: time.Second}
	if _, err := BuildSchedule(o); err == nil {
		t.Fatal("accepted scan every second")
	}
}
