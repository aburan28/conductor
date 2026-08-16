package harness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The adapters are the privacy boundary between a coding agent and the shared control plane.
// These tests assert that boundary mechanically, because "we were careful" is not a
// guarantee (DESIGN.md §12.4, §32.4).

const secret = "SENSITIVE-PROMPT-CONTENT-DO-NOT-STORE"

func eventContainsSecret(t *testing.T, ev Event) {
	t.Helper()
	body, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Errorf("adapter leaked content into the event: %s", body)
	}
}

func TestClaudeAdapterExtractsMetadataOnly(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","cwd":"/repo","tools":["Edit"]}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + secret + `"}],` +
			`"usage":{"input_tokens":1500,"output_tokens":420}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit",` +
			`"input":{"file_path":"/repo/a.go","new_string":"` + secret + `"}}],` +
			`"usage":{"input_tokens":200,"output_tokens":50}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"` + secret + `"}]}}`,
		`{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":0.0412,` +
			`"result":"` + secret + `","usage":{"input_tokens":40,"output_tokens":10}}`,
	}

	var got []Event
	for _, line := range lines {
		ev, ok := claudeAdapter([]byte(line))
		if !ok {
			continue
		}
		eventContainsSecret(t, ev)
		got = append(got, ev)
	}

	if len(got) < 4 {
		t.Fatalf("parsed %d events, want at least 4", len(got))
	}

	var sawToolUse, sawFinished bool
	var totalIn, totalOut int64
	var cost float64
	for _, ev := range got {
		totalIn += ev.TokensIn
		totalOut += ev.TokensOut
		if ev.CostUSD > cost {
			cost = ev.CostUSD
		}
		if ev.Kind == EventToolUse {
			sawToolUse = true
			if ev.Tool != "Edit" {
				t.Errorf("tool name = %q, want Edit", ev.Tool)
			}
		}
		if ev.Kind == EventFinished {
			sawFinished = true
			if ev.Turns != 3 {
				t.Errorf("turns = %d, want 3", ev.Turns)
			}
		}
	}
	if !sawToolUse {
		t.Error("tool_use block produced no tool event")
	}
	if !sawFinished {
		t.Error("result line produced no finished event")
	}
	if totalIn != 1740 || totalOut != 480 {
		t.Errorf("token totals = (%d, %d), want (1740, 480)", totalIn, totalOut)
	}
	if cost != 0.0412 {
		t.Errorf("cost = %v, want 0.0412", cost)
	}
}

func TestGenericAdapterExtractsMetadataOnly(t *testing.T) {
	lines := []string{
		`{"type":"agent_message","text":"` + secret + `","usage":{"input_tokens":900,"output_tokens":120}}`,
		`{"type":"tool_call","tool_name":"apply_patch","arguments":{"patch":"` + secret + `"}}`,
		`{"type":"task_complete","num_turns":2,"total_cost_usd":0.02,"summary":"` + secret + `"}`,
		`{"type":"error","message":"` + secret + `"}`,
	}
	for _, line := range lines {
		ev, ok := genericAdapter([]byte(line))
		if !ok {
			t.Fatalf("failed to parse %s", line)
		}
		eventContainsSecret(t, ev)
	}

	ev, _ := genericAdapter([]byte(lines[1]))
	if ev.Kind != EventToolUse || ev.Tool != "apply_patch" {
		t.Errorf("tool event = %+v, want tool_use/apply_patch", ev)
	}
	ev, _ = genericAdapter([]byte(lines[2]))
	if ev.Kind != EventFinished || ev.Turns != 2 {
		t.Errorf("completion event = %+v, want finished with 2 turns", ev)
	}
	ev, _ = genericAdapter([]byte(lines[3]))
	if ev.Kind != EventError {
		t.Errorf("error event kind = %s, want error", ev.Kind)
	}
}

func TestAdaptersRejectGarbageWithoutPanicking(t *testing.T) {
	for _, line := range []string{"", "not json", "{", `{"type":`, `[]`, `null`} {
		if _, ok := claudeAdapter([]byte(line)); ok && line != "null" {
			t.Errorf("claudeAdapter accepted garbage: %q", line)
		}
		_, _ = genericAdapter([]byte(line))
	}
}

// The Event type must never gain a field that can carry model output. If someone adds one,
// this test is the thing that stops it.
func TestEventTypeHasNoContentField(t *testing.T) {
	allowed := map[string]bool{
		"Kind": true, "Tool": true, "Turns": true, "TokensIn": true,
		"TokensOut": true, "CostUSD": true, "Note": true, "At": true,
	}
	rt := reflect.TypeOf(Event{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if !allowed[name] {
			t.Errorf("Event gained field %q; if it can carry harness output, the privacy "+
				"boundary is broken. Add it to `allowed` only if it is metadata.", name)
		}
	}
}

func TestExpandTemplateDoesNotResplitArguments(t *testing.T) {
	// An instruction containing spaces and quotes must stay exactly one argument, or a task
	// title becomes command-line injection.
	args := expandTemplate(
		[]string{"run", "--model", "{model}", "--prompt", "{instruction}"},
		RunSpec{Model: "some-model", Instruction: `fix "the thing"; rm -rf /`},
	)
	if len(args) != 5 {
		t.Fatalf("args = %#v, want 5 entries", args)
	}
	if args[4] != `fix "the thing"; rm -rf /` {
		t.Errorf("instruction argument was altered: %q", args[4])
	}
}

func TestExpandTemplateDropsEmptyPlaceholders(t *testing.T) {
	args := expandTemplate([]string{"run", "--model", "{model}"}, RunSpec{})
	for _, a := range args {
		if a == "" {
			t.Errorf("empty argument survived: %#v", args)
		}
	}
}
