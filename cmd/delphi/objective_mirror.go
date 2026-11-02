package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/domain"
	"github.com/aburan28/conductor/internal/objective"
)

// The objective mirror makes an objective tree visible in Conductor: one task per node,
// linked through parent, with the tree's decisions on each task's timeline, and each task
// settled (done, failed, cancelled) when its node is. objective.jsonl stays the source of
// truth; the mirror replays it, remembers how far it got in conductor.json, and catches up
// after an outage or a restart, so a Conductor that is down never stops the tree.
//
// What crosses: statements and criteria (as any agent-filed task's title and criteria),
// routing, targets, answer sha256s, token counts, costs, verdicts and outcome categories.
// What never does: prompts, answers, reviewer feedback (DESIGN.md §12.4).

const objectiveLabel = "delphi-objective"

type mirrorTask struct {
	ID  string `json:"id"`
	Ref string `json:"ref"`
}

type mirrorState struct {
	Endpoint string                `json:"endpoint"`
	Project  string                `json:"project"`
	Run      string                `json:"run"`
	Seq      int                   `json:"seq"` // last objective event mirrored
	Tasks    map[string]mirrorTask `json:"tasks"`
}

type objMirror struct {
	path string
	st   mirrorState
	c    *client.Client
}

func mirrorPath(dir string) string { return filepath.Join(dir, "conductor.json") }

