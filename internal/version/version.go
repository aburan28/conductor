// Package version reports which build of Conductor is running.
//
// Release builds and `make build` stamp the version and commit at link time:
//
//	go build -ldflags "-X <module>/internal/version.version=v1.2.3 -X <module>/internal/version.commit=abc123"
//
// A binary built any other way (`go install`, `go build` in a checkout) is not stamped, so the
// values fall back to what the Go toolchain embedded: the module version for `go install
// …@vX.Y.Z`, and the VCS revision for a build inside a Git checkout. A user has to be able to
// tell a stale binary from a current one, and "unknown" helps nobody.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set with -ldflags -X. Left empty in unstamped builds; never read these directly.
var (
	version string
	commit  string
)

// Info describes the running build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	GoVersion string `json:"go"`
	Platform  string `json:"platform"`
}

// Get returns the stamped build information, falling back to the toolchain's build info.
func Get() Info {
	return resolve(version, commit, readBuildInfo)
}

// Version is the short version string, e.g. "v1.2.3" or "devel+abc123def456".
func Version() string { return Get().Version }

// String is the one-line form every binary prints for --version.
func (i Info) String() string {
	s := i.Version
	if i.Commit != "" && !strings.Contains(i.Version, shortRev(i.Commit)) {
		s += " (" + shortRev(i.Commit) + ")"
	}
	return fmt.Sprintf("%s, %s, %s", s, i.GoVersion, i.Platform)
}

// readBuildInfo is a variable so tests can supply build info without rebuilding.
var readBuildInfo = debug.ReadBuildInfo

func resolve(stampedVersion, stampedCommit string, read func() (*debug.BuildInfo, bool)) Info {
	info := Info{
		Version:   stampedVersion,
		Commit:    stampedCommit,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	bi, ok := read()
	if !ok {
		if info.Version == "" {
			info.Version = "devel"
		}
		return info
	}
	if bi.GoVersion != "" {
		info.GoVersion = bi.GoVersion
	}

	var revision string
	var modified bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if info.Commit == "" {
		info.Commit = revision
	}
	if info.Version != "" {
		return info
	}

	// `go install module@vX.Y.Z` records the module version. A build inside a checkout
	// records "(devel)" (or, since Go 1.24, a pseudo-version derived from VCS), so the
	// revision is what identifies it.
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Version = v
		return info
	}
	switch {
	case revision != "" && modified:
		info.Version = "devel+" + shortRev(revision) + "-dirty"
	case revision != "":
		info.Version = "devel+" + shortRev(revision)
	default:
		info.Version = "devel"
	}
	return info
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// Mismatch reports whether a client and a server version differ. An empty version (an older
// server that does not report one) is not a mismatch; the caller says it cannot tell.
func Mismatch(client, server string) bool {
	client, server = strings.TrimSpace(client), strings.TrimSpace(server)
	return client != "" && server != "" && client != server
}
