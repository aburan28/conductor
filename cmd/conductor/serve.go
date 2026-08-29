package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/adamburan/conductor/internal/config"
)

// Local vLLM launchers folded into the CLI so OpenCode workers share one entrypoint
// (DESIGN.md §16.4). Variants match .conductor/models.yaml OpenCode profiles.

func cmdServe(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(serveUsage)
		if len(args) == 0 {
			return fmt.Errorf("pick flash, glm53, or qwen")
		}
		return nil
	}

	script, err := locateServeScript()
	if err != nil {
		return err
	}

	cmd := exec.Command(script, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

const serveUsage = `conductor serve — start a local vLLM endpoint for OpenCode

  conductor serve flash              GLM-5.3-Flash (~310 GiB)
  conductor serve glm53              GLM-5.3 (~750 GiB)
  conductor serve qwen               Qwen 3.8 27B FP8
  conductor serve flash stop
  conductor serve status
  conductor serve smoke

Then:
  conductor wrap opencode --model vllm/qwen3.8-27b
  conductor wrap opencode --model vllm/zai-org/GLM-5.3-Flash
  conductor wrap opencode --model vllm/glm-5.3

Merge scripts/opencode.vllm.json into your OpenCode config so those ids resolve.
Only one model listens on :8000 at a time. Override with PORT / WEIGHTS / VLLM_IMAGE / TP.
`

func locateServeScript() (string, error) {
	if p := os.Getenv("CONDUCTOR_SERVE_SCRIPT"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("CONDUCTOR_SERVE_SCRIPT: %w", err)
		}
		return p, nil
	}

	candidates := []string{}
	if cwd, err := os.Getwd(); err == nil {
		if root, err := config.FindRoot(cwd); err == nil {
			candidates = append(candidates, filepath.Join(root, "scripts", "serve-local.sh"))
		}
		candidates = append(candidates, filepath.Join(cwd, "scripts", "serve-local.sh"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "scripts", "serve-local.sh"),
			filepath.Join(dir, "..", "scripts", "serve-local.sh"),
		)
	}

	seen := map[string]struct{}{}
	for _, p := range candidates {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() {
			return abs, nil
		}
	}
	return "", fmt.Errorf("scripts/serve-local.sh not found; set CONDUCTOR_SERVE_SCRIPT or run from the conductor checkout")
}

// serveVariantNormalizes CLI aliases used in tests and help text. The shell script is
// the source of truth at runtime; this list must stay a subset of that script.
func serveVariantAliases() map[string]string {
	return map[string]string{
		"flash": "flash", "glm53-flash": "flash", "glm-5.3-flash": "flash",
		"glm53": "glm53", "glm-5.3": "glm53",
		"qwen": "qwen", "qwen3.8": "qwen", "qwen3.8-27b": "qwen",
	}
}

func knownServeArg(arg string) bool {
	switch strings.ToLower(arg) {
	case "status", "stop", "smoke", "pull", "start", "serve", "-h", "--help":
		return true
	}
	_, ok := serveVariantAliases()[strings.ToLower(arg)]
	return ok
}
