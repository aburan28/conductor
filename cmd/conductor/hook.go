package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/client"
	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/coord"
	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/localstate"
	"github.com/adamburan/conductor/internal/privacy"
)

// Hooks are how a coding tool asks Conductor a question at the moment it matters — "may I
// edit this file?" — without spending a model turn on it (DESIGN.md §17.4). Claude Code
// runs `conductor hook pre-tool` from its PreToolUse hook; the OpenCode plugin calls the same
// command. The contract is the exit code: 0 lets the edit through, 2 blocks it and the text
// on stderr is what the model reads.
//
// Two rules hold throughout. Hooks fail open — a control plane that is down must not brick
// an editor, so any transport or auth failure exits 0 with one line on stderr. And hooks read
// nothing they do not need: of the JSON a tool hands them, only the tool name, the working
// directory, and the file path are decoded. File contents, tool arguments, and the transcript
// path have no field to land in.

// hookTimeout bounds a hook's single round trip. An editor is waiting.
const hookTimeout = 5 * time.Second

func cmdHook(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor hook <pre-tool|session-start|session-end>")
	}
	switch args[0] {
	case "pre-tool":
		return hookPreTool(ctx, args[1:])
	case "session-start":
		return hookSessionStart(ctx, args[1:])
	case "session-end":
		return hookSessionEnd(ctx, args[1:])
	default:
		return fmt.Errorf("unknown hook event %q", args[0])
	}
}

// hookInput is the subset of a tool's hook payload this command reads. It is deliberately
// tiny: there is no field for file content or tool arguments beyond a path, so they cannot be
// decoded even by accident.
type hookInput struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolInput     struct {
		FilePath      string `json:"file_path"`
		FilePathCamel string `json:"filePath"`
		NotebookPath  string `json:"notebook_path"`
		Path          string `json:"path"`
	} `json:"tool_input"`
}

func (h hookInput) path() string {
	return firstNonEmptyString(h.ToolInput.FilePath, h.ToolInput.FilePathCamel, h.ToolInput.NotebookPath, h.ToolInput.Path)
}

// readHookInput decodes the hook payload from stdin when one was piped in.
func readHookInput(r io.Reader, isTerminal bool) (hookInput, error) {
	var in hookInput
	if isTerminal {
		return in, nil
	}
	body, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return in, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return in, nil
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return in, fmt.Errorf("hook input is not JSON: %w", err)
	}
	return in, nil
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// isEditTool reports whether a tool name is one that modifies files. Names are matched
// loosely because every harness spells them differently (Edit, edit, apply_patch,
// mcp__x__write_file); a path is required as well, so a name like TodoWrite with no file
// behind it never reaches the control plane.
func isEditTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	switch n {
	case "edit", "write", "multiedit", "multi_edit", "notebookedit", "notebook_edit",
		"patch", "apply_patch", "str_replace_editor", "str_replace_based_edit_tool",
		"create_file", "insert", "text_editor":
		return true
	case "read", "bash", "grep", "glob", "ls", "list", "search", "webfetch", "websearch", "task":
		return false
	}
	if i := strings.LastIndex(n, "__"); i >= 0 {
		n = n[i+2:]
	}
	return strings.Contains(n, "edit") || strings.Contains(n, "write") || strings.Contains(n, "patch")
}

