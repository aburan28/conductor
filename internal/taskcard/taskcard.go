// Package taskcard renders and parses the portable Markdown task card of DESIGN.md §21.
//
// Task cards are a materialized view and a handoff artifact, never the concurrency source of
// truth (§20.1, §34.4). Git synchronization is far too slow and conflict-prone to hold a live
// claim; the ledger owns state, and the card is how that state travels — into a worktree for
// an agent to read, into a PR for a human to review, into another tool during a handoff.
package taskcard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adamburan/conductor/internal/domain"
)

// Card is the frontmatter of a task card.
type Card struct {
	ID           string      `yaml:"id"`
	Project      string      `yaml:"project"`
	Parent       string      `yaml:"parent,omitempty"`
	ExternalRef  string      `yaml:"external_ref,omitempty"`
	Status       string      `yaml:"status"`
	Visibility   string      `yaml:"visibility"`
	Owner        string      `yaml:"owner"`
	Executor     string      `yaml:"executor,omitempty"`
	Lease        *CardLease  `yaml:"lease,omitempty"`
	BaseSHA      string      `yaml:"base_sha,omitempty"`
	Branch       string      `yaml:"branch,omitempty"`
	Worktree     string      `yaml:"worktree,omitempty"`
	WorkflowSHA  string      `yaml:"workflow_sha,omitempty"`
	Route        *CardRoute  `yaml:"route,omitempty"`
	Budget       *CardBudget `yaml:"budget,omitempty"`
	Scopes       []CardScope `yaml:"scopes,omitempty"`
	Dependencies []string    `yaml:"dependencies,omitempty"`
	Checks       []string    `yaml:"checks,omitempty"`

	// Body sections.
	Title              string   `yaml:"-"`
	Objective          string   `yaml:"-"`
	AcceptanceCriteria []string `yaml:"-"`
	CurrentProgress    string   `yaml:"-"`
	Decisions          []string `yaml:"-"`
	OpenQuestions      []string `yaml:"-"`
	Artifacts          []string `yaml:"-"`
}

type CardLease struct {
	ID           string    `yaml:"id"`
	FencingEpoch int64     `yaml:"fencing_epoch"`
	ExpiresAt    time.Time `yaml:"expires_at"`
}

