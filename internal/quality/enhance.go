package quality

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/domain"
)

// OpenAIEnhancer calls a local or OpenAI-compatible /v1/chat/completions endpoint
// with a cheap model (T1 / verifier.cheap class). Failures are soft: Lint keeps
// heuristic findings only.
type OpenAIEnhancer struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

const defaultEnhanceModel = "verifier.cheap"

func (e *OpenAIEnhancer) client() *http.Client {
	if e.Client != nil {
		return e.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (e *OpenAIEnhancer) model() string {
	if strings.TrimSpace(e.Model) == "" {
		return defaultEnhanceModel
	}
	return strings.TrimSpace(e.Model)
}

type chatReq struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type enhancePrompt struct {
	Summary            string                `json:"summary"`
	Objective          string                `json:"objective,omitempty"`
	AcceptanceCriteria []enhanceAC           `json:"acceptance_criteria,omitempty"`
	Scopes             []domain.ScopeRequest `json:"scopes,omitempty"`
	Labels             []string              `json:"labels,omitempty"`
}

type enhanceAC struct {
	Text string `json:"text"`
}

func (e *OpenAIEnhancer) Enhance(ctx context.Context, env Envelope, base []Finding) ([]Finding, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(e.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("quality: no enhance base URL")
	}
	prompt := buildEnhancePrompt(env, base)
	body, err := json.Marshal(chatReq{
		Model:       e.model(),
		Temperature: 0,
		Messages: []chatMessage{
			{Role: "system", Content: enhanceSystem},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return nil, err
	}
	url := joinChatURL(baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quality enhance: HTTP %d", resp.StatusCode)
	}
	var cr chatResp
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, nil
	}
	return parseEnhanceJSON(cr.Choices[0].Message.Content)
}

func joinChatURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/chat/completions") {
		return baseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/chat/completions"
	}
	return baseURL + "/v1/chat/completions"
}

const enhanceSystem = `You are Conductor quality.lint. Review the public task envelope (never chats or secrets).
Return ONLY a JSON array of findings: [{"severity":"info|warning|error","code":"snake_case","where":"field","message":"..."}].
At most 8 findings. Be concrete. Do not invent files. Do not ask for secrets. Advisory only — never a merge gate.`

func buildEnhancePrompt(env Envelope, base []Finding) string {
	acs := make([]enhanceAC, 0, len(env.AcceptanceCriteria))
	for _, ac := range env.AcceptanceCriteria {
		acs = append(acs, enhanceAC{Text: ac.Text})
	}
	var b strings.Builder
	b.WriteString("Envelope:\n")
	enc, _ := json.MarshalIndent(enhancePrompt{
		Summary:            env.Summary,
		Objective:          env.Objective,
		AcceptanceCriteria: acs,
		Scopes:             env.Scopes,
		Labels:             env.Labels,
	}, "", "  ")
	b.Write(enc)
	b.WriteString("\n\nHeuristic findings already reported:\n")
	hb, _ := json.MarshalIndent(base, "", "  ")
	b.Write(hb)
	b.WriteString("\n\nAdd only new findings JSON array:")
	return b.String()
}

func parseEnhanceJSON(content string) ([]Finding, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}
	// Models sometimes wrap JSON in fences.
	if i := strings.Index(content, "["); i >= 0 {
		if j := strings.LastIndex(content, "]"); j > i {
			content = content[i : j+1]
		}
	}
	var out []Finding
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return nil, err
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out, nil
}
