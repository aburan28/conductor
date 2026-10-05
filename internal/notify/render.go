package notify

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/adamburan/conductor/internal/domain"
)

// Message is the body of a generic webhook request, and what the Slack and Discord renderings
// are made from.
//
// It is built from an event after the API's visibility projection has been applied for an
// ordinary project member (coord.ProjectEvents), and then narrowed further for a private
// task: a channel is project-wide, and a Slack channel or a webhook's receiver is outside
// Conductor's membership checks altogether, so nothing goes out that a member who is not the
// task's owner could not see — and for private work, less than that.
type Message struct {
	// ID is the event's id. A receiver deduplicates by it: delivery is at least once.
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Project    string    `json:"project"`
	OccurredAt time.Time `json:"occurred_at"`
	// Subject is the task the event is about ("T-42"), "a private task", or empty.
	Subject string `json:"subject,omitempty"`
	Private bool   `json:"private,omitempty"`
	// Text is a one-line human summary.
	Text string `json:"text"`
	// URL links to the dashboard when conductord knows its public address.
	URL string `json:"url,omitempty"`
	// Data is the event's payload as a project observer sees it.
	Data map[string]any `json:"data"`
	Test bool           `json:"test,omitempty"`
}

// privateKeys are the payload keys kept for an event about a private task: what kind of thing
// happened, never where or to what. The API shows a private task's ref and reserved territory
// to project members so they can avoid it; a notification is not how anyone avoids anything,
// and goes to places membership does not reach, so it carries neither.
var privateKeys = map[string]bool{
	"status": true, "from": true, "to": true, "state": true, "phase": true, "outcome": true,
	"severity": true, "kind": true, "count": true, "percent_hint": true, "principal": true,
	"granted": true, "expired": true, "redacted": true,
}

// buildMessage renders a projected event.
func buildMessage(project string, e domain.Event, dashboard string) Message {
	private := e.Visibility == domain.VisibilityPrivate
	data := map[string]any{}
	for k, v := range e.Payload {
		if !private || privateKeys[k] {
			data[k] = v
		}
	}
	if private {
		data["redacted"] = true
	}
	m := Message{
		ID: e.ID, Type: e.Type, Project: project, OccurredAt: e.OccurredAt.UTC(),
		Private: private, Data: data,
	}
	ref, _ := data["task_ref"].(string)
	switch {
	case private:
		m.Subject = "a private task"
	case ref != "":
		m.Subject = ref
	}
	m.Text = summarize(e.Type, m.Subject, data)
	m.URL = dashboardLink(dashboard, project, ref, private)
	return m
}

func dashboardLink(base, project, ref string, private bool) string {
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	if ref != "" && !private {
		return base + "/tasks/" + url.PathEscape(ref) + "?project=" + url.QueryEscape(project)
	}
	return base + "/?project=" + url.QueryEscape(project)
}

