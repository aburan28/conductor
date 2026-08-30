package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Adapter turns one line of a harness's native event stream into a sanitized Event.
//
// This function signature is the privacy boundary. It takes bytes and returns an Event that
// has no text field, so an adapter physically cannot forward assistant output even by
// accident. Everything it does not explicitly extract is discarded when the line goes out of
// scope.
type Adapter func(line []byte) (Event, bool)

// ProcessDriver runs a harness as a subprocess and parses its event stream.
//
// This is the generic driver of DESIGN.md §16.5 and simultaneously the concrete one: Claude,
// Codex, and OpenCode differ only in their argument templates and stream adapters, so they
// are configurations of this type rather than three parallel implementations.
type ProcessDriver struct {
	Kind    string
	Command string
	// Args builds the argument vector for a run. Keeping it a function lets a harness map
	// model and effort onto whatever flags its current version uses.
	Args func(spec RunSpec) []string
	// VersionArgs probes availability. Empty means "assume available if the binary exists".
	VersionArgs []string
	Adapt       Adapter
	Caps        Capabilities
	// StdinInstruction sends the instruction on stdin instead of as an argument, which
	// avoids a multi-kilobyte task card hitting the OS argument-length limit.
	StdinInstruction bool
}

func (d *ProcessDriver) Name() string { return d.Kind }

func (d *ProcessDriver) Capabilities(ctx context.Context) Capabilities {
	caps := d.Caps
	caps.Kind = d.Kind

	path, err := exec.LookPath(d.Command)
	if err != nil {
		caps.Available = false
		caps.Detail = d.Command + " not found on PATH"
		return caps
	}
	caps.Available = true

	if len(d.VersionArgs) > 0 {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(probeCtx, path, d.VersionArgs...).Output()
		if err == nil {
			caps.Version = strings.TrimSpace(firstLine(string(out)))
		}
	}
	return caps
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func (d *ProcessDriver) Start(ctx context.Context, spec RunSpec) (Handle, error) {
	if spec.Cwd == "" {
		return nil, fmt.Errorf("%w: run spec has no working directory", domain.ErrInvalidArgument)
	}
	if _, err := exec.LookPath(d.Command); err != nil {
		return nil, fmt.Errorf("%w: harness %q not installed", domain.ErrCapacity, d.Kind)
	}

	runCtx, cancel := context.WithCancel(ctx)
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	}

	args := d.Args(spec)
	args = append(args, spec.ExtraArgs...)

	cmd := exec.CommandContext(runCtx, d.Command, args...)
	cmd.Dir = spec.Cwd
	cmd.Env = buildEnv(spec)
	// Own process group, so cancellation kills the harness and everything it spawned rather
	// than orphaning a subprocess that keeps editing the worktree (DESIGN.md §25.3).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if d.StdinInstruction {
		cmd.Stdin = strings.NewReader(spec.Instruction)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	// stderr is drained into the attempt log, never into events: the log lives inside the
	// attempt's worktree and is removed with it, while events cross the privacy boundary
	// to the control plane (DESIGN.md §26.3).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	logFile, err := openAttemptLog(spec.Cwd)
	if err != nil {
		cancel()
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		cancel()
		return nil, fmt.Errorf("start %s: %w", d.Command, err)
	}

	h := &processHandle{
		cmd:     cmd,
		cancel:  cancel,
		events:  make(chan Event, 128),
		log:     logFile,
		started: time.Now(),
	}

	go func() {
		_, _ = io.Copy(logFile, stderr)
	}()
	go h.pump(stdout, d.Adapt)
	return h, nil
}

// openAttemptLog creates <worktree>/.conductor/attempt.log, the combined harness output
// the dashboard tails while the attempt runs. Append-only, so a replacement attempt that
// adopts a recovered worktree keeps the earlier output.
func openAttemptLog(worktree string) (*os.File, error) {
	dir := filepath.Join(worktree, ".conductor")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, "attempt.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// buildEnv assembles a minimal environment for the harness process.
//
// The parent environment is passed through because a coding agent needs its own credentials
// and PATH, but Conductor's own database URL and service token are stripped: a worker has no
// business holding the control plane's credentials (DESIGN.md §25.2).
func buildEnv(spec RunSpec) []string {
	blocked := map[string]bool{
		"DATABASE_URL":      true,
		"CONDUCTOR_TOKEN":   true,
		"CONDUCTOR_DB":      true,
		"POSTGRES_PASSWORD": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if blocked[name] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"CONDUCTOR_TASK_REF="+spec.TaskRef,
		"CONDUCTOR_TASK_ID="+spec.Fence.TaskID,
		"CONDUCTOR_ATTEMPT_ID="+spec.Fence.AttemptID,
		"CONDUCTOR_LEASE_ID="+spec.Fence.LeaseID,
		fmt.Sprintf("CONDUCTOR_FENCING_EPOCH=%d", spec.Fence.FencingEpoch),
		"CONDUCTOR_BRANCH="+spec.Branch,
		"CONDUCTOR_WORKFLOW_SHA="+spec.WorkflowSHA,
	)
	if spec.TaskCardPath != "" {
		env = append(env, "CONDUCTOR_TASK_CARD="+spec.TaskCardPath)
	}
	return append(env, spec.Env...)
}

type processHandle struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	events  chan Event
	log     *os.File
	started time.Time

	mu      sync.Mutex
	result  Result
	done    bool
	waitCh  chan struct{}
	waitErr error
	once    sync.Once
}

