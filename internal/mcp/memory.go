package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aburan28/conductor/internal/memory"
)

func memoryToolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "memory_search",
			"description": "Search private Conductor observations for the current repository. Returns compact IDs and summaries from the owner's Redis store; never queries the shared coordination server.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
			}},
		},
		{
			"name":        "memory_timeline",
			"description": "Show nearby private observations from the same session as one memory ID.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id":    map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
			}, "required": []string{"id"}},
		},
		{
			"name":        "memory_get",
			"description": "Read one selected private Conductor observation by ID.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string"},
			}, "required": []string{"id"}},
		},
		{
			"name":        "memory_remember",
			"description": "Save a concise, explicit project observation to the owner's encrypted Redis memory store.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"summary": map[string]any{"type": "string", "maxLength": 8192},
				"kind":    map[string]any{"type": "string"},
			}, "required": []string{"summary"}},
		},
	}
}

func (s *Server) callMemory(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	store, err := memory.Open()
	if err != nil {
		return nil, err
	}
	defer store.Close()
	project := memory.ProjectForDir("")
	var args struct {
		Query   string `json:"query"`
		ID      string `json:"id"`
		Limit   int    `json:"limit"`
		Summary string `json:"summary"`
		Kind    string `json:"kind"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}
	switch name {
	case "memory_search":
		items, err := store.Search(ctx, project, args.Query, args.Limit)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]any{"project": project, "observations": memory.Previews(items)}), nil
	case "memory_timeline":
		if args.ID == "" {
			return nil, errors.New("id is required")
		}
		items, err := store.Timeline(ctx, project, args.ID, args.Limit)
		if err != nil {
			return nil, err
		}
		return jsonResult(map[string]any{"observations": memory.Previews(items)}), nil
	case "memory_get":
		if args.ID == "" {
			return nil, errors.New("id is required")
		}
		item, err := store.Get(ctx, project, args.ID)
		if err != nil {
			return nil, err
		}
		return jsonResult(item), nil
	case "memory_remember":
		if args.Summary == "" {
			return nil, errors.New("summary is required")
		}
		item, err := store.Remember(ctx, memory.Observation{Project: project, Summary: args.Summary, Kind: args.Kind, Session: string(s.session), Harness: "mcp"})
		if err != nil {
			return nil, err
		}
		return jsonResult(item), nil
	default:
		return nil, fmt.Errorf("unknown memory tool %q", name)
	}
}
