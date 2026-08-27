// Package usage turns what each harness records about its own model calls into a uniform
// token ledger, so Conductor can say how much a team's agents consume — by harness, by model,
// by person, by hour — without ever seeing a word of what was said.
//
// Every harness already keeps this data, because every harness bills for it: Claude Code
// writes a usage block on each assistant message in its session transcript, Codex emits a
// token_count event after every response in its rollout log, and OpenCode stores tokens and
// cost on each assistant message. The readers in this package open those stores read-only,
// extract only the numbers, the model, and the timestamp, and discard the rest before it
// leaves the function. The transcript text itself is never held in a struct, never logged,
// and never sent anywhere (DESIGN.md §12: no field in the shared model can carry it).
package usage

import (
	"sort"
	"time"
)

// Turn is one model call as the harness recorded it: who was called, when, and what it cost.
type Turn struct {
	Harness           string
	ExternalSessionID string // the harness's own session id, so re-reading is idempotent
	Model             string
	Provider          string
	Effort            string
	At                time.Time
	Input             int64 // uncached prompt tokens
	CacheRead         int64 // prompt tokens served from cache
	CacheWrite        int64 // prompt tokens written to cache
	Output            int64 // completion tokens, including reasoning where the harness folds it in
	Reasoning         int64 // the reasoning share of Output, when the harness reports it separately
	CostUSD           float64
}

// Bucket is the unit the ledger stores: one harness session, one model, one hour.
//
// Hourly buckets are the compromise between "every call" (Codex logs thousands per session)
// and "one number per session" (which cannot show a pattern over time). A bucket's counters
// are absolute, not deltas, so a collector that re-reads a whole log from the top reproduces
// exactly the same buckets, and the server can upsert them without double counting.
type Bucket struct {
	Harness           string    `json:"harness"`
	ExternalSessionID string    `json:"external_session_id"`
	Model             string    `json:"model,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	Effort            string    `json:"reasoning_effort,omitempty"`
	Start             time.Time `json:"bucket_start"`
	Requests          int64     `json:"requests"`
	Input             int64     `json:"input_tokens"`
	CacheRead         int64     `json:"cache_read_tokens"`
	CacheWrite        int64     `json:"cache_write_tokens"`
	Output            int64     `json:"output_tokens"`
	Reasoning         int64     `json:"reasoning_tokens"`
	CostUSD           float64   `json:"cost_usd"`
}

// Total is the sum of prompt and completion tokens, the number people mean by "tokens".
func (b Bucket) Total() int64 { return b.Input + b.CacheRead + b.CacheWrite + b.Output }

// Aggregate folds turns into hourly buckets, ordered by time then session then model.
func Aggregate(turns []Turn) []Bucket {
	type key struct {
		harness, session, model, provider, effort string
		start                                     time.Time
	}
	acc := map[key]*Bucket{}
	for _, t := range turns {
		k := key{t.Harness, t.ExternalSessionID, t.Model, t.Provider, t.Effort, t.At.UTC().Truncate(time.Hour)}
		b := acc[k]
		if b == nil {
			b = &Bucket{Harness: t.Harness, ExternalSessionID: t.ExternalSessionID, Model: t.Model,
				Provider: t.Provider, Effort: t.Effort, Start: k.start}
			acc[k] = b
		}
		b.Requests++
		b.Input += t.Input
		b.CacheRead += t.CacheRead
		b.CacheWrite += t.CacheWrite
		b.Output += t.Output
		b.Reasoning += t.Reasoning
		b.CostUSD += t.CostUSD
	}
	out := make([]Bucket, 0, len(acc))
	for _, b := range acc {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Start.Equal(b.Start) {
			return a.Start.Before(b.Start)
		}
		if a.ExternalSessionID != b.ExternalSessionID {
			return a.ExternalSessionID < b.ExternalSessionID
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Effort < b.Effort
	})
	return out
}

// Changed returns the buckets in cur that differ from prev (keyed by session, model, effort,
// hour), so a collector only sends what moved since its last report.
func Changed(prev, cur []Bucket) []Bucket {
	seen := make(map[string]Bucket, len(prev))
	for _, b := range prev {
		seen[b.Key()] = b
	}
	var out []Bucket
	for _, b := range cur {
		if old, ok := seen[b.Key()]; !ok || old != b {
			out = append(out, b)
		}
	}
	return out
}

// Key identifies a bucket within one collector's scope.
func (b Bucket) Key() string {
	return b.Harness + "\x00" + b.ExternalSessionID + "\x00" + b.Model + "\x00" + b.Provider + "\x00" +
		b.Effort + "\x00" + b.Start.UTC().Format(time.RFC3339)
}
