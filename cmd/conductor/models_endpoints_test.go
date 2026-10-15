package main

import (
	"strings"
	"testing"
	"time"

	"github.com/aburan28/conductor/internal/modelsrc"
)

func TestAgo(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	for _, c := range []struct {
		in   *time.Time
		want string
	}{
		{nil, "never"}, {at(10 * time.Second), "just now"}, {at(5 * time.Minute), "5m ago"},
		{at(3 * time.Hour), "3h ago"}, {at(72 * time.Hour), "3d ago"},
	} {
		if got := ago(c.in); got != c.want {
			t.Errorf("ago = %q, want %q", got, c.want)
		}
	}
}

func TestPresetNamesListEveryPreset(t *testing.T) {
	names := presetNames()
	for _, p := range modelsrc.Presets {
		if !strings.Contains(names, p.Name) {
			t.Errorf("presetNames() omits %q: %s", p.Name, names)
		}
	}
}

// Only the local presets are probed by `models discover`: a hosted provider is never called
// with no key, and a machine scan must not reach out to the internet.
func TestLocalProbesAreAllLocalAndKeyless(t *testing.T) {
	probes := localProbes()
	if len(probes) == 0 {
		t.Fatal("no local presets to probe")
	}
	for _, p := range probes {
		if !p.Local || p.KeyHint != "" {
			t.Errorf("%s is probed but is not a keyless local preset", p.Name)
		}
		if !strings.HasPrefix(p.BaseURL, "http://localhost") && !strings.HasPrefix(p.BaseURL, "http://127.") {
			t.Errorf("%s: %s is not on this machine", p.Name, p.BaseURL)
		}
	}
}
