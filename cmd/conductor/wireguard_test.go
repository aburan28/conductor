package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireGuardRenderPrivateFilesAndRefusesReplacement(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "mesh.yaml")
	key := func(value byte) string {
		return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
	}
	body := "version: 1\nnetwork:\n  cidr: 10.88.0.0/24\ngateway:\n  name: gateway\n  address: 10.88.0.1\n  public_key: " + key(1) + "\n  endpoint: gateway.internal:51820\nnodes:\n  - name: worker-a\n    address: 10.88.0.2\n    public_key: " + key(2) + "\n"
	if err := os.WriteFile(manifest, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "plan")
	if err := cmdWireGuard([]string{"render", "--manifest", manifest, "--out", output}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "gateway.conf", "worker-a.conf", "README.md"} {
		info, err := os.Stat(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Errorf("%s accessible to other users: %v", name, info.Mode())
		}
	}
	config := filepath.Join(output, "gateway.conf")
	if err := os.WriteFile(config, []byte("locally filled private configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := renderWireGuardFiles(manifest, output); err == nil {
		t.Fatal("existing configuration directory was replaced")
	}
	if body, err := os.ReadFile(config); err != nil || string(body) != "locally filled private configuration" {
		t.Fatal("existing configuration changed")
	}
	link := filepath.Join(root, "symlink")
	if err := os.Symlink(output, link); err != nil {
		t.Fatal(err)
	}
	if err := renderWireGuardFiles(manifest, link); err == nil {
		t.Fatal("output symlink accepted")
	}
}

func TestWireGuardInvalidManifestCreatesNoOutput(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "invalid.yaml")
	if err := os.WriteFile(manifest, []byte("version: 999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "plan")
	if err := renderWireGuardFiles(manifest, output); err == nil {
		t.Fatal("invalid manifest accepted")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("invalid manifest left an output directory")
	}
}