func (h *processHandle) Events() <-chan Event { return h.events }

// pump reads the harness's stream, mirroring every line into the attempt log and
// accumulating measurements from the adapted events.
func (h *processHandle) pump(stdout io.Reader, adapt Adapter) {
	defer close(h.events)
	defer h.log.Close()

	scanner := bufio.NewScanner(stdout)
	// Harness JSON events can be large (a single message with many content blocks). The
	// buffer is generous so a long line is parsed rather than silently truncated into
	// garbage — but the parsed line still yields only metadata.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	h.emit(Event{Kind: EventStarted, At: time.Now()})

	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = fmt.Fprintf(h.log, "%s\n", line)
		if len(line) == 0 || adapt == nil {
			continue
		}
		ev, ok := adapt(line)
		if !ok {
			continue
		}
		if ev.At.IsZero() {
			ev.At = time.Now()
		}

		h.mu.Lock()
		if ev.TokensIn > 0 {
			h.result.TokensIn += ev.TokensIn
		}
		if ev.TokensOut > 0 {
			h.result.TokensOut += ev.TokensOut
		}
		// Cost accumulates; adapters emit the incremental cost of one turn (OpenCode's
		// per-step `part.cost`) or a single session total on the final event (Claude's
		// `result.total_cost_usd`). The sum is the attempt's spend either way — a max
		// would silently report only the most expensive turn while tokens still summed
		// every turn, which is how cost and tokens ended up telling different stories.
		h.result.CostUSD += ev.CostUSD
		if ev.Turns > h.result.Turns {
			h.result.Turns = ev.Turns
		}
		if ev.Kind == EventTurn {
			h.result.Turns++
			ev.Turns = h.result.Turns
		}
		h.mu.Unlock()

		h.emit(ev)
	}
}

func (h *processHandle) emit(ev Event) {
	select {
	case h.events <- ev:
	default:
		// A slow consumer must not stall the harness. Progress events are advisory; the
		// authoritative totals are accumulated in `result` regardless.
	}
}

func (h *processHandle) Wait(ctx context.Context) (Result, error) {
	h.once.Do(func() {
		h.waitCh = make(chan struct{})
		go func() {
			defer close(h.waitCh)
			err := h.cmd.Wait()

			h.mu.Lock()
			defer h.mu.Unlock()
			h.done = true
			h.result.Duration = time.Since(h.started)
			h.result.ExitCode = h.cmd.ProcessState.ExitCode()
			h.result.Succeeded = err == nil && h.result.ExitCode == 0

			switch {
			case err == nil:
			case errors.Is(err, context.DeadlineExceeded):
				h.result.FailureClass = "timeout"
			default:
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					h.result.FailureClass = "nonzero_exit"
				} else {
					h.result.FailureClass = "process_error"
					h.waitErr = err
				}
			}
		}()
	})

	select {
	case <-h.waitCh:
	case <-ctx.Done():
		return h.snapshot(), ctx.Err()
	}

	h.cancel()
	return h.snapshot(), h.waitErr
}

func (h *processHandle) snapshot() Result {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result
}

// Cancel interrupts the harness, giving it a chance to flush before it is killed
// (DESIGN.md §27.3: soft interrupt first, then kill the process group after grace).
func (h *processHandle) Cancel(ctx context.Context) error {
	if h.cmd.Process == nil {
		return nil
	}
	pgid := -h.cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGINT)

	grace := 5 * time.Second
	select {
	case <-h.waitChOrClosed():
		return nil
	case <-time.After(grace):
	case <-ctx.Done():
	}
	_ = syscall.Kill(pgid, syscall.SIGKILL)
	h.cancel()
	return nil
}

