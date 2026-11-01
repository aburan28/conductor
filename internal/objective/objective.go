// Package objective is Delphi's objective tree (phase 4 of docs/DELPHI_OBJECTIVES.md): a
// broad objective is decomposed into sub-objectives and work items, each work item is sent
// to the cheapest model measured good enough for its task family, every answer is checked
// independently, and only what fails escalates.
//
// The tree is event-sourced. objective.jsonl is append-only, and a node's state is the fold
// of its events, so a crashed or stopped run resumes from exactly what was recorded and the
// file (objective.jsonl, beside the request store) is the audit trail: who was asked, what they answered (by sha256 and cost), who
// checked it and why it was accepted, escalated or failed.
//
// The package is pure logic. It never calls a model or runs a command; the runner in
// cmd/delphi does, and reports back as events.
package objective

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kinds of node.
const (
	KindObjective = "objective" // decomposed into children
	KindWork      = "work"      // one delegation, answered by one model
)

// States of a node.
const (
	Open      = "open"      // waiting for its next step
	Delegated = "delegated" // a request is out (decomposition or work)
	Checking  = "checking"  // answered; the check is out
	Accepted  = "accepted"
	Failed    = "failed"
	Abandoned = "abandoned" // a sibling failed, so the parent stopped needing it
)

// Limits bound a tree. A decomposition that breaks one is refused, not trimmed.
type Limits struct {
	MaxDepth  int `json:"max_depth"`  // the root is depth 0
	MaxFanout int `json:"max_fanout"` // children per node
	// MaxAttempts is how many models a node may try: the routed choice plus escalations.
	MaxAttempts int `json:"max_attempts"`
}

// DefaultLimits keeps a tree small enough for a person to read.
var DefaultLimits = Limits{MaxDepth: 3, MaxFanout: 6, MaxAttempts: 3}

// Config is frozen at init, in the first event.
type Config struct {
	Limits Limits `json:"limits"`
	// PlanFamily routes decompositions; ReviewFamily routes independent reviews ("" = none).
	PlanFamily   string `json:"plan_family"`
	ReviewFamily string `json:"review_family,omitempty"`
	// Families are the task families a work item may name, frozen at init.
	Families []string `json:"families"`
	// Evaluator is an operator-supplied executable that checks every answer (JSON on
	// stdin/stdout, like `delphi goal -evaluator`). Model-written commands are never run.
	Evaluator       string `json:"evaluator,omitempty"`
	EvalTimeoutSecs int    `json:"eval_timeout_seconds,omitempty"`
	MaxTokens       int    `json:"max_tokens"`
	// BatchWindowSecs is how long a batch API may take. Inside that window of a node's
	// deadline, its calls go only to live providers.
	BatchWindowSecs int `json:"batch_window_seconds,omitempty"`
}

// Attempt is one model asked to answer a node (or, for an objective, to decompose it).
type Attempt struct {
	Target    string  `json:"target"`     // provider:model[@effort]
	RequestID string  `json:"request_id"` // delphi request id, unique in the run
	Status    string  `json:"status"`     // delphi status; "" while out
	AnswerSHA string  `json:"answer_sha256,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Tokens    int64   `json:"tokens,omitempty"`
	Priced    bool    `json:"priced,omitempty"`
	// Outcome is why this attempt ended: accepted, or the check or refusal that rejected it.
	Outcome string `json:"outcome,omitempty"`
	// Reviewer is the independent reviewer chosen for this attempt's answer.
	Reviewer string `json:"reviewer,omitempty"`
	// ChainIndex is Target's place in the node's route; an attempt may skip a batch model
	// when the deadline is too near for it.
	ChainIndex int `json:"chain_index"`
}

// Check is one verdict on an answer.
type Check struct {
	Attempt   int     `json:"attempt"` // index into Attempts
	By        string  `json:"by"`      // "evaluator" or the reviewing target
	RequestID string  `json:"request_id,omitempty"`
	Passed    bool    `json:"passed"`
	Feedback  string  `json:"feedback"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Tokens    int64   `json:"tokens,omitempty"`
	Priced    bool    `json:"priced,omitempty"`
}

