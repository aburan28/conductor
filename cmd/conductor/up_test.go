package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListenAddrFromEndpoint(t *testing.T) {
	cases := []struct {
		ep   string
		want string
	}{
		{"http://localhost:8080", "localhost:8080"},
		{"http://127.0.0.1:9999", "127.0.0.1:9999"},
		{"http://localhost:8080/", "localhost:8080"},
		{"http://0.0.0.0:8081", "127.0.0.1:8081"},
		{"https://example.com:8443", "example.com:8443"},
		{"not a url", "127.0.0.1:8080"},
		{"", "127.0.0.1:8080"},
	}
	for _, tc := range cases {
		if got := listenAddrFromEndpoint(tc.ep); got != tc.want {
			t.Errorf("listenAddrFromEndpoint(%q) = %q, want %q", tc.ep, got, tc.want)
		}
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, host := range []string{"", "localhost", "127.0.0.1", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "10.0.0.5", "example.com"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestRemoteEndpoint(t *testing.T) {
	if remoteEndpoint("http://localhost:8080") {
		t.Error("http://localhost:8080 reported remote")
	}
	if remoteEndpoint("http://127.0.0.1:8080") {
		t.Error("http://127.0.0.1:8080 reported remote")
	}
	if !remoteEndpoint("https://conductor.example.com") {
		t.Error("https://conductor.example.com not reported remote")
	}
}

func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := tailLines(path, 2); got != "three\nfour" {
		t.Errorf("tailLines = %q, want %q", got, "three\nfour")
	}
	if got := tailLines(filepath.Join(dir, "missing"), 2); got != "<no log>" {
		t.Errorf("tailLines(missing) = %q, want <no log>", got)
	}
}

func TestFindComposeFileSkipsUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	// A compose file that is not conductor's must not be picked up.
	other := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(other, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(oldwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if d, n := findComposeFile(); d != "" || n != "" {
		t.Errorf("findComposeFile() = %q %q, want empty for an unrelated compose file", d, n)
	}
	// The conductor compose file is recognized.
	if err := os.WriteFile(other, []byte("services:\n  db:\n    container_name: conductor-db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, n := findComposeFile(); filepath.Base(d) != filepath.Base(dir) || n != "docker-compose.yml" {
		t.Errorf("findComposeFile() = %q %q, want the local conductor compose file", d, n)
	}
}
