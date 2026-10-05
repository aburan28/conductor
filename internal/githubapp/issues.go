package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The Issues half of the client: what issue sync (internal/tracker, api/github_issues.go)
// reads and writes. Every call goes through an installation token and needs the installation
// to have accepted the issues permission; a 403 that is not a rate limit (APIError.Forbidden)
// means it has not.

// Issue is the subset of an issue Conductor reads.
type Issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`        // open, closed
	StateReason string    `json:"state_reason"` // completed, not_planned, reopened, or empty
	HTMLURL     string    `json:"html_url"`
	UpdatedAt   time.Time `json:"updated_at"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is present when the "issue" is a pull request: GitHub's issues endpoints
	// list both.
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}

// IsPullRequest reports whether the issues endpoint returned a pull request.
func (i Issue) IsPullRequest() bool { return len(i.PullRequest) > 0 && string(i.PullRequest) != "null" }

// LabelNames lists the issue's labels.
func (i Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// IssueKey is how an issue is named in a task's external_ref: owner/repo#number.
func IssueKey(owner, repo string, number int) string {
	return owner + "/" + repo + "#" + strconv.Itoa(number)
}

var issueKey = regexp.MustCompile(`^([A-Za-z0-9._-]{1,100})/([A-Za-z0-9._-]{1,100})#([1-9][0-9]{0,9})$`)

// ParseIssueKey reads an IssueKey back.
func ParseIssueKey(key string) (owner, repo string, number int, ok bool) {
	m := issueKey.FindStringSubmatch(key)
	if m == nil || !ValidRepo(m[1], m[2]) {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, false
	}
	return m[1], m[2], n, true
}

// IssueURL is the web address of the issue an IssueKey names.
func IssueURL(web, key string) (string, bool) {
	owner, repo, n, ok := ParseIssueKey(key)
	if !ok {
		return "", false
	}
	if web == "" {
		web = DefaultWeb
	}
	return fmt.Sprintf("%s/%s/%s/issues/%d", strings.TrimRight(web, "/"), owner, repo, n), true
}

// IssueQuery bounds one listing.
type IssueQuery struct {
	// Since lists issues, open or closed, updated at or after it. Zero lists open issues only:
	// a first import has no use for issues that closed before it.
	Since time.Time
	// ETag is the one the previous identical listing returned. GitHub answers an unchanged
	// listing with 304, which does not count against the rate limit.
	ETag string
	// MaxPages bounds the listing to MaxPages×100 issues; the caller resumes from the newest
	// updated_at it was given.
	MaxPages int
}

// IssueListing is the result of ListIssues.
type IssueListing struct {
	Issues      []Issue
	ETag        string // of the first page, for the next identical listing
	NotModified bool   // nothing changed since the listing ETag was for
	Truncated   bool   // MaxPages were read and more remain
}

// ListIssues lists a repository's issues (pull requests included, as GitHub returns them;
// IsPullRequest tells them apart), least recently updated first, so a bounded listing is a
// contiguous stretch the next one continues from.
func (c *Client) ListIssues(ctx context.Context, installationID int64, owner, repo string, q IssueQuery) (IssueListing, error) {
	var out IssueListing
	base, err := repoPath(owner, repo)
	if err != nil {
		return out, err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return out, err
	}
	if q.MaxPages <= 0 {
		q.MaxPages = 1
	}
	params := url.Values{"sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}, "state": {"open"}}
	if !q.Since.IsZero() {
		params.Set("state", "all")
		params.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	for page := 1; page <= q.MaxPages; page++ {
		params.Set("page", strconv.Itoa(page))
		k := call{method: http.MethodGet, path: base + "/issues?" + params.Encode(), bearer: tok}
		if page == 1 && q.ETag != "" {
			k.header = http.Header{"If-None-Match": {q.ETag}}
		}
		var batch []Issue
		k.out = &batch
		status, hdr, err := c.send(ctx, k)
		if err != nil {
			return out, err
		}
		if page == 1 {
			if status == http.StatusNotModified {
				out.ETag, out.NotModified = q.ETag, true
				return out, nil
			}
			out.ETag = hdr.Get("ETag")
		}
		out.Issues = append(out.Issues, batch...)
		if len(batch) < 100 {
			return out, nil
		}
	}
	out.Truncated = true
	return out, nil
}

// GetIssue reads one issue.
func (c *Client) GetIssue(ctx context.Context, installationID int64, owner, repo string, number int) (Issue, error) {
	var out Issue
	base, err := repoPath(owner, repo)
	if err != nil {
		return out, err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return out, err
	}
	err = c.do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", base, number), tok, nil, &out)
	return out, err
}

// CommentOnIssue adds a comment to an issue (or pull request).
func (c *Client) CommentOnIssue(ctx context.Context, installationID int64, owner, repo string, number int, body string) error {
	base, err := repoPath(owner, repo)
	if err != nil {
		return err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", base, number), tok,
		map[string]string{"body": truncate(body, 60000)}, nil)
}

// AddIssueLabel adds a label to an issue. GitHub creates a label the repository lacks.
func (c *Client) AddIssueLabel(ctx context.Context, installationID int64, owner, repo string, number int, label string) error {
	base, err := repoPath(owner, repo)
	if err != nil {
		return err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/labels", base, number), tok,
		map[string][]string{"labels": {label}}, nil)
}

// RemoveIssueLabel removes a label from an issue. A label that is not there (removed by a
// person, say) is not an error: the outcome is the one asked for.
func (c *Client) RemoveIssueLabel(ctx context.Context, installationID int64, owner, repo string, number int, label string) error {
	base, err := repoPath(owner, repo)
	if err != nil {
		return err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/issues/%d/labels/%s", base, number, url.PathEscape(label)), tok, nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.NotFound() {
		return nil
	}
	return err
}

// CloseIssue closes an issue as completed.
func (c *Client) CloseIssue(ctx context.Context, installationID int64, owner, repo string, number int) error {
	base, err := repoPath(owner, repo)
	if err != nil {
		return err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", base, number), tok,
		map[string]string{"state": "closed", "state_reason": "completed"}, nil)
}