// Node is the folded state of one node.
type Node struct {
	ID        string   `json:"id"`
	Parent    string   `json:"parent,omitempty"`
	Depth     int      `json:"depth"`
	Kind      string   `json:"kind"`
	Statement string   `json:"statement"`
	Criteria  []string `json:"criteria"`
	Family    string   `json:"family,omitempty"` // work items: the task family it routes by
	BudgetUSD float64  `json:"budget_usd"`       // grants included
	// DeadlineAt (RFC 3339) is when the node must be settled; "" for none.
	DeadlineAt string `json:"deadline_at,omitempty"`

	State    string    `json:"state"`
	Route    []string  `json:"route,omitempty"` // the chain this node climbs
	Basis    string    `json:"basis,omitempty"`
	Attempts []Attempt `json:"attempts,omitempty"`
	Checks   []Check   `json:"checks,omitempty"`
	Children []string  `json:"children,omitempty"`
	// Superseded are earlier generations of children, replaced by a re-decomposition after
	// their combined answer was rejected. They stay in the tree as history.
	Superseded []string `json:"superseded,omitempty"`
	Grants     []Grant  `json:"grants,omitempty"`
	Answer     string   `json:"answer,omitempty"` // accepted (or last) answer
	Reason     string   `json:"reason,omitempty"` // why it failed or was abandoned
	// SpentUSD and SpentTokens include every descendant's calls.
	SpentUSD    float64 `json:"spent_usd"`
	SpentTokens int64   `json:"spent_tokens"`
	Unpriced    int     `json:"unpriced_calls,omitempty"`
}

// Terminal reports whether the node will change no more.
func (n *Node) Terminal() bool {
	return n.State == Accepted || n.State == Failed || n.State == Abandoned
}

// Current is the attempt in progress or last made.
func (n *Node) Current() *Attempt {
	if len(n.Attempts) == 0 {
		return nil
	}
	return &n.Attempts[len(n.Attempts)-1]
}

// Event is one line of objective.jsonl.
type Event struct {
	Seq  int             `json:"seq"`
	At   string          `json:"at"`
	Node string          `json:"node,omitempty"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Event types and their data.
const (
	EvInit       = "init"       // InitData
	EvRouted     = "routed"     // RoutedData: the chain a node will climb
	EvDelegated  = "delegated"  // Attempt (without result)
	EvAnswered   = "answered"   // AnsweredData
	EvDecomposed = "decomposed" // DecomposedData
	EvChecked    = "checked"    // Check
	EvAccepted   = "accepted"   // ReasonData
	EvFailed     = "failed"     // ReasonData
	EvAbandoned  = "abandoned"  // ReasonData
	EvReviewer   = "reviewer"   // ReviewerData: who reviews the current attempt
	EvSynthesis  = "synthesis"  // SynthesisData: an objective's children, combined for its check
	EvGrant      = "grant"      // Grant: an ancestor's spare budget given to a node that ran out
)

// Grant is budget an ancestor gave a node that ran out, instead of the node failing.
type Grant struct {
	From      string  `json:"from"` // the ancestor whose unallocated budget paid for it
	AmountUSD float64 `json:"amount_usd"`
	Reason    string  `json:"reason"`
}

type ReviewerData struct {
	Target string `json:"target"`
}

type SynthesisData struct {
	Answer string `json:"answer"`
}

type InitData struct {
	Config     Config   `json:"config"`
	Statement  string   `json:"statement"`
	Criteria   []string `json:"criteria"`
	BudgetUSD  float64  `json:"budget_usd"`
	DeadlineAt string   `json:"deadline_at,omitempty"`
}

type RoutedData struct {
	Chain []string `json:"chain"`
	Basis string   `json:"basis"`
}

type AnsweredData struct {
	RequestID string  `json:"request_id"`
	Status    string  `json:"status"`
	Answer    string  `json:"answer,omitempty"`
	AnswerSHA string  `json:"answer_sha256,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
	Tokens    int64   `json:"tokens"`
	Priced    bool    `json:"priced"`
	// Outcome is set when the answer itself ends the attempt (a refusal, an error, an
	// invalid decomposition) without a check.
	Outcome string `json:"outcome,omitempty"`
}

