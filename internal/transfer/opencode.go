package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// OpenCode keeps sessions in its own store and is the one harness with a first-class
// round trip:
//
//	opencode session list --format json
//	opencode session export <id>
//	opencode session import <file> --directory <dir>
//
// The export shape changed between v1 and v2 (see checkpoint.ParseOpenCode); import has
// only ever been exercised here against v2, with `opencode import <file>` kept as the
// fallback for older builds.

// openCodeSession is one row of `session list`.
type openCodeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Created   int64  `json:"created"`
	Updated   int64  `json:"updated"`
}

func openCodeDir(ctx context.Context, dir string) error {
	if dir == "" {
		return nil
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("OpenCode needs an existing project directory: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("OpenCode project %s is not a directory", dir)
	}
	return nil
}

// listOpenCode lists the sessions OpenCode associates with dir (its list command is
// project-scoped).
func listOpenCode(ctx context.Context, dir string) ([]openCodeSession, error) {
	if err := openCodeDir(ctx, dir); err != nil {
		return nil, err
	}
	raw, err := openCodeExec(ctx, dir, "session", "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("opencode session list: %w", err)
	}
	var out []openCodeSession
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("opencode session list returned unreadable JSON: %w", err)
	}
	return out, nil
}

func exportOpenCode(ctx context.Context, dir, id string) ([]byte, error) {
	if err := openCodeDir(ctx, dir); err != nil {
		return nil, err
	}
	raw, err := openCodeExec(ctx, dir, "session", "export", id)
	if err != nil {
		return nil, fmt.Errorf("opencode session export %s: %w", id, err)
	}
	return raw, nil
}

// importOpenCode loads a synthesized export into OpenCode, in dir. It returns the session id
// OpenCode reports ("Session already exists" is treated as a successful idempotent skip).
func importOpenCode(ctx context.Context, dir string, data []byte) (string, error) {
	if err := openCodeDir(ctx, dir); err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "conductor-transfer-opencode-*.json")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if os.Getenv("CONDUCTOR_TRANSFER_KEEP_DOC") == "" {
		defer os.Remove(name)
	} else {
		fmt.Fprintf(os.Stderr, "transfer: kept synthesized OpenCode document at %s\n", name)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	f.Close()

	out, err := openCodeExec(ctx, dir, "session", "import", name, "--directory", dir)
	if err != nil {
		// Older OpenCode builds expose import at the top level.
		if out2, err2 := openCodeExec(ctx, dir, "import", name); err2 == nil {
			return parseImportedID(string(out2)), nil
		}
		return "", fmt.Errorf("opencode session import: %w", err)
	}
	msg := string(out)
	if strings.Contains(msg, "already exists") {
		return "", errAlreadyExists
	}
	return parseImportedID(msg), nil
}

var errAlreadyExists = errors.New("session already exists in OpenCode")

func parseImportedID(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "Imported session:"); i >= 0 {
			return strings.TrimSpace(line[i+len("Imported session:"):])
		}
	}
	return ""
}

// openCodeModel picks a model pair the target accepts. OpenCode validates the model id on
// import, so a translated session carries one the installation actually knows; an explicit
// CONDUCTOR_TRANSFER_OPENCODE_MODEL (provider/model) wins.
func openCodeModel(ctx context.Context, dir string) (providerID, modelID string, err error) {
	if v := strings.TrimSpace(os.Getenv("CONDUCTOR_TRANSFER_OPENCODE_MODEL")); v != "" {
		return splitModel(v)
	}
	if err := openCodeDir(ctx, dir); err != nil {
		return "", "", err
	}
	raw, err := openCodeExec(ctx, dir, "models")
	if err != nil {
		return "", "", fmt.Errorf("opencode models: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "DESCRIPTION") {
			continue
		}
		if p, m, ok := splitModelOK(line); ok {
			return p, m, nil
		}
	}
	return "", "", errors.New("no OpenCode model is available; set CONDUCTOR_TRANSFER_OPENCODE_MODEL=provider/model")
}

func splitModel(v string) (string, string, error) {
	p, m, ok := splitModelOK(v)
	if !ok {
		return "", "", fmt.Errorf("model %q must be provider/model", v)
	}
	return p, m, nil
}

func splitModelOK(v string) (string, string, bool) {
	i := strings.Index(v, "/")
	if i <= 0 || i == len(v)-1 {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

// openCodeExportTime reads the timestamps a v1 or v2 export records, in milliseconds.
func openCodeExportTime(data []byte) (created, updated int64, cwd, title string) {
	var doc struct {
		Info struct {
			Title     string `json:"title"`
			Directory string `json:"directory"`
			Location  struct {
				Directory string `json:"directory"`
			} `json:"location"`
			Time struct {
				Created int64 `json:"created"`
				Updated int64 `json:"updated"`
			} `json:"time"`
		} `json:"info"`
	}
	_ = json.Unmarshal(data, &doc)
	cwd = doc.Info.Directory
	if cwd == "" {
		cwd = doc.Info.Location.Directory
	}
	return doc.Info.Time.Created, doc.Info.Time.Updated, cwd, doc.Info.Title
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return time.Now().UnixMilli()
	}
	return t.UnixMilli()
}

// opencodeAvailable reports whether the CLI is installed, so export can say so clearly.
func opencodeAvailable() bool {
	_, err := exec.LookPath("opencode")
	return err == nil
}

// openCodeExec runs the OpenCode CLI with its stderr included in the error, which is the
// only way an import rejection ("Bad Request", a model it does not know) is diagnosable.
// It is a variable so tests can substitute canned output.
var openCodeExec = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return out, fmt.Errorf("opencode %s: %s", strings.Join(args, " "), msg)
		}
	}
	return out, err
}