// initMirror links a new objective to a Conductor project. The token is never written here;
// it is read from the CLI's login (or CONDUCTOR_TOKEN) each time the mirror runs.
func initMirror(dir, project string) error {
	creds := client.LoadCredentials()
	if project == "" {
		project = creds.Project
	}
	if project == "" {
		return errors.New("-conductor needs a project: -conductor-project, CONDUCTOR_PROJECT, or `conductor login --project`")
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	st := mirrorState{Endpoint: creds.Endpoint, Project: project, Run: hex.EncodeToString(b), Tasks: map[string]mirrorTask{}}
	data, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(mirrorPath(dir), data, 0o600)
}

// loadMirror returns the objective's mirror, or nil when it has none.
func loadMirror(dir string) (*objMirror, error) {
	data, err := os.ReadFile(mirrorPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := &objMirror{path: mirrorPath(dir)}
	if err := json.Unmarshal(data, &m.st); err != nil {
		return nil, fmt.Errorf("%s: %w", m.path, err)
	}
	if m.st.Tasks == nil {
		m.st.Tasks = map[string]mirrorTask{}
	}
	creds := client.LoadCredentials()
	if creds.Token == "" {
		return nil, errors.New("the objective mirrors to Conductor, but no token is set (`conductor login`, or CONDUCTOR_TOKEN)")
	}
	m.c = client.New(m.st.Endpoint, creds.Token)
	return m, nil
}

func (m *objMirror) save() error {
	data, _ := json.MarshalIndent(m.st, "", "  ")
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

// sync mirrors every event recorded since the last sync. It stops at the first failure and
// resumes there next time; replaying an event is harmless (tasks are found by external
// ref, settling is idempotent), so a crash between a call and save costs nothing.
func (m *objMirror) sync(ctx context.Context, l *objective.Log) error {
	t := l.Tree()
	for _, e := range l.Events() {
		if e.Seq <= m.st.Seq {
			continue
		}
		if err := m.apply(ctx, t, e); err != nil {
			return fmt.Errorf("mirroring event %d (%s %s) to Conductor: %w", e.Seq, e.Node, e.Type, err)
		}
		m.st.Seq = e.Seq
		if err := m.save(); err != nil {
			return err
		}
	}
	return nil
}

func (m *objMirror) apply(ctx context.Context, t *objective.Tree, e objective.Event) error {
	node := e.Node
	if e.Type == objective.EvInit {
		node = t.Root
	}
	n := t.Get(node)
	if n == nil {
		return fmt.Errorf("unknown node %q", node)
	}
	if err := m.ensureTask(ctx, t, n); err != nil {
		return err
	}
	ev := func(typ string, payload map[string]any) error {
		payload["node"] = n.ID
		return m.timeline(ctx, n.ID, typ, payload)
	}
	switch e.Type {
	case objective.EvInit:
		p := map[string]any{"budget_usd": n.BudgetUSD}
		if n.DeadlineAt != "" {
			p["deadline_at"] = n.DeadlineAt
		}
		return ev("objective.created", p)
	case objective.EvRouted:
		var d objective.RoutedData
		_ = json.Unmarshal(e.Data, &d)
		return ev("objective.routed", map[string]any{"chain": d.Chain, "basis": d.Basis, "family": n.Family})
	case objective.EvDelegated:
		var a objective.Attempt
		_ = json.Unmarshal(e.Data, &a)
		return ev("objective.delegated", map[string]any{"attempt": len(n.Attempts), "target": a.Target})
	case objective.EvAnswered:
		var d objective.AnsweredData
		_ = json.Unmarshal(e.Data, &d)
		p := map[string]any{"status": d.Status, "answer_sha256": d.AnswerSHA, "tokens": d.Tokens, "cost_usd": d.CostUSD}
		if d.Outcome != "" {
			p["outcome"] = category(d.Outcome)
		}
		if d.Branch != "" { // a coding agent's work: where it is, never what it says
			p["branch"], p["commit_sha"], p["changed_paths"] = d.Branch, d.Commit, d.Changed
		}
		return ev("objective.answered", p)
	case objective.EvDecomposed:
		var refs []string
		for _, c := range n.Children {
			child := t.Get(c)
			if err := m.ensureTask(ctx, t, child); err != nil {
				return err
			}
			refs = append(refs, m.st.Tasks[c].Ref)
		}
		return ev("objective.decomposed", map[string]any{"children": refs, "count": len(refs)})
	case objective.EvChecked:
		var c objective.Check
		_ = json.Unmarshal(e.Data, &c)
		verdict := "reject"
		if c.Passed {
			verdict = "accept"
		}
		// The reviewer's feedback is model output and stays in objective.jsonl.
		return ev("objective.checked", map[string]any{"attempt": c.Attempt + 1, "target": c.By, "verdict": verdict, "cost_usd": c.CostUSD})
	case objective.EvReviewer:
		var d objective.ReviewerData
		_ = json.Unmarshal(e.Data, &d)
		return ev("objective.reviewer", map[string]any{"target": d.Target})
	case objective.EvSynthesis:
		return ev("objective.synthesized", map[string]any{"count": len(n.Children)})
	case objective.EvGrant:
		var g objective.Grant
		_ = json.Unmarshal(e.Data, &g)
		return ev("objective.granted", map[string]any{"from": g.From, "amount_usd": g.AmountUSD, "budget_usd": n.BudgetUSD})
	case objective.EvAccepted, objective.EvFailed, objective.EvAbandoned:
		var d objective.ReasonData
		_ = json.Unmarshal(e.Data, &d)
		status := map[string]string{objective.EvAccepted: "done", objective.EvFailed: "failed", objective.EvAbandoned: "cancelled"}[e.Type]
		reason := publicReason(e.Type, d.Reason)
		if err := ev("objective.settled", map[string]any{"outcome": e.Type, "reason": reason, "spent_usd": n.SpentUSD, "tokens": n.SpentTokens}); err != nil {
			return err
		}
		return m.c.Post(ctx, "/v1/tasks/"+url.PathEscape(m.st.Tasks[n.ID].ID)+"/settle", map[string]any{"status": status, "reason": reason}, nil)
	}
	return nil
}

// ensureTask files the node's task once. The external ref is unique per project, so a
// replay after a crash finds the task instead of filing a second.
func (m *objMirror) ensureTask(ctx context.Context, t *objective.Tree, n *objective.Node) error {
	if _, ok := m.st.Tasks[n.ID]; ok {
		return nil
	}
	ext := "delphi-objective:" + m.st.Run + ":" + n.ID
	var crit []domain.AcceptanceCriterion
	for _, c := range n.Criteria {
		crit = append(crit, domain.AcceptanceCriterion{Text: c})
	}
	body := map[string]any{"title": n.Statement, "objective": fmt.Sprintf("delphi objective %s, node %s (%s)", m.st.Run, n.ID, n.Kind),
		"external_ref": ext, "labels": []string{objectiveLabel, "delphi-objective-" + m.st.Run},
		"acceptance_criteria": crit, "status": "proposed"}
	if n.Parent != "" {
		parent, ok := m.st.Tasks[n.Parent]
		if !ok {
			if err := m.ensureTask(ctx, t, t.Get(n.Parent)); err != nil {
				return err
			}
			parent = m.st.Tasks[n.Parent]
		}
		body["parent"] = parent.ID
	}
	var got mirrorTask
	err := m.c.Post(ctx, "/v1/projects/"+url.PathEscape(m.st.Project)+"/tasks", body, &got)
	var ae *client.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		var list struct {
			Tasks []mirrorTask `json:"tasks"`
		}
		if err := m.c.Get(ctx, "/v1/projects/"+url.PathEscape(m.st.Project)+"/tasks?external_ref="+url.QueryEscape(ext), &list); err != nil {
			return err
		}
		if len(list.Tasks) != 1 {
			return fmt.Errorf("task %s exists but was not found by its external ref", ext)
		}
		got, err = list.Tasks[0], nil
	}
	if err != nil {
		return err
	}
	m.st.Tasks[n.ID] = got
	return m.save()
}

func (m *objMirror) timeline(ctx context.Context, node, typ string, payload map[string]any) error {
	return m.c.Post(ctx, "/v1/tasks/"+url.PathEscape(m.st.Tasks[node].ID)+"/timeline", map[string]any{"type": typ, "payload": payload}, nil)
}

// category is the outcome's kind without any text a model wrote.
func category(outcome string) string {
	head, _, _ := strings.Cut(outcome, ":")
	head, _, _ = strings.Cut(head, " (")
	return strings.TrimSpace(head)
}

// publicReason says why a node settled without quoting a model: a rejection names who
// rejected it, not what they said.
func publicReason(typ, reason string) string {
	if typ == objective.EvAbandoned || typ == objective.EvAccepted {
		return reason // the runner's own words
	}
	for _, k := range []string{"deadline", "budget exhausted", "no independent reviewer", "review failed", "no model for family"} {
		if strings.Contains(reason, k) {
			head, _, _ := strings.Cut(reason, ":")
			if k == "deadline" || k == "budget exhausted" {
				head, _, _ = strings.Cut(reason, " and ")
			}
			return strings.TrimSpace(head)
		}
	}
	if child, _, ok := strings.Cut(reason, " failed:"); ok && !strings.Contains(child, " ") {
		return "child " + child + " failed"
	}
	if head, _, ok := strings.Cut(reason, "; the last:"); ok {
		return head
	}
	return "failed"
}
