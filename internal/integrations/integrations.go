// Package integrations connects Conductor to the coding tools people actually use.
//
// Every tool has its own idea of where MCP servers are declared and how hooks are wired, and
// none of them agree. This package knows each format well enough to merge one entry into an
// existing file — never clobbering the rest of it, never writing a bearer token into a file
// that would be committed — and to report whether a tool is currently connected.
//
// The unit of work is a plan: a set of file operations computed from the current state of the
// tool's configuration. Planning never writes, so `conductor integrate --print` and
// `conductor doctor` are the same code path as the real install, minus Apply.
package integrations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ServerName is the key every tool's configuration uses for Conductor's MCP server.
const ServerName = "conductor"

// MCPCommand is the stdio gateway binary.
const MCPCommand = "conductor-mcp"

// Transport selects how a tool reaches the gateway.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Options describes one integration request.
type Options struct {
	// Root is the repository root; project-level files are written beneath it.
	Root string
	// Home is the user's home directory; user-level files are written beneath it.
	Home string
	// Project is the project slug the gateway should serve.
	Project string
	// Endpoint is the control plane URL. The HTTP transport mounts <Endpoint>/mcp.
	Endpoint string
	// Token is the caller's bearer token. It is written to disk only for tools that offer no
	// environment interpolation, and only into user-level files (see tokenPolicy).
	Token string
	// Transport is TransportStdio or TransportHTTP.
	Transport string
	// Global prefers the user-level configuration where a tool has both.
	Global bool
	// Hooks installs enforcement hooks or plugins where the tool supports them.
	Hooks bool
	// Rules installs instruction files (Cursor rules) where the tool reads them.
	Rules bool
	// Remove uninstalls instead of installing.
	Remove bool
	// ManagedBlock is the instruction text written into rules files.
	ManagedBlock string

	Getenv   func(string) string
	LookPath func(string) (string, error)
}

func (o Options) getenv(key string) string {
	if o.Getenv != nil {
		return o.Getenv(key)
	}
	return os.Getenv(key)
}

func (o Options) lookPath(name string) bool {
	look := o.LookPath
	if look == nil {
		look = exec.LookPath
	}
	_, err := look(name)
	return err == nil
}

func (o Options) transport() string {
	if o.Transport == "" {
		return TransportStdio
	}
	return o.Transport
}

// httpURL is the Streamable HTTP endpoint, with the project selected in the query string so
// every client, whatever headers it can send, reaches the right project.
func (o Options) httpURL() string {
	return strings.TrimRight(o.Endpoint, "/") + "/mcp?project=" + o.Project
}

func (o Options) stdioArgs() []any {
	return []any{"--project", o.Project}
}

// configHome is $XDG_CONFIG_HOME or ~/.config.
func (o Options) configHome() string {
	if v := o.getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	return filepath.Join(o.Home, ".config")
}

// Op is one planned file operation. Content nil means delete.
type Op struct {
	Path    string      `json:"path"`
	Action  string      `json:"action"` // create | update | unchanged | delete | skip
	Content []byte      `json:"-"`
	Mode    os.FileMode `json:"-"`
	Note    string      `json:"note,omitempty"`
}

// Result is a tool's plan plus what a person needs to know about it.
type Result struct {
	Tool     string   `json:"tool"`
	Ops      []Op     `json:"ops"`
	Commands []string `json:"commands,omitempty"` // shell commands to run instead of, or as well as, ops
	Warnings []string `json:"warnings,omitempty"`
	Next     string   `json:"next,omitempty"`
	// Snippet is what the user should paste by hand when a file could not be edited safely
	// (for example, a settings file with comments the parser cannot round-trip).
	Snippet string `json:"snippet,omitempty"`
	// Unparseable names the file that Snippet is for.
	Unparseable string `json:"unparseable,omitempty"`
}

// Changed reports whether applying the plan would touch anything.
func (r Result) Changed() bool {
	for _, op := range r.Ops {
		if op.Action == "create" || op.Action == "update" || op.Action == "delete" {
			return true
		}
	}
	return len(r.Commands) > 0
}

// Status is a tool's current connection state, for `conductor doctor`.
type Status struct {
	Tool       string `json:"tool"`
	Title      string `json:"title"`
	Detected   bool   `json:"detected"`
	Configured bool   `json:"configured"`
	Transport  string `json:"transport,omitempty"`
	ConfigPath string `json:"config_path,omitempty"`
	// HooksSupported is whether the tool can enforce anything; Hooks is whether it does.
	HooksSupported bool   `json:"hooks_supported"`
	Hooks          bool   `json:"hooks"`
	Fix            string `json:"fix,omitempty"`
}

// Tool is one supported coding tool.
type Tool struct {
	Name  string
	Title string
	// Detect reports whether the tool appears to be installed, and how it was recognized.
	Detect func(o Options) (bool, string)
	Plan   func(o Options) (Result, error)
	Status func(o Options) Status
}

