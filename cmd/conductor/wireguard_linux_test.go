package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireGuardCLIWriteFailureRemovesOnlyNewOutput(t *testing.T) {
	root := t.TempDir()
	manifest := writeWireGuardInventory(t, root)
	output := filepath.Join(root, "incomplete-plan")
	// Limit this child alone. Ignoring SIGXFSZ makes Write return EFBIG so the
	// command's cleanup executes, even when tests run as root.
	cmd := exec.Command("sh", "-c", `ulimit -f 1; trap '' XFSZ; exec "$@"`, "sh", buildCLI(t), "wireguard", "render", "--manifest", manifest, "--out", output)
	body, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(body), "file too large") {
		t.Fatalf("write-limit failure = %v, %s", err, body)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("failed write left partial output: %v", err)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("cleanup removed input inventory: %v", err)
	}
}
