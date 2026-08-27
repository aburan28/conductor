package main

import (
	"testing"
	"time"
)

func TestJoinLinkRoundTrip(t *testing.T) {
	link := joinLink("https://conductor.team", "myrepo", "cdt_abc123")
	// The token must ride in the fragment, never the query — a browser must not send it to
	// the server.
	if got := link; !contains(got, "/#") || contains(got, "?token=") {
		t.Fatalf("link should carry the token in the fragment: %s", link)
	}
	creds, err := parseJoinLink(link)
	if err != nil {
		t.Fatalf("parseJoinLink: %v", err)
	}
	if creds.Endpoint != "https://conductor.team" {
		t.Errorf("endpoint = %q", creds.Endpoint)
	}
	if creds.Project != "myrepo" {
		t.Errorf("project = %q", creds.Project)
	}
	if creds.Token != "cdt_abc123" {
		t.Errorf("token = %q", creds.Token)
	}
}

// Both invite and dashboard links must keep the token in the fragment, never the query — a
// browser sends the query to the server, the fragment it does not.
func TestJoinLinkKeepsTokenOutOfQuery(t *testing.T) {
	link := joinLink("https://conductor.team", "myrepo", "cdt_secret")
	if contains(link, "?") {
		t.Fatalf("link must have no query string: %s", link)
	}
	if !contains(link, "#") || !contains(link[indexOf(link, "#"):], "token=cdt_secret") {
		t.Fatalf("token must be in the fragment: %s", link)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestParseJoinLinkForms(t *testing.T) {
	cases := []struct {
		name, in                        string
		wantEndpoint, wantProj, wantTok string
		wantErr                         bool
	}{
		{"fragment", "https://c.team/#token=cdt_x&project=p", "https://c.team", "p", "cdt_x", false},
		{"query (legacy dashboard)", "https://c.team/?project=p&token=cdt_y", "https://c.team", "p", "cdt_y", false},
		{"fragment on deep path", "https://c.team/tasks/T-1#token=cdt_z&project=p", "https://c.team", "p", "cdt_z", false},
		{"bare fragment body", "token=cdt_w&project=p", "", "p", "cdt_w", false},
		{"port preserved", "http://192.168.1.5:8080/#token=cdt_v&project=p", "http://192.168.1.5:8080", "p", "cdt_v", false},
		{"no token", "https://c.team/#project=p", "", "", "", true},
		{"garbage", "::::not a url", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			creds, err := parseJoinLink(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJoinLink(%q): %v", c.in, err)
			}
			if creds.Endpoint != c.wantEndpoint || creds.Project != c.wantProj || creds.Token != c.wantTok {
				t.Errorf("got endpoint=%q project=%q token=%q; want %q/%q/%q",
					creds.Endpoint, creds.Project, creds.Token, c.wantEndpoint, c.wantProj, c.wantTok)
			}
		})
	}
}

func TestParseFriendlyDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"12h", 12 * time.Hour, false},
		{"90m", 90 * time.Minute, false},
		{"1.5d", 36 * time.Hour, false},
		{"", 0, true},
		{"soon", 0, true},
		{"7dd", 0, true},
		// Non-positive durations must be rejected: a zero/negative TTL would slip past the
		// server's human-token cap and mint a non-expiring credential.
		{"0", 0, true},
		{"0h", 0, true},
		{"-5d", 0, true},
		{"-1w", 0, true},
		{"-12h", 0, true},
	}
	for _, c := range cases {
		got, err := parseFriendlyDuration(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseFriendlyDuration(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseFriendlyDuration(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseFriendlyDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsLoopbackEndpoint(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8080":   true,
		"http://127.0.0.1:8080":   true,
		"http://[::1]:8080":       true,
		"https://conductor.team":  false,
		"http://192.168.1.5:8080": false,
		"http://0.0.0.0:8080":     false, // all-interfaces bind is reachable, not loopback
		"https://10.0.0.2":        false,
	}
	for in, want := range cases {
		if got := isLoopbackEndpoint(in); got != want {
			t.Errorf("isLoopbackEndpoint(%q) = %v, want %v", in, got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
