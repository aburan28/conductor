package tokencost

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// Exact counting against a real tokenizer, for when the estimate is not enough.
//
// This is the one place in Conductor that sends file content anywhere — and where it goes
// matters: to the Anthropic API, with the caller's own credentials, from the caller's own
// machine. That is the same trust boundary the harness already crosses on every turn. It is
// never the Conductor server, which remains structurally unable to see content
// (docs/PRIVACY.md). The endpoint counts tokens without running inference, so it is free.
//
// Counts are model-specific; tiktoken-style approximations undercount Claude tokens by
// 15-20%, which is exactly why the estimate in this package is labeled an estimate and this
// path exists for the precise number.

// DefaultExactModel is the tokenizer counts are made against when the caller does not name
// a model.
const DefaultExactModel = "claude-opus-5"

// ExactCounter counts tokens with the Anthropic count-tokens endpoint. The zero value is
// not usable; construct with NewExactCounter, which resolves credentials from the caller's
// environment (ANTHROPIC_API_KEY et al.) exactly as every other Anthropic tool does.
type ExactCounter struct {
	client anthropic.Client
	model  string
}

func NewExactCounter(model string) *ExactCounter {
	if model == "" {
		model = DefaultExactModel
	}
	return &ExactCounter{client: anthropic.NewClient(), model: model}
}

// Model reports which model's tokenizer answers.
func (c *ExactCounter) Model() string { return c.model }

// Count returns the exact input-token count for text under the counter's model.
func (c *ExactCounter) Count(ctx context.Context, text string) (int64, error) {
	if text == "" {
		return 0, nil
	}
	res, err := c.client.Messages.CountTokens(ctx, anthropic.MessageCountTokensParams{
		Model: anthropic.Model(c.model),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(text)),
		},
	})
	if err != nil {
		return 0, fmt.Errorf("count tokens against %s: %w", c.model, err)
	}
	return res.InputTokens, nil
}