type DecomposedData struct {
	Children []Child `json:"children"`
}

type ReasonData struct {
	Reason string `json:"reason"`
}

// Tree is the fold of a run's events.
type Tree struct {
	Config  Config
	Root    string
	Nodes   map[string]*Node
	order   []string // creation order
	seq     int
	handled map[string]bool // request ids already folded (answers and reviews)
}

// Get returns a node or nil.
func (t *Tree) Get(id string) *Node { return t.Nodes[id] }

// IDs lists nodes in creation order (parents before children).
func (t *Tree) IDs() []string { return append([]string(nil), t.order...) }

// Handled reports whether a request's answer has already been recorded.
func (t *Tree) Handled(requestID string) bool { return t.handled[requestID] }

// Apply folds one event. It is the only way a tree changes.
func (t *Tree) Apply(e Event) error {
	if e.Seq != t.seq+1 {
		return fmt.Errorf("event %d out of order (want %d)", e.Seq, t.seq+1)
	}
	t.seq = e.Seq
	if e.Type == EvInit {
		var d InitData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		t.Config = d.Config
		t.Root = "n0"
		t.add(&Node{ID: "n0", Kind: KindObjective, Statement: d.Statement, Criteria: d.Criteria, BudgetUSD: d.BudgetUSD,
			DeadlineAt: d.DeadlineAt, State: Open})
		return nil
	}
	n := t.Nodes[e.Node]
	if n == nil {
		return fmt.Errorf("event %d names unknown node %q", e.Seq, e.Node)
	}
	switch e.Type {
	case EvRouted:
		var d RoutedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		n.Route, n.Basis = d.Chain, d.Basis
	case EvDelegated:
		var a Attempt
		if err := json.Unmarshal(e.Data, &a); err != nil {
			return err
		}
		n.Attempts = append(n.Attempts, a)
		n.State = Delegated
	case EvAnswered:
		var d AnsweredData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		t.handled[d.RequestID] = true
		a := n.Current()
		if a == nil || a.RequestID != d.RequestID {
			return fmt.Errorf("event %d answers %s, which %s is not waiting for", e.Seq, d.RequestID, n.ID)
		}
		a.Status, a.AnswerSHA, a.CostUSD, a.Tokens, a.Priced, a.Outcome = d.Status, d.AnswerSHA, d.CostUSD, d.Tokens, d.Priced, d.Outcome
		t.spend(n, d)
		if d.Answer != "" {
			n.Answer = d.Answer
		}
		if d.Outcome == "" {
			n.State = Checking
		} else {
			n.State = Open // the attempt ended without an answer worth checking
		}
	case EvDecomposed:
		var d DecomposedData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		if a := n.Current(); a != nil {
			a.Outcome = "decomposed"
		}
		// A re-decomposition replaces the previous generation, which stays as history; ids
		// keep counting so none is ever reused.
		n.Superseded = append(n.Superseded, n.Children...)
		n.Children = nil
		at, _ := time.Parse(time.RFC3339Nano, e.At)
		for i, c := range d.Children {
			id := fmt.Sprintf("%s.%d", n.ID, len(n.Superseded)+i+1)
			n.Children = append(n.Children, id)
			deadline := n.DeadlineAt
			if c.DeadlineMinutes > 0 {
				deadline = at.Add(time.Duration(c.DeadlineMinutes) * time.Minute).UTC().Format(time.RFC3339)
			}
			t.add(&Node{ID: id, Parent: n.ID, Depth: n.Depth + 1, Kind: c.Kind, Statement: c.Statement,
				Criteria: c.Criteria, Family: c.Family, BudgetUSD: c.BudgetUSD, DeadlineAt: deadline, State: Open})
		}
		n.State = Delegated // waiting on its children
	case EvChecked:
		var c Check
		if err := json.Unmarshal(e.Data, &c); err != nil {
			return err
		}
		if c.RequestID != "" {
			t.handled[c.RequestID] = true
			t.spend(n, AnsweredData{CostUSD: c.CostUSD, Tokens: c.Tokens, Priced: c.Priced})
		}
		n.Checks = append(n.Checks, c)
		if !c.Passed {
			if a := n.Current(); a != nil && (a.Outcome == "" || a.Outcome == "decomposed") {
				what := "rejected by "
				if a.Outcome == "decomposed" {
					what = "combined answer rejected by "
				}
				a.Outcome = what + c.By + ": " + c.Feedback
			}
			n.State = Open // the next attempt, if any, climbs the chain
		}
	case EvReviewer:
		var d ReviewerData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		if a := n.Current(); a != nil {
			a.Reviewer = d.Target
		}
	case EvSynthesis:
		var d SynthesisData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		n.Answer, n.State = d.Answer, Checking
	case EvGrant:
		var g Grant
		if err := json.Unmarshal(e.Data, &g); err != nil {
			return err
		}
		if t.Nodes[g.From] == nil || !(g.AmountUSD > 0) {
			return fmt.Errorf("event %d: invalid grant %+v", e.Seq, g)
		}
		// The grant raises the budget of the node and of every ancestor below the giver,
		// so each subtree's budget still covers its children's.
		for p := n; p != nil && p.ID != g.From; p = t.Nodes[p.Parent] {
			p.BudgetUSD += g.AmountUSD
			if p.Parent == "" {
				return fmt.Errorf("event %d: %s is not an ancestor of %s", e.Seq, g.From, n.ID)
			}
		}
		n.Grants = append(n.Grants, g)
	case EvAccepted, EvFailed, EvAbandoned:
		var d ReasonData
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return err
		}
		n.Reason = d.Reason
		n.State = map[string]string{EvAccepted: Accepted, EvFailed: Failed, EvAbandoned: Abandoned}[e.Type]
		if a := n.Current(); a != nil && a.Outcome == "" && e.Type == EvAccepted {
			a.Outcome = "accepted"
		}
	default:
		return fmt.Errorf("event %d has unknown type %q", e.Seq, e.Type)
	}
	return nil
}