// repoRelative resolves a tool's path against the repository the hook runs in. A path
// outside the repository is not Conductor's territory and is reported as such.
func repoRelative(cwd, path string) (rel string, ok bool) {
	if path == "" {
		return "", false
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, path)
	}
	abs = filepath.Clean(abs)
	root, err := config.FindRoot(cwd)
	if err != nil {
		if root, err = config.FindRoot(filepath.Dir(abs)); err != nil {
			return "", false
		}
	}
	rel, err = filepath.Rel(root, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// preToolVerdict is what a pre-tool hook decides. Exactly one of Block or Warning is set,
// or neither for a clean allow.
type preToolVerdict struct {
	Block   bool   `json:"block"`
	Message string `json:"message,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// judgePreTool turns the control plane's answer into the hook's verdict.
//
// Holdings by the caller's own tasks are not conflicts: a person editing a file their own
// claim reserved is the system working, not a collision. Beyond that the decision itself is
// the verdict: the server has already applied the project's enforcement level (DESIGN.md
// §11.5), so a cooperative project warns where a strict one blocks, and the hook never
// re-derives that from the raw per-conflict outcomes — the level in project.yaml stays the
// single source of truth.
func judgePreTool(d coord.IntentDecision, self string) preToolVerdict {
	var others []db.ScopeConflict
	for _, c := range d.Conflicts {
		if self != "" && c.HolderOwner == self {
			continue
		}
		others = append(others, c)
	}
	if len(others) == 0 {
		return preToolVerdict{}
	}
	if d.Outcome.Blocks() {
		message := fmt.Sprintf(
			"Conductor: %s holds %s for %s (%s). Wait for it, split your scope, or join their task "+
				"with coord_start_work(attach_to: %q). Run conductor_check_conflicts to see the current holders.",
			others[0].HolderOwner, others[0].ResourceKey, others[0].HolderTaskRef, others[0].HolderMode, others[0].HolderTaskRef)
		if d.Outcome != domain.OutcomeBlockConflict && d.Advice != "" {
			// A block for another reason (an exact duplicate, say): the server's advice
			// explains it better than a file-conflict message would.
			message = d.Advice
		}
		return preToolVerdict{Block: true, Message: message}
	}
	warning := fmt.Sprintf(
		"Conductor: %s overlaps %s (%s by %s). Proceed, but expect to coordinate on merge.",
		others[0].ResourceKey, others[0].HolderTaskRef, others[0].HolderMode, others[0].HolderOwner)
	if d.Advice != "" {
		// The server's advice carries the level's instruction: cooperative decisions say
		// how to request the territory, advisory ones say to coordinate on merge.
		warning = "Conductor: " + d.Advice
	}
	return preToolVerdict{Warning: warning}
}

func hookPreTool(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook pre-tool", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	toolFlag := fs.String("tool", "", "tool name (default: from the hook payload on stdin)")
	pathFlag := fs.String("path", "", "file the tool is about to modify (default: from the hook payload)")
	project := fs.String("project", "", "project id or slug")
	strict := fs.Bool("strict", false, "block when Conductor cannot be reached, instead of failing open")
	requireClaim := fs.Bool("require-claim", false, "block edits from a session that holds no task")
	asJSON := fs.Bool("json", false, "print the decision and verdict to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}

	in, err := readHookInput(os.Stdin, stdinIsTerminal())
	if err != nil {
		return failOpen(*strict, err.Error())
	}
	tool := firstNonEmptyString(*toolFlag, in.ToolName)
	path := firstNonEmptyString(*pathFlag, in.path())
	if !isEditTool(tool) || path == "" {
		return nil
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	rel, ok := repoRelative(cwd, path)
	if !ok {
		return nil // not inside a repository Conductor knows about
	}

	creds := client.LoadCredentials()
	if creds.Token == "" {
		return failOpen(*strict, "not logged in; edits are not being checked (run `conductor login`)")
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return failOpen(*strict, err.Error())
	}
	api := client.New(creds.Endpoint, creds.Token)
	api.HTTP.Timeout = hookTimeout
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()

	excludeTask := os.Getenv("CONDUCTOR_TASK_ID")
	hasClaim := excludeTask != ""
	if sessionID := os.Getenv("CONDUCTOR_SESSION_ID"); excludeTask == "" && sessionID != "" {
		if active := activeTaskFor(ctx, api, ref, sessionID); active.ID != "" {
			excludeTask, hasClaim = active.ID, true
		}
	}
	if *requireClaim && !hasClaim {
		return blockEdit("Conductor: this session holds no task. Claim one first (coord_start_work, or " +
			"`conductor task claim --next`), then edit.")
	}

	var decision coord.IntentDecision
	err = api.Post(ctx, "/v1/projects/"+ref+"/intents/check", map[string]any{
		"summary":      "edit " + rel,
		"scopes":       []domain.ScopeRequest{{Resource: "path:" + rel, Mode: domain.ModeWriteExclusive}},
		"exclude_task": excludeTask,
	}, &decision)
	if err != nil {
		return failOpen(*strict, "could not check "+rel+" with Conductor: "+err.Error())
	}

	verdict := judgePreTool(decision, selfHandle(ctx, api, creds))
	if *asJSON {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"decision": decision, "verdict": verdict})
	}
	switch {
	case verdict.Block:
		return blockEdit(verdict.Message)
	case verdict.Warning != "":
		// Exit 0 with a JSON body: the edit proceeds and the model sees the warning.
		out := map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "allow",
			"permissionDecisionReason": verdict.Warning,
			"additionalContext":        verdict.Warning,
		}}
		body, _ := json.Marshal(out)
		fmt.Println(string(body))
	}
	return nil
}

// blockEdit is the one hard answer a hook gives: exit 2, reason on stderr.
func blockEdit(message string) error {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
	return nil
}

// failOpen lets the edit through when Conductor could not answer, saying so once on stderr.
// With --strict the same condition blocks.
func failOpen(strict bool, reason string) error {
	if strict {
		return blockEdit("Conductor (strict): " + reason)
	}
	fmt.Fprintln(os.Stderr, "conductor hook: "+reason)
	return nil
}

// selfHandle is the caller's handle, from the login file or one cached whoami call.
func selfHandle(ctx context.Context, api *client.Client, creds client.Credentials) string {
	if creds.Handle != "" {
		return creds.Handle
	}
	var cached struct {
		Handle    string    `json:"handle"`
		FetchedAt time.Time `json:"fetched_at"`
	}
	if readHookCache("whoami", &cached) && time.Since(cached.FetchedAt) < time.Hour {
		return cached.Handle
	}
	var who struct {
		Principal domain.Principal `json:"principal"`
	}
	if err := api.Get(ctx, "/v1/whoami", &who); err != nil {
		return ""
	}
	cached.Handle, cached.FetchedAt = who.Principal.Handle, time.Now()
	writeHookCache("whoami", cached)
	return cached.Handle
}

// activeTask is what a session is working on, as far as a hook needs to know.
type activeTask struct {
	ID        string    `json:"task_id"`
	Ref       string    `json:"task_ref"`
	FetchedAt time.Time `json:"fetched_at"`
}

// activeTaskFor finds the task a session holds, caching the answer briefly so a burst of
// edits costs one lookup rather than one per file.
func activeTaskFor(ctx context.Context, api *client.Client, project, sessionID string) activeTask {
	var cached activeTask
	if readHookCache("session-"+sessionID, &cached) && time.Since(cached.FetchedAt) < time.Minute {
		return cached
	}
	var out struct {
		Sessions []privacy.SessionView `json:"sessions"`
	}
	if err := api.Get(ctx, "/v1/projects/"+project+"/sessions", &out); err != nil {
		return cached
	}
	found := activeTask{FetchedAt: time.Now()}
	for _, s := range out.Sessions {
		if s.ID == sessionID && s.ActiveTaskRef != "" {
			found.Ref = s.ActiveTaskRef
			var view privacy.TaskView
			if err := api.Get(ctx, "/v1/tasks/"+s.ActiveTaskRef+client.Query("project", project), &view); err == nil {
				found.ID = view.ID
			}
			break
		}
	}
	writeHookCache("session-"+sessionID, found)
	return found
}

// hookCacheDir is ~/.conductor/hookcache (or under CONDUCTOR_STATE_DIR), owner-only.
func hookCacheDir() (string, error) {
	if v := os.Getenv("CONDUCTOR_STATE_DIR"); v != "" {
		return filepath.Join(v, "hookcache"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".conductor", "hookcache"), nil
}

func readHookCache(name string, dst any) bool {
	dir, err := hookCacheDir()
	if err != nil {
		return false
	}
	body, err := os.ReadFile(filepath.Join(dir, safeCacheName(name)+".json"))
	if err != nil {
		return false
	}
	return json.Unmarshal(body, dst) == nil
}

func writeHookCache(name string, v any) {
	dir, err := hookCacheDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, safeCacheName(name)+".json"), body, 0o600)
}

func safeCacheName(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, name)
}

// ---------------------------------------------------------------------------
// session-start / session-end
// ---------------------------------------------------------------------------

// hookSessionStart prints what a fresh session should know. Claude Code adds a SessionStart
// hook's stdout to the model's context, so this is short: a connection line, the active task
// card when there is one, offers waiting for this session, and the one rule that matters.
func hookSessionStart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook session-start", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "project id or slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	creds := client.LoadCredentials()
	if creds.Token == "" {
		return nil
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		return nil
	}
	api := client.New(creds.Endpoint, creds.Token)
	api.HTTP.Timeout = hookTimeout
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()

	var b strings.Builder
	fmt.Fprintf(&b, "Conductor: coordinating on project %s at %s", ref, creds.Endpoint)
	if creds.Handle != "" {
		fmt.Fprintf(&b, " as %s", creds.Handle)
	}
	b.WriteString(".\n")

	sessionID := os.Getenv("CONDUCTOR_SESSION_ID")
	if sessionID == "" {
		b.WriteString("This session is not registered: launch through `conductor wrap <tool>` to be visible " +
			"to teammates and to be offered work.\n")
	} else {
		if active := activeTaskFor(ctx, api, ref, sessionID); active.Ref != "" {
			if card, err := api.Raw(ctx, "/v1/tasks/"+active.Ref+"/card"+client.Query("project", ref)); err == nil {
				fmt.Fprintf(&b, "\nActive task %s:\n%s\n", active.Ref, trimLines(string(card), 60))
			}
		}
		var offers struct {
			Assignments []domain.Assignment `json:"assignments"`
		}
		if err := api.Get(ctx, "/v1/sessions/"+sessionID+"/assignments", &offers); err == nil && len(offers.Assignments) > 0 {
			b.WriteString("\nWork offered to this session (take it with coord_start_work, attach_to the task):\n")
			for _, a := range offers.Assignments {
				fmt.Fprintf(&b, "- %s — requires %s\n", a.TaskRef, a.Requirement.Describe())
			}
		}
	}
	b.WriteString("\nBefore editing any file, call conductor_check_conflicts (or run `conductor check --scope path:<file>`); " +
		"claim work with coord_start_work. Prompts and output stay local; only task titles, scopes, and evidence are shared.\n")
	fmt.Print(b.String())
	return nil
}

// hookSessionEnd closes a bare session's presence record. A session launched through
// `conductor wrap` is closed by the wrapper itself and is left alone here.
func hookSessionEnd(ctx context.Context, args []string) error {
	sessionID := os.Getenv("CONDUCTOR_SESSION_ID")
	if sessionID == "" {
		return nil
	}
	if records, err := localstate.List(); err == nil {
		for _, r := range records {
			if r.SessionID == sessionID && r.Wrapped {
				return nil
			}
		}
	}
	creds := client.LoadCredentials()
	if creds.Token == "" {
		return nil
	}
	api := client.New(creds.Endpoint, creds.Token)
	api.HTTP.Timeout = hookTimeout
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	_ = api.Post(ctx, "/v1/sessions/"+sessionID+"/close", nil, nil)
	return nil
}

func trimLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + "\n…"
}
