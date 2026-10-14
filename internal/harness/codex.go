package harness

import (
	"encoding/json"
	"strconv"
)

// Codex's `codex exec --json` event stream.
//
// Sources (fetched 2026-10-05): the non-interactive mode documentation at
// https://developers.openai.com/codex/noninteractive (now learn.chatgpt.com/docs/non-interactive-mode),
// which lists the event types and the turn.completed usage fields, and the event definitions
// in openai/codex codex-rs/exec/src/exec_events.rs and
// codex-rs/exec/src/event_processor_with_jsonl_output.rs, which fix the item types, their
// statuses, and which events each item gets. Codex is not installed on the machine this was
// written on, so the fixture in testdata/codex_exec.jsonl is built from those definitions,
// not captured from a live run.
//
// The stream, one JSON object per line:
//
//	{"type":"thread.started","thread_id":"…"}
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"…","status":"in_progress"}}
//	{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"…","aggregated_output":"…","exit_code":0,"status":"completed"}}
//	{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"…"}}
//	{"type":"turn.completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122,"reasoning_output_tokens":0}}
//
// Item types: agent_message and reasoning (completed only), command_execution, file_change,
// mcp_tool_call and web_search (started, then completed), todo_list (started, updated,
// completed), and error (completed only; a non-fatal warning). A turn ends in turn.completed
// or turn.failed; a top-level `error` event is a stream-level failure.
//
// Two details matter for metering. turn.completed carries the thread's running total, not
// the turn's own usage (the processor reports its last total-usage update), so a stream with
// several turns is metered by difference. And cached_input_tokens is the cached share of
// input_tokens, as in OpenAI's usage accounting, so it is not added again.
//
// The privacy boundary is the struct below: it has fields for event and item types,
// statuses, an exit code, an MCP server and tool name, and token counts. An agent message's
// text, reasoning, a command line or its output, file changes, tool arguments and results,
// search queries, and error messages have nowhere to land.

// codexStream is the per-run state of the Codex adapter: the last usage total seen, so that a
// cumulative total becomes a per-turn increment.
type codexStream struct {
	lastIn, lastOut int64
}

// newCodexAdapter returns an adapter for one Codex run.
func newCodexAdapter() Adapter {
	var st codexStream
	return st.adapt
}

type codexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

func (st *codexStream) adapt(line []byte) (Event, bool) {
	var msg struct {
		Type  string      `json:"type"`
		Usage *codexUsage `json:"usage"`
		Item  *struct {
			Type     string `json:"type"`
			Status   string `json:"status"`
			ExitCode *int   `json:"exit_code"`
			Server   string `json:"server"`
			Tool     string `json:"tool"`
		} `json:"item"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return Event{}, false
	}

	switch msg.Type {
	case "thread.started", "turn.started":
		return Event{Kind: EventStatus, Note: msg.Type}, true

	case "turn.completed":
		ev := Event{Kind: EventTurn}
		if u := msg.Usage; u != nil {
			in, out := u.InputTokens, u.OutputTokens
			// A running total that goes backwards is not a running total (a resumed thread,
			// or a future Codex that reports per turn): take it as this turn's own usage.
			if in >= st.lastIn && out >= st.lastOut {
				ev.TokensIn, ev.TokensOut = in-st.lastIn, out-st.lastOut
			} else {
				ev.TokensIn, ev.TokensOut = in, out
			}
			st.lastIn, st.lastOut = in, out
		}
		return ev, true

	case "turn.failed", "error":
		// The message is never read: it can quote the model or the prompt.
		return Event{Kind: EventError, Note: msg.Type}, true

	case "item.started", "item.updated", "item.completed":
		if msg.Item == nil || msg.Item.Type == "" {
			return Event{}, false
		}
		it := msg.Item
		if msg.Type == "item.started" {
			// A tool starting is the tool-use signal; its completion is a status, so each
			// call is counted once.
			switch it.Type {
			case "command_execution", "file_change", "web_search":
				return Event{Kind: EventToolUse, Tool: it.Type}, true
			case "mcp_tool_call":
				name := "mcp"
				if it.Server != "" || it.Tool != "" {
					name = "mcp:" + it.Server + "/" + it.Tool
				}
				return Event{Kind: EventToolUse, Tool: name}, true
			}
		}
		note := it.Type
		if it.Status != "" {
			note += " " + it.Status
		}
		if it.ExitCode != nil && *it.ExitCode != 0 {
			note += " (exit code " + strconv.Itoa(*it.ExitCode) + ")"
		}
		return Event{Kind: EventStatus, Note: note}, true
	}
	return Event{}, false
}