// Tools lists every supported tool in display order.
var Tools = []Tool{claudeTool, cursorTool, codexTool, opencodeTool, vscodeTool, windsurfTool, zedTool, geminiTool}

// Lookup finds a tool by name.
func Lookup(name string) (Tool, bool) {
	for _, t := range Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Names lists tool names.
func Names() []string {
	out := make([]string, 0, len(Tools))
	for _, t := range Tools {
		out = append(out, t.Name)
	}
	return out
}

// Detected returns the tools that appear to be installed.
func Detected(o Options) []Tool {
	var out []Tool
	for _, t := range Tools {
		if ok, _ := t.Detect(o); ok {
			out = append(out, t)
		}
	}
	return out
}

// Statuses reports every tool's connection state.
func Statuses(o Options) []Status {
	out := make([]Status, 0, len(Tools))
	for _, t := range Tools {
		out = append(out, t.Status(o))
	}
	return out
}

// ErrUnparseable marks a configuration file this package refuses to rewrite because it could
// not parse it — a settings file with comments, say. Clobbering it would be far worse than
// asking the user to paste a snippet.
var ErrUnparseable = errors.New("configuration file could not be parsed")

// Apply performs a plan's operations. run executes the plan's shell commands; nil skips them.
func Apply(r Result, run func(cmd string) error) error {
	for _, op := range r.Ops {
		switch op.Action {
		case "create", "update":
			if err := os.MkdirAll(filepath.Dir(op.Path), 0o755); err != nil {
				return err
			}
			mode := op.Mode
			if info, err := os.Stat(op.Path); err == nil {
				mode = info.Mode().Perm() // keep whatever the user chose
			}
			if mode == 0 {
				mode = 0o644
			}
			if err := os.WriteFile(op.Path, op.Content, mode); err != nil {
				return err
			}
		case "delete":
			if err := os.Remove(op.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if run != nil {
		for _, cmd := range r.Commands {
			if err := run(cmd); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// File planning helpers
// ---------------------------------------------------------------------------

// planWrite compares desired content with what is on disk.
func planWrite(path string, content []byte, mode os.FileMode, note string) Op {
	op := Op{Path: path, Content: content, Mode: mode, Note: note}
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		op.Action = "create"
	case err == nil && bytes.Equal(existing, content):
		op.Action = "unchanged"
	default:
		op.Action = "update"
	}
	return op
}

// planDelete removes a file this package created.
func planDelete(path string, note string) Op {
	op := Op{Path: path, Note: note}
	if _, err := os.Stat(path); err == nil {
		op.Action = "delete"
	} else {
		op.Action = "unchanged"
	}
	return op
}

// readJSONObject loads a JSON object file, treating a missing file as empty. A file that
// exists but does not parse yields ErrUnparseable — it may be JSONC, and rewriting it would
// drop the user's comments.
func readJSONObject(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnparseable, path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// planJSON reads a JSON object file, applies mutate, and plans the write. Keys are emitted
// sorted, which is the price of a round trip through map[string]any; the alternative — a
// custom ordered parser — is not worth it for a file whose order nothing depends on.
func planJSON(path string, mutate func(m map[string]any), note string) (Op, error) {
	m, err := readJSONObject(path)
	if err != nil {
		return Op{}, err
	}
	mutate(m)
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Op{}, err
	}
	body = append(body, '\n')
	return planWrite(path, body, 0o644, note), nil
}

// section returns m[key] as an object, creating it when absent.
func section(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	sub := map[string]any{}
	m[key] = sub
	return sub
}

// removeKey deletes m[parent][key], dropping the parent object when it becomes empty.
func removeKey(m map[string]any, parent, key string) {
	sub, ok := m[parent].(map[string]any)
	if !ok {
		return
	}
	delete(sub, key)
	if len(sub) == 0 {
		delete(m, parent)
	}
}

// hasKey reports whether m[parent][key] exists.
func hasKey(m map[string]any, parent, key string) bool {
	sub, ok := m[parent].(map[string]any)
	if !ok {
		return false
	}
	_, ok = sub[key]
	return ok
}

// entryTransport guesses which transport an existing entry uses.
func entryTransport(m map[string]any, parent, key string) string {
	sub, _ := m[parent].(map[string]any)
	entry, _ := sub[key].(map[string]any)
	if entry == nil {
		return ""
	}
	for _, k := range []string{"url", "httpUrl", "serverUrl"} {
		if _, ok := entry[k]; ok {
			return TransportHTTP
		}
	}
	if _, ok := entry["command"]; ok {
		return TransportStdio
	}
	return ""
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func prettyJSON(v any) string {
	body, _ := json.MarshalIndent(v, "", "  ")
	return string(body)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// readTextFile returns a file's content, or "" when it does not exist or cannot be read.
func readTextFile(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(body)
}
