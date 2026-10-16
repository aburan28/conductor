package main

import (
	"reflect"
	"testing"

	"github.com/aburan28/conductor/internal/delphi/hosted"
)

func TestDelphiFallbackBody(t *testing.T) {
	base := hosted.SubmitRequest{Ref: "q", Provider: "anthropic", Model: "claude-mythos-5-1", Effort: "high", Text: "t",
		Params: map[string]any{"thinking": map[string]any{"type": "adaptive"}}, Profile: "frontier"}
	cases := map[string]hosted.SubmitRequest{
		// Same provider: effort and params carry over unless the target sets effort.
		"anthropic:claude-opus-5-5": {Ref: "q", Provider: "anthropic", Model: "claude-opus-5-5", Effort: "high", Text: "t", Params: base.Params},
		// Another provider: its params and effort are dropped; the target's effort applies.
		"openai:gpt-daybreak@xhigh": {Ref: "q", Provider: "openai", Model: "gpt-daybreak", Effort: "xhigh", Text: "t"},
		// A profile: the server resolves provider, model and effort.
		"implementer": {Ref: "q", Profile: "implementer", Text: "t"},
	}
	for target, want := range cases {
		if got := delphiFallbackBody(base, target); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", target, got, want)
		}
	}
	if got := splitList(" a, ,b,"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("splitList = %q", got)
	}
}
