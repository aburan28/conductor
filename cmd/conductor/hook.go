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
	"strconv"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/checkpoint"
	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/config"
	"github.com/aburan28/conductor/internal/coord"
	"github.com/aburan28/conductor/internal/db"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/localstate"
	"github.com/aburan28/conductor/internal/privacy"
	"github.com/aburan28/conductor/internal/resource"
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
		return errors.New("usage: conductor hook <pre-tool|session-start|session-end|checkpoint|memory-observe|memory-context>")
	}
	switch args[0] {
	case "pre-tool":
		return hookPreTool(ctx, args[1:])
	case "session-start":
		return hookSessionStart(ctx, args[1:])
	case "session-end":
		return hookSessionEnd(ctx, args[1:])
	case "checkpoint":
		return hookCheckpoint(ctx, args[1:])
	case "memory-observe":
		return hookMemoryObserve(ctx, args[1:])
	case "memory-context":
		return hookMemoryContext(ctx, args[1:])
	default:
		return fmt.Errorf("unknown hook event %q", args[0])
	}
}

// hookInput is the subset of a tool's hook payload this command reads. It is deliberately
// tiny: there is no field for file content or tool arguments beyond a path, so they cannot be
// decoded even by accident. The one exception is a patch (Codex's apply_patch), whose file
// paths exist only inside the patch text: its header lines are read and the rest discarded.
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
	// patchPaths are the files a patch touches, filled by readHookInput for patch tools.
	patchPaths []string
}

// paths are the files the tool is about to modify: the one path an edit names, or every file
// a patch adds, updates, deletes, or moves to.
func (h hookInput) paths() []string {
	if p := firstNonEmptyString(h.ToolInput.FilePath, h.ToolInput.FilePathCamel, h.ToolInput.NotebookPath, h.ToolInput.Path); p != "" {
		return []string{p}
	}
	return h.patchPaths
}

// isPatchTool reports whether a tool carries its edits as a patch rather than a path.
func isPatchTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "apply_patch", "patch":
		return true
	}
	return false
}

// patchHeaders are the apply_patch lines that name a file; every other line is content.
var patchHeaders = []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "}

