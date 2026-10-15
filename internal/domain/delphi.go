package domain

import "time"

// DelphiDelegation is one request a member delegated to a frontier model through a provider
// batch API (internal/delphi). It is metadata only: the request body and the answer never
// reach the server's database (DESIGN.md §12.4).
type DelphiDelegation struct {
	ID             ID     `json:"id"`
	OrganizationID ID     `json:"organization_id"`
	ProjectID      ID     `json:"project_id"`
	Ref            string `json:"ref"`
	PrincipalID    ID     `json:"principal_id,omitempty"`
	TaskID         ID     `json:"task_id,omitempty"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	BodySHA256     string `json:"body_sha256"`
	Label          string `json:"label"`
	BatchID        string `json:"batch_id,omitempty"`
	// State is the delegation's lifecycle: submitting (provider outcome not yet known),
	// running, finished, or abandoned (an unknown submit an operator gave up on).
	State string `json:"state"`
	// Status is the outcome once finished: succeeded, refused, errored, expired, canceled
	// or missing.
	Status           string            `json:"status,omitempty"`
	Truncated        bool              `json:"truncated,omitempty"`
	StopReason       string            `json:"stop_reason,omitempty"`
	InputTokens      int64             `json:"input_tokens"`
	OutputTokens     int64             `json:"output_tokens"`
	CacheReadTokens  int64             `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64             `json:"cache_write_tokens,omitempty"`
	CostUSD          float64           `json:"cost_usd"`
	ErrorType        string            `json:"error_type,omitempty"`
	ErrorDetail      string            `json:"error_detail,omitempty"`
	RefusalCategory  string            `json:"refusal_category,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	SubmittedAt      time.Time         `json:"submitted_at"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// Delphi delegation states.
const (
	DelphiSubmitting = "submitting"
	DelphiRunning    = "running"
	DelphiFinished   = "finished"
	DelphiAbandoned  = "abandoned"
)