func (t *Tree) add(n *Node) {
	t.Nodes[n.ID] = n
	t.order = append(t.order, n.ID)
}

func (t *Tree) spend(n *Node, d AnsweredData) {
	for p := n; p != nil; p = t.Nodes[p.Parent] {
		p.SpentUSD += d.CostUSD
		p.SpentTokens += d.Tokens
		if !d.Priced {
			p.Unpriced++
		}
		if p.Parent == "" {
			break
		}
	}
}

func newTree() *Tree {
	return &Tree{Nodes: map[string]*Node{}, handled: map[string]bool{}}
}

// Log is a run's append-only event file.
type Log struct {
	path string
	tree *Tree
}

// EventsPath is where a run directory keeps its events.
func EventsPath(dir string) string { return filepath.Join(dir, "objective.jsonl") }

// Create starts a run; it refuses a directory that already has one.
func Create(dir string, d InitData) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(EventsPath(dir), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s already holds an objective; resume it with `delphi objective run`", dir)
		}
		return nil, err
	}
	f.Close()
	l := &Log{path: EventsPath(dir), tree: newTree()}
	return l, l.Append("", EvInit, d)
}

// Load folds an existing run.
func Load(dir string) (*Log, error) {
	f, err := os.Open(EventsPath(dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l := &Log{path: EventsPath(dir), tree: newTree()}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// A torn final line (a crash mid-write) is the only damage tolerated.
			if !sc.Scan() {
				break
			}
			return nil, fmt.Errorf("%s: event %d: %w", l.path, l.tree.seq+1, err)
		}
		if err := l.tree.Apply(e); err != nil {
			return nil, fmt.Errorf("%s: %w", l.path, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if l.tree.Root == "" {
		return nil, fmt.Errorf("%s has no init event", l.path)
	}
	return l, nil
}

// Tree is the current fold.
func (l *Log) Tree() *Tree { return l.tree }

// Append records one event durably, then folds it.
func (l *Log) Append(node, typ string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	e := Event{Seq: l.tree.seq + 1, At: time.Now().UTC().Format(time.RFC3339Nano), Node: node, Type: typ, Data: raw}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// Check the event against a copy of the fold first, so an invalid event never reaches
	// the file and the in-memory tree never runs ahead of it.
	if err := l.dryRun(e); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return l.tree.Apply(e)
}

// dryRun applies e to a re-fold of the file, which leaves l.tree untouched.
func (l *Log) dryRun(e Event) error {
	probe, err := Load(filepath.Dir(l.path))
	if err != nil && e.Type != EvInit {
		return err
	}
	if e.Type == EvInit {
		return newTree().Apply(e)
	}
	return probe.tree.Apply(e)
}

// Child is one proposed child in a decomposition.
type Child struct {
	Kind      string   `json:"kind"`
	Statement string   `json:"statement"`
	Criteria  []string `json:"criteria"`
	Family    string   `json:"family,omitempty"`
	BudgetUSD float64  `json:"budget_usd"`
	// DeadlineMinutes optionally gives the child an earlier deadline than its parent's.
	DeadlineMinutes int `json:"deadline_minutes,omitempty"`
}

// Decomposition is what the planner returns.
type Decomposition struct {
	Decomposable bool    `json:"decomposable"`
	Reason       string  `json:"reason,omitempty"`
	Children     []Child `json:"children,omitempty"`
}

// ParseDecomposition reads the planner's reply: one JSON object, optionally fenced.
func ParseDecomposition(text string) (Decomposition, error) {
	var d Decomposition
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, fmt.Errorf("the decomposition is not the requested JSON: %w", err)
	}
	if dec.More() {
		return d, errors.New("the decomposition must be exactly one JSON object")
	}
	return d, nil
}

// Validate checks a decomposition mechanically before any child runs: every child has a
// statement, checkable criteria, a kind, a routable family (work items) and a budget, the
// budgets fit the parent's remaining budget, and the tree stays within its limits.
// routable says whether a family has a model; it is asked once per family.
func Validate(parent *Node, d Decomposition, lim Limits, now time.Time, routable func(family string) error) []string {
	var problems []string
	if !d.Decomposable {
		return []string{"not decomposable: " + strings.TrimSpace(d.Reason)}
	}
	if len(d.Children) == 0 {
		problems = append(problems, "no children")
	}
	if len(d.Children) > lim.MaxFanout {
		problems = append(problems, fmt.Sprintf("%d children, more than the limit of %d", len(d.Children), lim.MaxFanout))
	}
	var sum float64
	checked := map[string]error{}
	for i, c := range d.Children {
		at := fmt.Sprintf("child %d", i+1)
		if strings.TrimSpace(c.Statement) == "" {
			problems = append(problems, at+": no statement")
		}
		real := 0
		for _, k := range c.Criteria {
			if strings.TrimSpace(k) != "" {
				real++
			}
		}
		if real == 0 {
			problems = append(problems, at+": no acceptance criteria")
		}
		if !(c.BudgetUSD > 0) {
			problems = append(problems, at+": budget_usd must be positive")
		}
		sum += c.BudgetUSD
		if c.DeadlineMinutes < 0 {
			problems = append(problems, at+": deadline_minutes cannot be negative")
		} else if c.DeadlineMinutes > 0 && parent.DeadlineAt != "" {
			if left := TimeLeft(parent, now); time.Duration(c.DeadlineMinutes)*time.Minute > left {
				problems = append(problems, fmt.Sprintf("%s: deadline_minutes %d is later than the parent's deadline (%s left)", at, c.DeadlineMinutes, left.Round(time.Minute)))
			}
		}
		switch c.Kind {
		case KindWork:
			if c.Family == "" {
				problems = append(problems, at+": a work item needs a family")
				break
			}
			err, seen := checked[c.Family]
			if !seen {
				err = routable(c.Family)
				checked[c.Family] = err
			}
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: family %q has no model: %v", at, c.Family, err))
			}
		case KindObjective:
			if parent.Depth+1 >= lim.MaxDepth {
				problems = append(problems, fmt.Sprintf("%s: at depth %d it must be a work item (max depth %d)", at, parent.Depth+1, lim.MaxDepth))
			}
			if c.Family != "" {
				problems = append(problems, at+": an objective takes no family; its children do")
			}
		default:
			problems = append(problems, fmt.Sprintf("%s: kind %q is not objective or work", at, c.Kind))
		}
	}
	if remaining := Unallocated(parent, nil); sum > remaining+1e-9 {
		problems = append(problems, fmt.Sprintf("children's budgets sum to $%.4f, more than the $%.4f left", sum, remaining))
	}
	return problems
}

// Vendor is the model family a target belongs to, for independent review: a Claude answer
// is reviewed by a non-Claude model and the reverse. Hosted open models are named by
// their publisher ("vendor/model" on OpenRouter-style endpoints); otherwise the provider.
func Vendor(provider, model string) string {
	switch provider {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	}
	if v, _, ok := strings.Cut(model, "/"); ok && v != "" {
		return strings.ToLower(v)
	}
	m := strings.ToLower(model)
	for _, f := range []struct{ prefix, vendor string }{
		{"claude", "anthropic"}, {"gpt", "openai"}, {"glm", "zhipu"}, {"qwen", "qwen"},
		{"deepseek", "deepseek"}, {"llama", "meta"}, {"gemini", "google"}, {"gemma", "google"},
		{"mistral", "mistral"}, {"kimi", "moonshot"},
	} {
		if strings.HasPrefix(m, f.prefix) {
			return f.vendor
		}
	}
	return provider
}

// Subtree lists id and every descendant.
func (t *Tree) Subtree(id string) []string {
	out := []string{id}
	for _, c := range t.Nodes[id].Children {
		out = append(out, t.Subtree(c)...)
	}
	return out
}

// TimeLeft is how long until the node's deadline; a very long time when it has none.
func TimeLeft(n *Node, now time.Time) time.Duration {
	if n.DeadlineAt == "" {
		return 1<<63 - 1
	}
	at, err := time.Parse(time.RFC3339, n.DeadlineAt)
	if err != nil {
		return 0
	}
	return at.Sub(now)
}

// Unallocated is what a node has neither spent itself nor handed to its current children:
// its budget, less its own calls (decompositions, reviews of its synthesis), less what its
// current children were given, less what superseded children spent. Re-decomposition and
// grants draw on it. With t nil, a node with no children: budget less spend.
func Unallocated(n *Node, t *Tree) float64 {
	if t == nil || (len(n.Children) == 0 && len(n.Superseded) == 0) {
		return n.BudgetUSD - n.SpentUSD
	}
	own := n.SpentUSD
	alloc := 0.0
	for _, id := range n.Children {
		c := t.Nodes[id]
		own -= c.SpentUSD
		alloc += c.BudgetUSD
	}
	for _, id := range n.Superseded {
		c := t.Nodes[id]
		own -= c.SpentUSD
		alloc += c.SpentUSD
	}
	return n.BudgetUSD - own - alloc
}

// Summary counts nodes by state.
func (t *Tree) Summary() map[string]int {
	out := map[string]int{}
	for _, n := range t.Nodes {
		out[n.State]++
	}
	return out
}

// Active lists the nodes that are not terminal, parents first.
func (t *Tree) Active() []*Node {
	var out []*Node
	for _, id := range t.order {
		if n := t.Nodes[id]; !n.Terminal() {
			out = append(out, n)
		}
	}
	return out
}

// ChecksFor lists the checks made on attempt i.
func (n *Node) ChecksFor(i int) []Check {
	var out []Check
	for _, c := range n.Checks {
		if c.Attempt == i {
			out = append(out, c)
		}
	}
	return out
}

// Key is the node id in a form safe for request ids and file names.
func Key(id string) string { return strings.ReplaceAll(id, ".", "_") }

// Waiting lists objectives whose children are all terminal, so they need settling.
func (t *Tree) Waiting() []*Node {
	var out []*Node
	for _, id := range t.order {
		n := t.Nodes[id]
		if n.Kind != KindObjective || n.State != Delegated || len(n.Children) == 0 {
			continue
		}
		if a := n.Current(); a == nil || a.Outcome != "decomposed" {
			continue // a re-decomposition is out; these children are the previous generation
		}
		done := true
		for _, c := range n.Children {
			if !t.Nodes[c].Terminal() {
				done = false
			}
		}
		if done {
			out = append(out, n)
		}
	}
	// Deepest first, so a settled child is folded before its parent looks.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Depth > out[j].Depth })
	return out
}