// patchFilePaths extracts the file paths from an apply_patch body, in order, without
// duplicates. Only header lines are examined.
func patchFilePaths(patch string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, h := range patchHeaders {
			if p, ok := strings.CutPrefix(line, h); ok {
				if p = strings.TrimSpace(p); p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
				break
			}
		}
	}
	return out
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
	if isPatchTool(in.ToolName) {
		// The patch text is decoded only here, only for a patch tool, and only its file
		// paths outlive this function.
		var raw struct {
			ToolInput json.RawMessage `json:"tool_input"`
		}
		if json.Unmarshal(body, &raw) == nil {
			var patch struct {
				Command string `json:"command"`
				Input   string `json:"input"`
				Patch   string `json:"patch"`
			}
			var text string
			if json.Unmarshal(raw.ToolInput, &patch) == nil {
				text = firstNonEmptyString(patch.Command, patch.Input, patch.Patch)
			} else {
				_ = json.Unmarshal(raw.ToolInput, &text) // some harnesses pass the patch bare
			}
			in.patchPaths = patchFilePaths(text)
		}
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
// claim reserved is the system working, not a collision. Everything else follows the
// decision — a blocking outcome blocks, an advisory overlap warns.
func judgePreTool(d coord.IntentDecision, self string) preToolVerdict {
	var others []db.ScopeConflict
	for _, c := range d.Conflicts {
		if self != "" && c.HolderOwner == self {
			continue
		}
		others = append(others, c)
	}
	for _, c := range others {
		if !c.Outcome.Blocks() {
			continue
		}
		if c.PendingMerge() {
			// The holder is not editing: their finished change to this file is waiting to
			// merge. Editing now would build on contents that are about to change under you.
			pr := ""
			if c.HolderPullRequest != "" {
				pr = " (" + c.HolderPullRequest + ")"
			}
			return preToolVerdict{Block: true, Message: fmt.Sprintf(
				"Conductor: %s's %s changed %s and is waiting to merge%s. The file stays reserved until "+
					"that merges or %s is marked done. Wait for the merge, or build on their branch.",
				c.HolderOwner, c.HolderTaskRef, c.ResourceKey, pr, c.HolderTaskRef)}
		}
		return preToolVerdict{Block: true, Message: fmt.Sprintf(
			"Conductor: %s holds %s for %s (%s). Wait for it, split your scope, or join their task "+
				"with coord_start_work(attach_to: %q). Run conductor_check_conflicts to see the current holders.",
			c.HolderOwner, c.ResourceKey, c.HolderTaskRef, c.HolderMode, c.HolderTaskRef)}
	}
	if len(others) > 0 {
		c := others[0]
		return preToolVerdict{Warning: fmt.Sprintf(
			"Conductor: %s overlaps %s (%s by %s). Proceed, but expect to coordinate on merge.",
			c.ResourceKey, c.HolderTaskRef, c.HolderMode, c.HolderOwner)}
	}
	return preToolVerdict{}
}

func hookPreTool(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook pre-tool", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	toolFlag := fs.String("tool", "", "tool name (default: from the hook payload on stdin)")
	pathFlag := fs.String("path", "", "file the tool is about to modify (default: from the hook payload)")
	project := fs.String("project", "", "project id or slug")
	strict := fs.Bool("strict", false, "block when Conductor cannot be reached, instead of failing open")
	requireClaim := fs.Bool("require-claim", false, "block edits from a session that holds no task")
	autoReserve := fs.Bool("auto-reserve", true, "reserve a file under the session's claim the first time it is edited outside the claimed scope (=false only reports it)")
	asJSON := fs.Bool("json", false, "print the decision and verdict to stderr")
	harness := fs.String("harness", "claude", "harness that fired the hook, which decides the output shape (claude, codex)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	in, err := readHookInput(os.Stdin, stdinIsTerminal())
	if err != nil {
		return failOpen(*strict, err.Error())
	}
	tool := firstNonEmptyString(*toolFlag, in.ToolName)
	paths := in.paths()
	if *pathFlag != "" {
		paths = []string{*pathFlag}
	}
	if !isEditTool(tool) || len(paths) == 0 {
		return nil
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	var rels []string
	for _, p := range paths {
		if rel, ok := repoRelative(cwd, p); ok {
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return nil // nothing inside a repository Conductor knows about
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

	sessionID := os.Getenv("CONDUCTOR_SESSION_ID")
	claim := claimFromEnv(ctx, api, ref, sessionID)
	excludeTask := claim.ID
	hasClaim := excludeTask != ""
	if *requireClaim && !hasClaim {
		return blockEdit("Conductor: this session holds no task. Claim one first (coord_start_work, or " +
			"`conductor task claim --next`), then edit.")
	}

	// A patch can touch several files: every one is checked before any is reserved, and one
	// blocked file blocks the whole patch, since the tool applies it all or nothing.
	self := selfHandle(ctx, api, creds)
	notes := []string{}
	for _, rel := range rels {
		var decision coord.IntentDecision
		err = api.Post(ctx, "/v1/projects/"+ref+"/intents/check", map[string]any{
			"summary":      "edit " + rel,
			"scopes":       []domain.ScopeRequest{{Resource: "path:" + rel, Mode: domain.ModeWriteExclusive}},
			"exclude_task": excludeTask,
		}, &decision)
		if err != nil {
			return failOpen(*strict, "could not check "+rel+" with Conductor: "+err.Error())
		}
		verdict := judgePreTool(decision, self)
		if *asJSON {
			_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"path": rel, "decision": decision, "verdict": verdict})
		}
		if verdict.Block {
			return blockEdit(verdict.Message)
		}
		if verdict.Warning != "" {
			notes = append(notes, verdict.Warning)
		}
	}

	// The edit goes ahead. What remains is making it visible: an edit outside the claim's
	// scope is scope expansion, and an edit with no claim at all is invisible to everyone.
	if hasClaim {
		for _, rel := range rels {
			if note := expandOwnScope(ctx, api, claim, rel, *autoReserve); note != "" {
				notes = append(notes, note)
			}
		}
	} else if note := unclaimedEditNote(sessionID, cwd); note != "" {
		notes = append(notes, note)
	}
	if len(notes) > 0 {
		// Exit 0 with a JSON body: the edit proceeds and the model sees the notes.
		fmt.Println(preToolNotes(*harness, strings.Join(notes, " ")))
	}
	return nil
}

// preToolNotes is the exit-0 body that lets an edit through with notes the model reads.
// Claude Code takes an explicit allow with a reason; Codex accepts only additionalContext on
// an allowed call and marks a hook that sends permissionDecision without updatedInput as
// failed, so it gets the context alone.
func preToolNotes(harness, text string) string {
	specific := map[string]any{"hookEventName": "PreToolUse", "additionalContext": text}
	if checkpoint.NormalizeHarness(harness) != "codex" {
		specific["permissionDecision"] = "allow"
		specific["permissionDecisionReason"] = text
	}
	body, _ := json.Marshal(map[string]any{"hookSpecificOutput": specific})
	return string(body)
}

// claimFromEnv finds the claim this hook's session is working under: a runner-launched
// attempt names its task and fence in the environment; a wrapped session is looked up by its
// session id.
func claimFromEnv(ctx context.Context, api *client.Client, project, sessionID string) activeTask {
	if taskID := os.Getenv("CONDUCTOR_TASK_ID"); taskID != "" {
		claim := activeTask{ID: taskID, Ref: os.Getenv("CONDUCTOR_TASK_REF"), FetchedAt: time.Now()}
		claim.Fence = domain.Fence{TaskID: taskID, AttemptID: os.Getenv("CONDUCTOR_ATTEMPT_ID"),
			LeaseID: os.Getenv("CONDUCTOR_LEASE_ID")}
		if n, err := strconv.ParseInt(os.Getenv("CONDUCTOR_FENCING_EPOCH"), 10, 64); err == nil {
			claim.Fence.FencingEpoch = n
		}
		var view privacy.TaskView
		if err := api.Get(ctx, "/v1/tasks/"+taskID, &view); err == nil {
			claim.Ref, claim.Scopes = view.Ref, view.Scopes
		}
		return claim
	}
	if sessionID == "" {
		return activeTask{}
	}
	claim := activeTaskFor(ctx, api, project, sessionID)
	claim.SessionID = sessionID
	return claim
}

// coveredBy reports whether a repository path falls inside any of a claim's scopes.
func coveredBy(scopes []string, rel string) bool {
	want, err := resource.Parse("path:" + rel)
	if err != nil {
		return true // not a path Conductor can reason about; do not nag about it
	}
	for _, sc := range scopes {
		held, err := resource.Parse(sc)
		if err != nil {
			continue
		}
		if held.Type == domain.ResourceRepo || resource.Overlaps(want, held) {
			return true
		}
	}
	return false
}

// expandOwnScope handles an edit outside the caller's own claimed scope.
//
// Scope drift is normal — the work turns out to need an adjacent file — but unreported it is
// invisible: the claim says one thing, the diff says another, and a teammate who checks the
// file is told it is free. With auto-reserve (the default, and what `conductor integrate`
// configures) the file is reserved under the claim on its first edit, as an `observed`
// reservation, and the model is told so. Without it the model is told to report the expansion
// itself. Only the path is sent, never the edit.
func expandOwnScope(ctx context.Context, api *client.Client, claim activeTask, rel string, autoReserve bool) string {
	if coveredBy(claim.Scopes, rel) {
		return ""
	}
	ref := firstNonEmptyString(claim.Ref, "your task")
	if !autoReserve {
		return fmt.Sprintf("Conductor: %s is outside %s's claimed scope. Report the scope expansion "+
			"(coord_expand_scope, or `conductor scope add %s path:%s`) so teammates can see it.", rel, ref, ref, rel)
	}
	scopes := []domain.ScopeRequest{{Resource: "path:" + rel, Mode: domain.ModeWriteExclusive}}
	var result coord.ExpandScopeResult
	var err error
	switch {
	case claim.SessionID != "":
		var out coord.SessionScopeResult
		err = api.Post(ctx, "/v1/sessions/"+claim.SessionID+"/scopes",
			map[string]any{"scopes": scopes, "source": domain.SourceObserved}, &out)
		result = out.ExpandScopeResult
		if err == nil && out.NoClaim {
			return ""
		}
	case claim.Fence.LeaseID != "":
		err = api.Post(ctx, "/v1/tasks/"+claim.ID+"/scopes", map[string]any{
			"attempt_id": claim.Fence.AttemptID, "lease_id": claim.Fence.LeaseID,
			"fencing_epoch": claim.Fence.FencingEpoch,
			"scopes":        scopes, "source": domain.SourceObserved,
		}, &result)
	default:
		return ""
	}
	if err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Blocked() {
			return fmt.Sprintf("Conductor: %s is outside %s's claimed scope and could not be reserved: %s",
				rel, ref, result.Advice)
		}
		return fmt.Sprintf("Conductor: %s is outside %s's claimed scope (reserving it failed: %v). "+
			"Report the scope expansion with coord_expand_scope.", rel, ref, err)
	}
	// Remember it, so the next edit of the same file costs nothing.
	claim.Scopes = append(claim.Scopes, "path:"+rel)
	if claim.SessionID != "" {
		writeHookCache("session-"+claim.SessionID, claim)
	}
	return fmt.Sprintf("Conductor: %s was outside %s's claimed scope, so it is now reserved for %s "+
		"(scope expansion recorded).", rel, ref, ref)
}

// unclaimedNoticeEvery bounds how often a session with no claim is reminded of it.
const unclaimedNoticeEvery = 10 * time.Minute

// unclaimedEditNote reminds a session that holds no task that its edits reserve nothing. It
// does not block (that is --require-claim) and it does not repeat on every edit.
func unclaimedEditNote(sessionID, cwd string) string {
	key := "unclaimed-" + firstNonEmptyString(sessionID, cwd)
	var last struct {
		At time.Time `json:"at"`
	}
	if readHookCache(key, &last) && time.Since(last.At) < unclaimedNoticeEvery {
		return ""
	}
	last.At = time.Now()
	writeHookCache(key, last)
	return "Conductor: this session holds no task, so its edits reserve nothing and teammates cannot " +
		"see them. Claim the work first (coord_start_work, or `conductor task claim`) to protect it."
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
	Scopes    []string  `json:"scopes,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	// SessionID and Fence say how to act on the claim: through the wrapped session, or with
	// a runner attempt's fence. Neither is cached.
	SessionID string       `json:"-"`
	Fence     domain.Fence `json:"-"`
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
				found.ID, found.Scopes = view.ID, view.Scopes
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
	noStdin := fs.Bool("no-stdin", false, "use the process working directory when the harness has no session payload")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var in hookInput
	if !*noStdin {
		in, _ = readHookInput(os.Stdin, stdinIsTerminal())
	}
	memoryContext := memoryStartupContext(ctx, in.Cwd, firstNonEmptyString(in.SessionID, os.Getenv("CONDUCTOR_SESSION_ID")))
	creds := client.LoadCredentials()
	if creds.Token == "" {
		fmt.Print(memoryContext)
		return nil
	}
	ref, err := projectRef(*project, creds)
	if err != nil {
		fmt.Print(memoryContext)
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
	if memoryContext != "" {
		b.WriteString("\n")
		b.WriteString(memoryContext)
	}
	fmt.Print(b.String())
	return nil
}

// hookSessionEnd closes a bare session's presence record. A session launched through
// `conductor wrap` is closed by the wrapper itself and is left alone here.
func hookSessionEnd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook session-end", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	harness := fs.String("harness", "", "harness that fired the hook (default: claude, or CONDUCTOR_HARNESS)")
	if fs.Parse(args) == nil && *harness != "" {
		os.Setenv("CONDUCTOR_HARNESS", *harness)
	}
	// The transcript is complete now: the last checkpoint of this session, forced past the
	// rate limit but still skipped when nothing changed.
	hookCheckpointFromStdin(ctx, true)
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

// hookCheckpoint is the Stop / PreCompact hook: checkpoint this session so it can continue
// elsewhere. It is rate-limited and skips unchanged sessions inside the checkpoint package,
// and like every hook it fails open — a checkpoint that could not be taken is one line on
// stderr, never a blocked turn. Only the session id, cwd, and event name are read from the
// hook payload; the transcript is located from those, not from a path the payload names.
func hookCheckpoint(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook checkpoint", flag.ContinueOnError)
	harness := fs.String("harness", "", "harness that fired the hook (default: claude, or CONDUCTOR_HARNESS)")
	force := fs.Bool("force", false, "ignore the rate limit (still skips an unchanged session)")
	if err := fs.Parse(args); err != nil {
		return nil
	}
	if *harness != "" {
		os.Setenv("CONDUCTOR_HARNESS", *harness)
	}
	hookCheckpointFromStdin(ctx, *force)
	return nil
}

func hookCheckpointFromStdin(ctx context.Context, force bool) {
	if checkpoint.Disabled(os.Getenv) {
		return
	}
	in, err := readHookInput(os.Stdin, stdinIsTerminal())
	if err != nil {
		return
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	harness := checkpoint.NormalizeHarness(firstNonEmptyString(os.Getenv("CONDUCTOR_HARNESS"), "claude"))
	reason := checkpoint.ReasonHook
	if in.HookEventName != "" {
		reason = checkpoint.ReasonHook + ":" + in.HookEventName
		if in.HookEventName == "PreCompact" || in.HookEventName == "SessionEnd" {
			force = true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	_, err = checkpoint.Capture(ctx, checkpoint.Request{
		Harness: harness, Cwd: cwd, SessionID: in.SessionID, Since: time.Now().Add(-30 * 24 * time.Hour),
		Reason: reason, Force: force, Conductor: conductorRefFromEnv(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "conductor: checkpoint not taken: %v\n", err)
	}
}