func str(data map[string]any, key string) string {
	switch v := data[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func strList(data map[string]any, key string) []string {
	switch v := data[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func num(data map[string]any, key string) (float64, bool) {
	switch v := data[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// summarize is the one-line text for an event. It reads only the narrowed payload, so it can
// say no more than the payload does.
func summarize(typ, subject string, d map[string]any) string {
	task := subject
	if task == "" {
		task = "a task"
	}
	with := func(s, key, format string) string {
		if v := str(d, key); v != "" {
			return s + fmt.Sprintf(format, v)
		}
		return s
	}
	switch typ {
	case "task.status_changed":
		s := fmt.Sprintf("%s is now %s", task, orDash(str(d, "to")))
		s = with(s, "from", " (was %s)")
		return with(s, "reason", ": %s")
	case "scope.released":
		s := "Territory "
		if who := str(d, "principal"); who != "" {
			s = "@" + who + ": territory "
		}
		if res := strList(d, "resources"); len(res) > 0 {
			s += "you were waiting for is free (" + strings.Join(res, ", ") + ")"
		} else {
			s += "you were waiting for is free"
		}
		return s + ", released by " + task + ". Check again before you start."
	case "attempt.stalled":
		s := "An attempt on " + task + " has stalled"
		s = with(s, "harness", " (%s)")
		return with(s, "reason", ": %s")
	case "lease.expired":
		return with("The lease on "+task+" expired", "status", "; the task is now %s")
	case "github.pr_merged":
		return with("Pull request for "+task+" merged", "branch", " (%s)")
	case "github.pr_closed":
		return with("Pull request for "+task+" closed without merging", "branch", " (%s)")
	case "github.pr_checked":
		s := with("Checked a pull request", "branch", " on %s")
		return with(s, "outcome", ": %s")
	case "task.pull_request_linked":
		return "A pull request was linked to " + task
	case "task.claimed":
		return with(task+" was claimed", "harness", " (%s)")
	case "task.released":
		return with(task+" was released", "status", "; it is now %s")
	case "task.handoff":
		return with(task+" was handed off", "harness", " to %s")
	case "task.blocked":
		return task + " is waiting on a dependency"
	case "task.unblocked":
		return task + "'s dependencies are met; it is ready"
	case "task.assigned":
		return with(task+" was offered to a session", "harness", " (%s)")
	case "attempt.evidence":
		return "Validation evidence was submitted for " + task
	case "budget.downshift", "budget.exhausted":
		s := "Project budget: downshift threshold crossed; routing moves to cheaper models"
		if typ == "budget.exhausted" {
			s = "Project budget: pause threshold reached; new work is held"
		}
		if v, ok := num(d, "cost_usd"); ok {
			s += fmt.Sprintf(" ($%.2f spent in the last 30 days)", v)
		}
		return s
	case "budget.shared":
		s := "Token allowance shared"
		if from, to := str(d, "from"), str(d, "to"); from != "" && to != "" {
			s = fmt.Sprintf("%s shared token allowance with %s", from, to)
		}
		return s
	case "quota.warning", "quota.exhausted":
		s := "A teammate's "
		if h := str(d, "harness"); h != "" {
			s += h + " "
		}
		if typ == "quota.exhausted" {
			s += "login hit its usage limit"
		} else {
			s += "login is near its usage limit"
		}
		var detail []string
		if w := str(d, "kind"); w != "" {
			detail = append(detail, w+" window")
		}
		if p, ok := num(d, "percent_hint"); ok {
			detail = append(detail, fmt.Sprintf("%d%% used", int(math.Round(p))))
		}
		if r := str(d, "expires_at"); r != "" {
			if t, err := time.Parse(time.RFC3339Nano, r); err == nil {
				detail = append(detail, "resets "+t.UTC().Format("15:04 MST Jan 2"))
			}
		}
		if len(detail) > 0 {
			s += " (" + strings.Join(detail, ", ") + ")"
		}
		return s
	}
	if subject != "" {
		return typ + " on " + subject
	}
	return typ
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// testMessage is what POST …/notifications/{id}/test sends.
func testMessage(project string, dashboard string, now time.Time) Message {
	return Message{
		ID: "test-" + fmt.Sprint(now.UnixNano()), Type: "notification.test", Project: project,
		OccurredAt: now.UTC(), Test: true,
		Text: "Test notification from Conductor: this channel is connected.",
		URL:  dashboardLink(dashboard, project, "", false),
		Data: map[string]any{},
	}
}

// ---------------------------------------------------------------------------
// Per-kind bodies
// ---------------------------------------------------------------------------

// slackEscape escapes the three characters Slack's mrkdwn treats as markup. Escaping < and >
// is also what keeps event text from forming a link or a <!channel> mention.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackBody renders a Slack incoming-webhook message in Block Kit, with plain text as the
// notification fallback.
func slackBody(m Message) ([]byte, error) {
	title := "*" + slackEscape(m.Project) + "* · " + slackEscape(m.Type)
	section := map[string]any{"type": "section", "text": map[string]any{
		"type": "mrkdwn", "text": slackEscape(m.Text)}}
	context := []any{map[string]any{"type": "mrkdwn", "text": title + " · " +
		"<!date^" + fmt.Sprint(m.OccurredAt.Unix()) + "^{date_short_pretty} {time}|" +
		m.OccurredAt.Format(time.RFC3339) + ">"}}
	if m.URL != "" {
		context = append(context, map[string]any{"type": "mrkdwn",
			"text": "<" + slackEscape(m.URL) + "|Open in Conductor>"})
	}
	return json.Marshal(map[string]any{
		"text":   "[" + m.Project + "] " + m.Text,
		"blocks": []any{section, map[string]any{"type": "context", "elements": context}},
	})
}

// discordBody renders a Discord webhook message. allowed_mentions is empty so nothing in the
// text can ping @everyone or a role.
func discordBody(m Message) ([]byte, error) {
	content := "**" + m.Project + "** · " + m.Text
	if m.URL != "" {
		content += "\n<" + m.URL + ">"
	}
	content = truncate(content, 1900)
	return json.Marshal(map[string]any{
		"content":          content,
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
}
