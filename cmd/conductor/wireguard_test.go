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

func writeWireGuardInventory(t *testing.T, root string) string {
	t.Helper()
	key := func(value byte) string {
		return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
	}
	body := "version: 1\nnetwork:\n  cidr: 10.88.0.0/24\ngateway:\n  name: gateway\n  address: 10.88.0.1\n  public_key: " + key(1) + "\n  endpoint: gateway.internal:51820\nnodes:\n  - name: worker-a\n    address: 10.88.0.2\n    public_key: " + key(2) + "\n"
	path := filepath.Join(root, "mesh.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWireGuardRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		nil, {"activate"}, {"render"}, {"render", "--manifest", "mesh.yaml"},
		{"render", "--out", "plan"}, {"render", "--unknown"},
		{"render", "--manifest"}, {"render", "--manifest", "mesh.yaml", "--out", "plan", "extra"},
	} {
		if err := cmdWireGuard(args); err == nil {
			t.Errorf("accepted args %q", args)
		}
	}
}

func TestWireGuardInputAndOutputIOErrors(t *testing.T) {
	root := t.TempDir()
	manifest := writeWireGuardInventory(t, root)
	for _, input := range []string{filepath.Join(root, "missing.yaml"), root} {
		output := filepath.Join(root, "uncreated-plan")
		if err := cmdWireGuard([]string{"render", "--manifest", input, "--out", output}); err == nil {
			t.Fatalf("unreadable manifest %s accepted", input)
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			t.Fatalf("input failure created output: %v", err)
		}
	}
	existing := filepath.Join(root, "owned-file")
	if err := os.WriteFile(existing, []byte("local private configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(root, "dangling-link")
	target := filepath.Join(root, "absent-target")
	if err := os.Symlink(target, dangling); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{existing, dangling, filepath.Join(root, "missing-parent", "plan")} {
		if err := renderWireGuardFiles(manifest, output); err == nil {
			t.Errorf("invalid output %s accepted", output)
		}
	}
	if body, err := os.ReadFile(existing); err != nil || string(body) != "local private configuration" {
		t.Fatalf("existing file changed: %q, %v", body, err)
	}
	if got, err := os.Readlink(dangling); err != nil || got != target {
		t.Fatalf("existing symlink changed: %q, %v", got, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("symlink target created: %v", err)
	}
}

func TestWireGuardCLIWorksOffline(t *testing.T) {
	root := t.TempDir()
	manifest := writeWireGuardInventory(t, root)
	output := filepath.Join(root, "plan")
	stdout, stderr, code := runCLI(t, "wireguard", "render", "--manifest", manifest, "--out", output)
	if code != 0 || stderr != "" || !strings.Contains(stdout, "add each private key on its owning node") {
		t.Fatalf("offline render exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if body, err := os.ReadFile(filepath.Join(output, "worker-a.conf")); err != nil || !strings.Contains(string(body), "AllowedIPs = 10.88.0.0/24") {
		t.Fatalf("offline CLI did not render worker configuration: %q, %v", body, err)
	}
	for _, args := range [][]string{
		{"wireguard", "activate"}, {"wireguard", "render", "--unknown"},
		{"wireguard", "render", "--manifest", manifest, "--out", output, "extra"},
		{"wireguard", "render", "--manifest", manifest, "--out", output},
	} {
		stdout, stderr, code := runCLI(t, args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "error:") {
			t.Errorf("args %q: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
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
