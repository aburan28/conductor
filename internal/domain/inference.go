package domain

import "time"

// InferenceKind is the wire protocol an inference endpoint speaks, which is all discovery
// needs to know about it.
type InferenceKind string

const (
	// InferenceAnthropic is the Anthropic API: GET /v1/models with x-api-key.
	InferenceAnthropic InferenceKind = "anthropic"
	// InferenceOpenAI is the OpenAI API: GET /v1/models with a bearer key.
	InferenceOpenAI InferenceKind = "openai"
	// InferenceOpenAICompatible is anything that serves OpenAI's list shape: vLLM, LM Studio,
	// llama.cpp, LiteLLM, OpenRouter, Fireworks, Together, a corporate gateway.
	InferenceOpenAICompatible InferenceKind = "openai_compatible"
	// InferenceOllama is Ollama's native API: GET /api/tags.
	InferenceOllama InferenceKind = "ollama"
)

// Valid reports whether k is a kind discovery can list.
func (k InferenceKind) Valid() bool {
	switch k {
	case InferenceAnthropic, InferenceOpenAI, InferenceOpenAICompatible, InferenceOllama:
		return true
	}
	return false
}

// InferenceEndpoint is an inference API a project routes to: where it is and which vault
// secret holds its key. The key itself is never part of this record; it stays sealed in the
// project secret vault, so this struct is safe to list and to show to any member.
type InferenceEndpoint struct {
	ID             ID            `json:"id"`
	OrganizationID ID            `json:"organization_id"`
	ProjectID      ID            `json:"project_id"`
	Name           string        `json:"name"`
	Kind           InferenceKind `json:"kind"`
	BaseURL        string        `json:"base_url"`
	// APIKeySecret names the project secret that holds the key; empty for keyless servers.
	APIKeySecret string `json:"api_key_secret,omitempty"`
	Enabled      bool   `json:"enabled"`
	// LastStatus is "never", "ok" or "error". LastError is a short, sanitised reason.
	LastStatus    string     `json:"last_status"`
	LastError     string     `json:"last_error,omitempty"`
	LastRefreshAt *time.Time `json:"last_refresh_at,omitempty"`
	ModelCount    int        `json:"model_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// DiscoveredModel is a model an endpoint reported serving. Rows are never deleted when a
// provider stops listing a model, only marked missing, so "when did this appear" and "when
// was it retired" stay answerable.
type DiscoveredModel struct {
	EndpointID      ID         `json:"endpoint_id"`
	Model           string     `json:"model"`
	DisplayName     string     `json:"display_name,omitempty"`
	CreatedUpstream *time.Time `json:"created_upstream,omitempty"`
	ContextWindow   int        `json:"context_window,omitempty"`
	FirstSeenAt     time.Time  `json:"first_seen_at"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
	MissingSince    *time.Time `json:"missing_since,omitempty"`
}

// ModelSelector lets a catalog profile name the models it wants instead of one model id, so
// a new release that fits is picked up by discovery rather than by editing a file.
type ModelSelector struct {
	// Endpoint is the name of an inference endpoint in the project that declared the profile.
	Endpoint string `json:"endpoint"`
	// Match is a list of globs (`*` and `?`); a model qualifies when it matches any of them.
	Match []string `json:"match"`
	// Exclude removes models that matched, for example snapshot ids or `*-preview`.
	Exclude []string `json:"exclude,omitempty"`
	// Pick chooses among qualifying models. "newest" (the default) is the only strategy.
	Pick string `json:"pick,omitempty"`
	// Prefix is prepended to the discovered id to form the profile's model string, for
	// harnesses that namespace models ("vllm/", "ollama/").
	Prefix string `json:"prefix,omitempty"`
	// ProjectID is the project whose endpoints are searched; set when the profile is stored.
	ProjectID ID `json:"project_id,omitempty"`
	// ResolvedAt is when the profile's model was last chosen or confirmed by discovery.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}
