package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/privacy"
)

// fakeDoctorEnv is a machine with git, without Docker, and with nothing listening for Postgres.
func fakeDoctorEnv(clientVersion string) doctorEnv {
	return doctorEnv{
		lookPath: func(name string) (string, error) {
			if name == "git" {
				return "/usr/bin/git", nil
			}
			return "", errors.New("not found")
		},
		output:     func(string, ...string) (string, error) { return "git version 2.43.0", nil },
		daemon:     func() (string, error) { return "/opt/conductor/conductord", nil },
		portOpen:   func(string, string) bool { return false },
		dockerUp:   func() bool { return false },
		getenv:     func(string) string { return "" },
		clientVers: clientVersion,
	}
}

func healthServer(t *testing.T, body map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDoctorWarnsOnVersionMismatch(t *testing.T) {
	ep := healthServer(t, map[string]any{"status": "ok", "version": "v0.1.0"})
	out := collectDoctor(context.Background(), client.Credentials{Endpoint: ep}, "", fakeDoctorEnv("v0.3.0"))
	if out.ServerVersion != "v0.1.0" || !strings.Contains(out.VersionWarning, "v0.3.0") {
		t.Fatalf("server %q, warning %q", out.ServerVersion, out.VersionWarning)
	}
	if !out.DatabaseOK {
		t.Errorf("a healthy server's database should read as ok: %q", out.Database)
	}

	var buf bytes.Buffer
	printDoctor(&buf, out)
	if !strings.Contains(buf.String(), "WARNING") {
		t.Errorf("printed report carries no warning:\n%s", buf.String())
	}
}

func TestDoctorQuietWhenVersionsMatch(t *testing.T) {
	ep := healthServer(t, map[string]any{"status": "ok", "version": "v0.3.0"})
	out := collectDoctor(context.Background(), client.Credentials{Endpoint: ep}, "", fakeDoctorEnv("v0.3.0"))
	if out.VersionWarning != "" {
		t.Errorf("unexpected warning: %q", out.VersionWarning)
	}
}

// A server from before the version field says nothing; that is itself the tell.
func TestDoctorFlagsServerWithoutVersion(t *testing.T) {
	ep := healthServer(t, map[string]any{"status": "ok"})
	out := collectDoctor(context.Background(), client.Credentials{Endpoint: ep}, "", fakeDoctorEnv("v0.3.0"))
	if !strings.Contains(out.VersionWarning, "older") {
		t.Errorf("warning = %q", out.VersionWarning)
	}
}

// Nothing running on this machine: doctor checks the database itself, and says what to do.
func TestDoctorChecksLocalDatabaseWhenServerIsDown(t *testing.T) {
	out := collectDoctor(context.Background(), client.Credentials{Endpoint: "http://127.0.0.1:1"}, "", fakeDoctorEnv("v0.3.0"))
	if out.DatabaseOK || !strings.Contains(out.Database, defaultLocalDBAddr) || !strings.Contains(out.Database, "conductor up") {
		t.Errorf("database = %q (ok=%v)", out.Database, out.DatabaseOK)
	}
	if out.VersionWarning != "" {
		t.Errorf("no server, so no version comparison; got %q", out.VersionWarning)
	}
	if out.Git != "2.43.0" || out.Docker != "not installed" || out.Conductord == "" {
		t.Errorf("machine checks: git %q, docker %q, conductord %q", out.Git, out.Docker, out.Conductord)
	}

	env := fakeDoctorEnv("v0.3.0")
	env.portOpen = func(host, port string) bool { return port == "5433" }
	env.getenv = func(k string) string {
		if k == "DATABASE_URL" {
			return "postgres://me:secret@127.0.0.1:5433/conductor"
		}
		return ""
	}
	out = collectDoctor(context.Background(), client.Credentials{Endpoint: "http://127.0.0.1:1"}, "", env)
	if !out.DatabaseOK || !strings.Contains(out.Database, "127.0.0.1:5433") {
		t.Errorf("database = %q (ok=%v)", out.Database, out.DatabaseOK)
	}
	if strings.Contains(out.Database, "secret") {
		t.Errorf("the report leaks the DSN password: %q", out.Database)
	}
}

// Codex is off by default, so the registry never probed it and doctor never mentioned it.
func TestDoctorListsDisabledHarnesses(t *testing.T) {
	out := collectDoctor(context.Background(), client.Credentials{Endpoint: "http://127.0.0.1:1"}, "", fakeDoctorEnv("v0.3.0"))
	found := false
	for _, c := range out.Disabled {
		if c.Kind == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("codex missing from the disabled harnesses: %+v", out.Disabled)
	}
	var buf bytes.Buffer
	printDoctor(&buf, out)
	if !strings.Contains(buf.String(), "codex") {
		t.Errorf("printed report omits codex:\n%s", buf.String())
	}
}

func TestStatusNextStepOnlyWhenIdle(t *testing.T) {
	var buf bytes.Buffer
	printStatusNextStep(&buf, coord.StatusSummary{Project: "myrepo"})
	if !strings.Contains(buf.String(), "conductor task create") {
		t.Errorf("idle project prints no next step: %q", buf.String())
	}

	buf.Reset()
	printStatusNextStep(&buf, coord.StatusSummary{Project: "myrepo", Ready: []privacy.TaskView{{Ref: "T-1"}}})
	if buf.Len() != 0 {
		t.Errorf("busy project printed a hint: %q", buf.String())
	}
}
