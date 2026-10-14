package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func buildInfo(mainVersion string, settings ...string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		bi := &debug.BuildInfo{GoVersion: "go1.25.14"}
		bi.Main.Version = mainVersion
		for i := 0; i+1 < len(settings); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return bi, true
	}
}

func TestStampedVersionWins(t *testing.T) {
	got := resolve("v1.2.3", "0123456789abcdef", buildInfo("(devel)", "vcs.revision", "ffff"))
	if got.Version != "v1.2.3" || got.Commit != "0123456789abcdef" {
		t.Fatalf("got %+v, want the stamped values", got)
	}
	if s := got.String(); !strings.HasPrefix(s, "v1.2.3 (0123456789ab), go1.25.14, ") {
		t.Errorf("String() = %q", s)
	}
}

// `go install github.com/…/cmd/conductor@v1.2.3` is not stamped; the module version is the
// only thing that identifies it.
func TestGoInstallUsesModuleVersion(t *testing.T) {
	got := resolve("", "", buildInfo("v1.2.3"))
	if got.Version != "v1.2.3" {
		t.Fatalf("Version = %q, want v1.2.3", got.Version)
	}
}

func TestCheckoutBuildUsesRevision(t *testing.T) {
	got := resolve("", "", buildInfo("(devel)", "vcs.revision", "0123456789abcdef", "vcs.modified", "true"))
	if got.Version != "devel+0123456789ab-dirty" {
		t.Fatalf("Version = %q", got.Version)
	}
	if s := got.String(); strings.Count(s, "0123456789ab") != 1 {
		t.Errorf("String() repeats the revision: %q", s)
	}
}

func TestNoBuildInfo(t *testing.T) {
	got := resolve("", "", func() (*debug.BuildInfo, bool) { return nil, false })
	if got.Version != "devel" {
		t.Fatalf("Version = %q, want devel", got.Version)
	}
}

func TestMismatch(t *testing.T) {
	cases := []struct {
		client, server string
		want           bool
	}{
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "", false}, // an older server that reports nothing
		{"devel+abc", "v1.2.3", true},
	}
	for _, c := range cases {
		if got := Mismatch(c.client, c.server); got != c.want {
			t.Errorf("Mismatch(%q, %q) = %v, want %v", c.client, c.server, got, c.want)
		}
	}
}
