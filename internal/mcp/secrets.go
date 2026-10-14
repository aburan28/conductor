package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
)

// coord_list_secrets / coord_get_secret: agents retrieve project secrets sealed at rest.
// Values are returned only in the tool result for this session — never put them in
// progress notes, handoffs, or task titles.

func (s *Server) listSecrets(ctx context.Context, _ json.RawMessage) (any, error) {
	var out struct {
		Secrets []map[string]any `json:"secrets"`
	}
	if err := s.api.Get(ctx, "/v1/projects/"+s.project+"/secrets", &out); err != nil {
		return nil, err
	}
	return jsonResult(out), nil
}

func (s *Server) getSecret(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.Name == "" {
		return nil, errors.New("name is required")
	}
	var out map[string]any
	if err := s.api.Get(ctx, "/v1/projects/"+s.project+"/secrets/"+url.PathEscape(args.Name), &out); err != nil {
		return nil, err
	}
	return jsonResult(out), nil
}
