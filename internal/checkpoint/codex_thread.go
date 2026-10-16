package checkpoint

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// CodexBootstrap starts (or resumes) one persistent Codex thread and runs one read-only
// orientation turn. A completed turn is necessary: an empty thread/start response is not
// retained by the Codex session store after app-server exits.
func CodexBootstrap(ctx context.Context, cwd, title, prompt, existingID string, created func(string) error) (string, error) {
	if cwd == "" || prompt == "" {
		return "", errors.New("Codex bootstrap needs a working directory and prompt")
	}
	rpc, stop, err := startCodexRPC(ctx)
	if err != nil {
		return "", err
	}
	defer stop()
	if err := rpc.initialize(ctx); err != nil {
		return "", err
	}
	id := existingID
	if id != "" {
		if err := rpc.call(ctx, 2, "thread/resume", map[string]any{
			"threadId": id, "cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never",
		}, nil); err != nil {
			if !codexThreadMissing(err) {
				return id, err
			}
			// A thread/start response can be recorded before its first turn. Codex
			// discards such an empty thread when that app-server exits. A retry may
			// create a replacement only when Codex explicitly says the ID is absent.
			id = ""
		}
	}
	if id == "" {
		var result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := rpc.call(ctx, 2, "thread/start", map[string]any{
			"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false,
		}, &result); err != nil {
			return "", err
		}
		id = result.Thread.ID
		if id == "" {
			return "", errors.New("Codex thread/start returned no thread ID")
		}
		if created != nil {
			if err := created(id); err != nil {
				return id, err
			}
		}
	}
	if title != "" {
		if err := rpc.call(ctx, 4, "thread/name/set", map[string]string{
			"threadId": id, "name": title,
		}, nil); err != nil {
			return id, err
		}
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := rpc.call(ctx, 3, "turn/start", map[string]any{
		"threadId": id, "input": []map[string]string{{"type": "text", "text": prompt}},
	}, &started); err != nil {
		return id, err
	}
	if started.Turn.ID == "" {
		return id, errors.New("Codex turn/start returned no turn ID")
	}
	if err := rpc.waitTurn(ctx, id, started.Turn.ID); err != nil {
		return id, err
	}
	return id, nil
}

func codexThreadMissing(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") || strings.Contains(message, "no such thread")
}

// VerifyCodexThread uses a fresh app-server process to prove the thread can be reopened
// from durable local state, rather than only existing in the process that created it.
func VerifyCodexThread(ctx context.Context, id, cwd string) error {
	rpc, stop, err := startCodexRPC(ctx)
	if err != nil {
		return err
	}
	defer stop()
	if err := rpc.initialize(ctx); err != nil {
		return err
	}
	return rpc.call(ctx, 2, "thread/resume", map[string]any{"threadId": id, "cwd": cwd}, nil)
}

type codexRPC struct {
	input  io.Writer
	lines  chan []byte
	done   chan struct{}
	events []rpcMessage
}

type rpcMessage struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func startCodexRPC(ctx context.Context) (*codexRPC, func(), error) {
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--listen", "stdio://")
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	// Avoid printing app-server output: a migration transcript is local-only.
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	rpc := newCodexRPC(input, output)
	return rpc, func() {
		close(rpc.done)
		_ = input.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}, nil
}

func newCodexRPC(input io.Writer, output io.Reader) *codexRPC {
	rpc := &codexRPC{input: input, lines: make(chan []byte, 32), done: make(chan struct{})}
	go func() {
		defer close(rpc.lines)
		sc := bufio.NewScanner(output)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			select {
			case rpc.lines <- append([]byte(nil), sc.Bytes()...):
			case <-rpc.done:
				return
			}
		}
	}()
	return rpc
}

func (r *codexRPC) send(id int, method string, params any) error {
	message := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if id != 0 {
		message["id"] = id
	}
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = r.input.Write(data)
	return err
}

func (r *codexRPC) next(ctx context.Context) (rpcMessage, error) {
	select {
	case <-ctx.Done():
		return rpcMessage{}, ctx.Err()
	case data, ok := <-r.lines:
		if !ok {
			return rpcMessage{}, errors.New("Codex app-server closed before confirming the migration")
		}
		var m rpcMessage
		if err := json.Unmarshal(data, &m); err != nil {
			return rpcMessage{}, fmt.Errorf("Codex app-server returned invalid JSON: %w", err)
		}
		return m, nil
	}
}

func (r *codexRPC) call(ctx context.Context, id int, method string, params any, result any) error {
	if err := r.send(id, method, params); err != nil {
		return err
	}
	for {
		message, err := r.next(ctx)
		if err != nil {
			return err
		}
		if message.Method != "" {
			r.events = append(r.events, message)
			continue
		}
		if message.ID != id {
			continue
		}
		if message.Error != nil {
			return fmt.Errorf("Codex %s: %s (code %d)", method, message.Error.Message, message.Error.Code)
		}
		if result != nil {
			return json.Unmarshal(message.Result, result)
		}
		return nil
	}
}

func (r *codexRPC) initialize(ctx context.Context) error {
	if err := r.call(ctx, 1, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "conductor", "title": "Conductor", "version": "0.1.0"},
	}, nil); err != nil {
		return err
	}
	return r.send(0, "initialized", map[string]any{})
}

func (r *codexRPC) waitTurn(ctx context.Context, threadID, turnID string) error {
	for {
		var message rpcMessage
		if len(r.events) > 0 {
			message, r.events = r.events[0], r.events[1:]
		} else {
			var err error
			message, err = r.next(ctx)
			if err != nil {
				return err
			}
		}
		if message.Method != "turn/completed" {
			continue
		}
		var event struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &event) != nil || event.ThreadID != threadID || event.Turn.ID != turnID {
			continue
		}
		if event.Turn.Status != "completed" {
			return fmt.Errorf("Codex orientation turn ended with status %q", strings.TrimSpace(event.Turn.Status))
		}
		return nil
	}
}