type CardRoute struct {
	Role            string `yaml:"role"`
	Harness         string `yaml:"harness"`
	ModelAlias      string `yaml:"model_alias"`
	ResolvedModel   string `yaml:"resolved_model,omitempty"`
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`
}

type CardBudget struct {
	Class       string `yaml:"class,omitempty"`
	MaxAttempts int    `yaml:"max_attempts,omitempty"`
}

type CardScope struct {
	Resource string `yaml:"resource"`
	Mode     string `yaml:"mode"`
}

// PrivacyNote is appended to every rendered card. It is documentation for the humans and
// agents who read cards, stating plainly what the file does and does not contain.
const PrivacyNote = "This card contains coordination state only. It does not contain the " +
	"originating chat transcript or hidden model reasoning."

// FromTask builds a card from ledger state.
func FromTask(
	task domain.Task,
	projectSlug, ownerHandle string,
	attempt *domain.Attempt,
	lease *domain.Lease,
	reservations []domain.ScopeReservation,
	dependencies []string,
	checks []string,
) Card {
	c := Card{
		ID:           task.Ref,
		Project:      projectSlug,
		ExternalRef:  task.ExternalRef,
		Status:       string(task.Status),
		Visibility:   string(task.Visibility),
		Owner:        "user:" + ownerHandle,
		BaseSHA:      task.BaseSHA,
		WorkflowSHA:  task.WorkflowSHA,
		Dependencies: dependencies,
		Checks:       checks,
		Title:        task.Title,
		Objective:    task.Objective,
	}
	if task.MaxAttempts > 0 {
		c.Budget = &CardBudget{Class: task.Budget.Class, MaxAttempts: task.MaxAttempts}
	}
	for _, criterion := range task.AcceptanceCriteria {
		text := criterion.Text
		if criterion.Status != "" && criterion.Status != "pending" {
			text += " — " + criterion.Status
		}
		c.AcceptanceCriteria = append(c.AcceptanceCriteria, text)
	}
	for _, r := range reservations {
		c.Scopes = append(c.Scopes, CardScope{Resource: r.Resource(), Mode: string(r.Mode)})
	}
	sort.Slice(c.Scopes, func(i, j int) bool { return c.Scopes[i].Resource < c.Scopes[j].Resource })

	if attempt != nil {
		c.Branch = attempt.Branch
		c.Worktree = attempt.WorktreePath
		c.Executor = fmt.Sprintf("agent:%s/attempt-%d", attempt.Harness, attempt.AttemptNumber)
		c.Route = &CardRoute{
			Role:            string(attempt.Role),
			Harness:         attempt.Harness,
			ModelAlias:      attempt.ModelAlias,
			ResolvedModel:   attempt.ResolvedModel,
			ReasoningEffort: string(attempt.ReasoningEffort),
		}
		if attempt.BaseCommitSHA != "" {
			c.BaseSHA = attempt.BaseCommitSHA
		}
		if attempt.CommitSHA != "" {
			c.Artifacts = append(c.Artifacts, "Commit: "+attempt.CommitSHA)
		}
	}
	if lease != nil {
		c.Lease = &CardLease{
			ID: lease.ID, FencingEpoch: lease.FencingEpoch, ExpiresAt: lease.ExpiresAt.UTC(),
		}
	}
	if len(c.Artifacts) == 0 {
		c.Artifacts = []string{"Commit: not yet published"}
	}
	return c
}

// Render writes the card as Markdown with YAML frontmatter.
func (c Card) Render() (string, error) {
	front, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(front)
	b.WriteString("---\n\n")

	title := c.Title
	if title == "" {
		title = c.ID
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	if c.Objective != "" {
		fmt.Fprintf(&b, "## Objective\n\n%s\n\n", c.Objective)
	}
	writeList(&b, "Acceptance criteria", c.AcceptanceCriteria)
	if c.CurrentProgress != "" {
		fmt.Fprintf(&b, "## Current progress\n\n%s\n\n", c.CurrentProgress)
	}
	writeList(&b, "Decisions", c.Decisions)
	writeList(&b, "Open questions", c.OpenQuestions)
	writeList(&b, "Artifacts", c.Artifacts)

	fmt.Fprintf(&b, "## Privacy note\n\n%s\n", PrivacyNote)
	return b.String(), nil
}

func writeList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", heading)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
	b.WriteString("\n")
}

// Parse reads a task card back, so a card edited by hand or produced by another tool can
// re-enter the system (DESIGN.md §3.14: portable Markdown task cards).
func Parse(src string) (Card, error) {
	var c Card

	body := src
	if strings.HasPrefix(src, "---\n") {
		rest := src[4:]
		end := strings.Index(rest, "\n---")
		if end < 0 {
			return c, fmt.Errorf("task card: unterminated frontmatter")
		}
		if err := yaml.Unmarshal([]byte(rest[:end]), &c); err != nil {
			return c, fmt.Errorf("task card frontmatter: %w", err)
		}
		body = strings.TrimPrefix(rest[end+4:], "\n")
	}

	section := ""
	var buf []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(buf, "\n"))
		items := bulletItems(buf)
		switch strings.ToLower(section) {
		case "objective":
			c.Objective = text
		case "acceptance criteria":
			c.AcceptanceCriteria = items
		case "current progress":
			c.CurrentProgress = text
		case "decisions":
			c.Decisions = items
		case "open questions":
			c.OpenQuestions = items
		case "artifacts":
			c.Artifacts = items
		}
		buf = nil
	}

	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "# "):
			flush()
			section = ""
			if c.Title == "" {
				c.Title = strings.TrimSpace(line[2:])
			}
		case strings.HasPrefix(line, "## "):
			flush()
			section = strings.TrimSpace(line[3:])
		default:
			buf = append(buf, line)
		}
	}
	flush()
	return c, nil
}

func bulletItems(lines []string) []string {
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- ") {
			out = append(out, strings.TrimSpace(trimmed[2:]))
		}
	}
	return out
}

// Instruction renders the agent-facing brief: the workflow rules, the task card, and any
// handoff bundle.
//
// This is the entire input an autonomous attempt receives. A new attempt after a failure, or
// a different harness after a handoff, gets this — never the previous session's conversation
// (DESIGN.md §13.6, §22).
func Instruction(card Card, workflow string, handoff *domain.HandoffBundle) (string, error) {
	rendered, err := card.Render()
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("You are executing a Conductor task. Follow the repository workflow ")
	b.WriteString("exactly, stay inside your reserved scope, and report scope expansion ")
	b.WriteString("before editing outside it.\n\n")

	if workflow != "" {
		b.WriteString("# Repository workflow\n\n")
		b.WriteString(workflow)
		b.WriteString("\n\n")
	}

	b.WriteString("# Task card\n\n")
	b.WriteString(rendered)
	b.WriteString("\n")

	if handoff != nil {
		b.WriteString("\n# Handoff from the previous attempt\n\n")
		b.WriteString("This is structured continuation state. You are not receiving the ")
		b.WriteString("previous session's conversation, and you should not ask for it.\n\n")

		if handoff.RecommendedNextAction != "" {
			fmt.Fprintf(&b, "**Next action:** %s\n\n", handoff.RecommendedNextAction)
		}
		writeBundleList(&b, "Completed", handoff.CompletedWork)
		writeBundleList(&b, "Assumptions", handoff.Assumptions)
		writeBundleList(&b, "Open questions", handoff.OpenQuestions)
		if len(handoff.Decisions) > 0 {
			b.WriteString("## Decisions\n\n")
			for _, d := range handoff.Decisions {
				fmt.Fprintf(&b, "- %s (%s)\n", d.Summary, d.By)
			}
			b.WriteString("\n")
		}
		if len(handoff.Blockers) > 0 {
			b.WriteString("## Blockers\n\n")
			for _, blocker := range handoff.Blockers {
				fmt.Fprintf(&b, "- [%s] %s\n", blocker.Kind, blocker.Summary)
			}
			b.WriteString("\n")
		}
		if len(handoff.Validation) > 0 {
			b.WriteString("## Validation so far\n\n")
			for _, v := range handoff.Validation {
				status := "passed"
				if v.ExitCode != 0 {
					status = fmt.Sprintf("failed (exit %d)", v.ExitCode)
				}
				fmt.Fprintf(&b, "- `%s` %s\n", v.Command, status)
			}
			b.WriteString("\n")
		}
	}

	if len(card.Checks) > 0 {
		b.WriteString("# Required checks\n\n")
		b.WriteString("Completion requires each of these to run and exit zero. ")
		b.WriteString("Reporting success without them will be rejected.\n\n")
		for _, check := range card.Checks {
			fmt.Fprintf(&b, "- `%s`\n", check)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func writeBundleList(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", heading)
	for _, item := range items {
		fmt.Fprintf(b, "- %s\n", item)
	}
	b.WriteString("\n")
}
