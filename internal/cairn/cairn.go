// Package cairn reads objective state from a cairn node, for routing policies that want to
// spend model capacity in proportion to what a result is verifiably worth.
//
// Read-only, and deliberately over HTTP rather than through the MCP server. A cairn node
// takes its ledger's exclusive write lock at startup, so a second process opening the same
// log to ask a question would be refused -- and would be refusing the search worker, which
// needs that lock more. `cairn serve` answers the same questions over a socket without
// contending for anything.
//
// Everything here is a fact anyone holding the log can re-derive and `cairn audit` can prove.
// That is what makes it admissible in a routing decision: conductor's policies read
// deterministic facts, never a model's account of its own work.
package cairn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/policy"
)

// Client reads one cairn node.
type Client struct {
	Endpoint string
	HTTP     *http.Client
}

// New returns a client for a node's base URL, e.g. http://127.0.0.1:8080.
func New(endpoint string) *Client {
	return &Client{
		Endpoint: strings.TrimSuffix(endpoint, "/"),
		// Short: this call sits in front of dispatching an attempt, and a node that is slow
		// to answer must not hold up work that does not depend on the answer.
		HTTP: &http.Client{Timeout: 5 * time.Second},
	}
}

// objective is the subset of GET /objectives this needs. The node serves more.
type objective struct {
	ID       string `json:"id"`
	Reward   int64  `json:"reward"`
	Settled  bool   `json:"settled"`
	Frontier *struct {
		Score         int64 `json:"score"`
		PoolRemaining int64 `json:"pool_remaining"`
	} `json:"frontier"`
}

// Facts fetches the routing facts for one objective id.
//
// A node that cannot be reached, or that has never heard of this objective, yields
// policy.CairnFacts{} -- Known false, every name nil, no threshold satisfied. That is the
// same shape as "this task is not cairn work", and deliberately so: a routing decision made
// while the ledger is unreachable should fall back to the ordinary ladder rather than act on
// a guess about what an objective is worth.
func (c *Client) Facts(ctx context.Context, objectiveID string) (policy.CairnFacts, error) {
	if c == nil || c.Endpoint == "" || objectiveID == "" {
		return policy.CairnFacts{}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint+"/objectives", nil)
	if err != nil {
		return policy.CairnFacts{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return policy.CairnFacts{}, fmt.Errorf("cairn %s: %w", c.Endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return policy.CairnFacts{}, fmt.Errorf("cairn %s: HTTP %d", c.Endpoint, resp.StatusCode)
	}

	// The listing is an array at the top level, or an object wrapping one; accept both so a
	// node's framing choice is not a breaking change here.
	var body json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return policy.CairnFacts{}, err
	}
	var list []objective
	if err := json.Unmarshal(body, &list); err != nil {
		// A pointer, so that a body simply lacking the key is distinguishable from one
		// carrying an empty list. Unmarshalling into a plain slice succeeds for any JSON
		// object at all and leaves it nil, which would report a misconfigured endpoint as
		// "no such objective" -- the same answer as a healthy node, and unfixable by
		// whoever has to read it.
		var wrapper struct {
			Objectives *[]objective `json:"objectives"`
		}
		if err := json.Unmarshal(body, &wrapper); err != nil || wrapper.Objectives == nil {
			return policy.CairnFacts{}, fmt.Errorf("cairn %s: unrecognised listing", c.Endpoint)
		}
		list = *wrapper.Objectives
	}

	for _, o := range list {
		if o.ID != objectiveID {
			continue
		}
		f := policy.CairnFacts{
			Known:       true,
			ObjectiveID: o.ID,
			Settled:     o.Settled,
			// No frontier yet means nothing has been verified against this objective, so the
			// whole reward is still there and the best score is zero. Both are true
			// statements about the ledger rather than placeholders.
			RewardRemaining: o.Reward,
		}
		if o.Frontier != nil {
			f.FrontierScore = o.Frontier.Score
			f.RewardRemaining = o.Frontier.PoolRemaining
		}
		return f, nil
	}
	// Reached the node, and it does not have this objective. Not an error: a task may name an
	// objective this node has not synced yet.
	return policy.CairnFacts{}, nil
}
