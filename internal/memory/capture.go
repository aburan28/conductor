package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aburan28/conductor/internal/config"
)

// HookEvent contains only the fields needed to derive a compact observation. The raw
// hook payload is never written to Redis or to Conductor's shared control plane.
type HookEvent struct {
	SessionID            string          `json:"session_id"`
	TurnID               string          `json:"turn_id"`
	Cwd                  string          `json:"cwd"`
	HookEventName        string          `json:"hook_event_name"`
	ToolName             string          `json:"tool_name"`
	ToolUseID            string          `json:"tool_use_id"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Prompt               string          `json:"prompt"`
	Error                string          `json:"error"`
}

func ProjectForDir(dir string) string {
	if explicit := os.Getenv("CONDUCTOR_MEMORY_PROJECT"); explicit != "" {
		return "explicit:" + explicit
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	root, err := config.FindRoot(dir)
	if err != nil {
		root = findGitRoot(dir)
	}
	// The project.yaml ID is checkout-controlled. A second repository can reuse
	// it, so only this machine's checked Git identity or canonical root may scope
	// private observations. Linked worktrees share the registered common Git dir.
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
	}
	if common := gitCommonDir(root); common != "" {
		return "git:" + common
	}
	return "path:" + filepath.Clean(root)
}

func findGitRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	for {
		if info, err := os.Lstat(filepath.Join(abs, ".git")); err == nil &&
			(info.IsDir() || info.Mode().IsRegular()) {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return filepath.Clean(dir)
		}
		abs = parent
	}
}

func readSmallFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func canonicalPath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	return filepath.Clean(path)
}

func gitCommonDir(root string) string {
	git := filepath.Join(root, ".git")
	info, err := os.Lstat(git)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return canonicalPath(git)
	}
	if !info.Mode().IsRegular() {
		return ""
	}
	line := readSmallFile(git)
	if !strings.HasPrefix(line, "gitdir: ") {
		return ""
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir: "))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	gitDir = canonicalPath(gitDir)
	// A copied or forged .git file cannot claim another checkout's memory.
	registered := readSmallFile(filepath.Join(gitDir, "gitdir"))
	if registered == "" {
		return ""
	}
	if !filepath.IsAbs(registered) {
		registered = filepath.Join(gitDir, registered)
	}
	if canonicalPath(registered) != canonicalPath(git) {
		return ""
	}
	common := readSmallFile(filepath.Join(gitDir, "commondir"))
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	return canonicalPath(common)
}

var secretAssignment = regexp.MustCompile(`(?i)\b([a-z0-9_\-]*(?:password|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_\-]*)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var secretFlag = regexp.MustCompile(`(?i)(--[a-z0-9_\-]*(?:password|secret|token|key)[a-z0-9_\-]*)\s+(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
var bearerToken = regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/-]{8,}`)
var privateKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

func redact(s string) string {
	s = privateKey.ReplaceAllString(s, "[private key redacted]")
	s = secretAssignment.ReplaceAllString(s, "$1=[redacted]")
	s = secretFlag.ReplaceAllString(s, "$1 [redacted]")
	s = bearerToken.ReplaceAllString(s, "Bearer [redacted]")
	return s
}

func sensitivePath(path string) bool {
	clean := strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
	name := filepath.Base(clean)
	for _, part := range strings.Split(clean, "/") {
		switch part {
		case ".ssh", ".aws", ".gnupg", ".kube", ".docker", ".config", "secrets", "credentials":
			return true
		}
	}
	if name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasSuffix(name, ".pem") ||
		strings.HasSuffix(name, ".key") || strings.Contains(name, "credentials") ||
		strings.Contains(name, "secret") || name == ".npmrc" || name == ".netrc" ||
		name == ".pypirc" || name == "id_rsa" || name == "id_ed25519" ||
		name == "id_ecdsa" || name == "kubeconfig" {
		return true
	}
	return false
}

func toolPath(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "filePath", "notebook_path", "notebookPath", "path"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func toolCommand(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"command", "cmd"} {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	return ""
}

func patchPaths(raw json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var patch string
	for _, key := range []string{"patchText", "command", "cmd"} {
		if v, ok := m[key].(string); ok && strings.Contains(v, "*** Begin Patch") {
			patch = v
			break
		}
	}
	if patch == "" {
		return nil
	}
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: ", "*** Move to: "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			path := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if path == "" || sensitivePath(path) || seen[path] {
				break
			}
			seen[path] = true
			paths = append(paths, compactText(path, 300))
			break
		}
		if len(paths) == 16 {
			break
		}
	}
	return paths
}

func compactText(s string, max int) string {
	s = strings.Join(strings.Fields(redact(s)), " ")
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		s += "…"
	}
	return s
}

// FromHook produces a bounded, private observation from a Claude/Codex hook. The final
// assistant message is already a human-readable turn summary; tool records retain only
// action metadata, avoiding a second raw transcript database.
func FromHook(body []byte, harness string) (*Observation, error) {
	var e HookEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	project := ProjectForDir(e.Cwd)
	if project == "" {
		return nil, nil
	}
	o := &Observation{Project: project, Session: e.SessionID, Harness: harness, CreatedAt: time.Now().UTC()}
	switch e.HookEventName {
	case "Stop":
		o.Kind = "turn_summary"
		o.Summary = compactText(e.LastAssistantMessage, 3000)
	case "PostToolUse", "PostToolUseFailure":
		o.Kind = "tool_action"
		tool := compactText(e.ToolName, 80)
		if tool == "" {
			return nil, nil
		}
		path := toolPath(e.ToolInput)
		if paths := patchPaths(e.ToolInput); len(paths) > 0 {
			o.Files = paths
			o.Summary = tool + " changed " + strings.Join(paths, ", ")
		} else if path != "" {
			if sensitivePath(path) {
				return nil, nil
			}
			path = compactText(path, 300)
			o.Files = []string{path}
			o.Summary = tool + " " + path
		} else if cmd := strings.TrimSpace(toolCommand(e.ToolInput)); cmd != "" {
			// Persist only the action name. Even a familiar test command may have
			// credentials or private data in its arguments.
			action := safeBuildAction(cmd)
			if action == "" {
				return nil, nil
			}
			o.Summary = tool + " " + action
		} else {
			return nil, nil
		}
		if e.HookEventName == "PostToolUseFailure" {
			o.Summary += " (failed)"
		}
	default:
		return nil, nil
	}
	if o.Summary == "" {
		return nil, nil
	}
	// Replayed hooks use the same ID. Hashing the event avoids indexing duplicates.
	h := sha256.Sum256(append([]byte(harness+"\x00"), body...))
	o.ID = hex.EncodeToString(h[:16])
	return o, nil
}

func safeBuildAction(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, action := range []string{"go test", "go vet", "cargo test", "pytest", "python -m pytest", "npm test", "npm run test", "pnpm test", "make test"} {
		if cmd == action || strings.HasPrefix(cmd, action+" ") {
			return action
		}
	}
	return ""
}