func (h *processHandle) waitChOrClosed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.waitCh != nil {
		return h.waitCh
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

// ---------------------------------------------------------------------------
// Stream adapters
// ---------------------------------------------------------------------------

// claudeAdapter parses Claude Code's `--output-format stream-json` events.
//
// It reads `usage`, `total_cost_usd`, `num_turns`, and the *names* of tool_use blocks. The
// `text` and `input` fields of those blocks are never read, so assistant output and tool
// arguments leave no trace.
func claudeAdapter(line []byte) (Event, bool) {
	var msg struct {
		Type         string        `json:"type"`
		Subtype      string        `json:"subtype"`
		IsError      bool          `json:"is_error"`
		NumTurns     int           `json:"num_turns"`
		TotalCostUSD float64       `json:"total_cost_usd"`
		Usage        *usagePayload `json:"usage"`
		Message      *struct {
			Usage   *usagePayload `json:"usage"`
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return Event{}, false
	}

	switch msg.Type {
	case "system":
		return Event{Kind: EventStatus, Note: msg.Subtype}, true

	case "assistant":
		ev := Event{Kind: EventTurn}
		if msg.Message != nil {
			if u := msg.Message.Usage; u != nil {
				ev.TokensIn, ev.TokensOut = u.in(), u.out()
			}
			for _, block := range msg.Message.Content {
				if block.Type == "tool_use" && block.Name != "" {
					// Emit the tool name only. `input` is deliberately not in the struct.
					ev.Kind = EventToolUse
					ev.Tool = block.Name
					break
				}
			}
		}
		return ev, true

	case "result":
		ev := Event{Kind: EventFinished, Turns: msg.NumTurns, CostUSD: msg.TotalCostUSD}
		if msg.Usage != nil {
			ev.TokensIn, ev.TokensOut = msg.Usage.in(), msg.Usage.out()
		}
		if msg.IsError || msg.Subtype == "error" {
			ev.Kind = EventError
			ev.Note = msg.Subtype
		}
		return ev, true
	}
	return Event{}, false
}

type usagePayload struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	PromptTokens             int64 `json:"prompt_tokens"`
	CompletionTokens         int64 `json:"completion_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
}

func (u *usagePayload) in() int64 {
	if u == nil {
		return 0
	}
	if u.InputTokens > 0 || u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0 {
		return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	}
	return u.PromptTokens
}

func (u *usagePayload) out() int64 {
	if u == nil {
		return 0
	}
	if u.OutputTokens > 0 {
		return u.OutputTokens
	}
	return u.CompletionTokens
}

// opencodeAdapter parses OpenCode's `run --format json` event stream.
//
// Only two of its event shapes carry metadata worth keeping: `tool_use` (whose `part.tool`
// is the tool's *name*; `part.state` holds the arguments and results and is never read) and
// `step_finish` (one model call's tokens and cost, incremental per step). `text` parts —
// the assistant's own output — are skipped outright, so nothing readable leaves the adapter.
func opencodeAdapter(line []byte) (Event, bool) {
	var msg struct {
		Type string `json:"type"`
		Part struct {
			Tool   string  `json:"tool"`
			Cost   float64 `json:"cost"`
			Tokens *struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"part"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return Event{}, false
	}

	switch msg.Type {
	case "step_finish":
		ev := Event{Kind: EventTurn, CostUSD: msg.Part.Cost}
		if tk := msg.Part.Tokens; tk != nil {
			ev.TokensIn = tk.Input + tk.Cache.Read + tk.Cache.Write
			ev.TokensOut = tk.Output
		}
		return ev, true
	case "tool_use":
		if msg.Part.Tool == "" {
			return Event{}, false
		}
		return Event{Kind: EventToolUse, Tool: msg.Part.Tool}, true
	}
	return Event{}, false
}

// genericAdapter parses a JSONL event stream defensively, extracting whichever of the
// well-known metadata keys are present.
//
// Codex and OpenCode both emit JSONL, but their exact schemas vary by version and this build
// has not verified them. Reading a small set of key aliases rather than a fixed schema means
// a version bump degrades to "fewer metrics" instead of "no events at all" — and the privacy
// property holds either way, because unknown keys are simply never read.
func genericAdapter(line []byte) (Event, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}

	ev := Event{Kind: EventStatus}

	typ := firstString(raw, "type", "event", "kind", "msg_type")
	switch {
	case strings.Contains(typ, "error"), boolAt(raw, "is_error"):
		ev.Kind = EventError
		ev.Note = typ
	case strings.Contains(typ, "result"), strings.Contains(typ, "complete"),
		strings.Contains(typ, "finish"), strings.Contains(typ, "done"):
		ev.Kind = EventFinished
	case strings.Contains(typ, "tool"):
		ev.Kind = EventToolUse
		ev.Tool = firstString(raw, "tool", "tool_name", "name")
	case strings.Contains(typ, "message"), strings.Contains(typ, "assistant"),
		strings.Contains(typ, "turn"), strings.Contains(typ, "agent"):
		ev.Kind = EventTurn
	default:
		ev.Note = typ
	}

	if usage := mapAt(raw, "usage", "token_usage", "tokens"); usage != nil {
		ev.TokensIn = firstInt(usage, "input_tokens", "prompt_tokens", "input", "in")
		ev.TokensOut = firstInt(usage, "output_tokens", "completion_tokens", "output", "out")
	}
	if ev.TokensIn == 0 && ev.TokensOut == 0 {
		ev.TokensIn = firstInt(raw, "input_tokens", "prompt_tokens")
		ev.TokensOut = firstInt(raw, "output_tokens", "completion_tokens")
	}
	ev.CostUSD = firstFloat(raw, "total_cost_usd", "cost_usd", "cost")
	if n := firstInt(raw, "num_turns", "turns", "turn"); n > 0 {
		ev.Turns = int(n)
	}
	return ev, true
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstInt(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		}
	}
	return 0
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k].(float64); ok {
			return v
		}
	}
	return 0
}

func boolAt(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func mapAt(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v
		}
	}
	return nil
}
