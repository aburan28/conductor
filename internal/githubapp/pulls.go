package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Reading pull requests for the pull-request list (docs/COLLABORATION.md §4.5).
//
// Three calls cover it. The open list, as a conditional request, says which pull requests
// exist and which changed. One GraphQL query per pull request that needs it carries
// everything the REST API would need three calls for: the review decision, the unresolved
// review threads, mergeability, and the check rollup. The commits list is read only to find
// a Conductor-Task trailer.
//
// Nothing here returns or keeps text a stranger wrote beyond the title: the body is read
// for a task reference and dropped, review and comment text is never requested, and commit
// messages are scanned for the trailer and dropped.

// Repository returns the pull request's repository as owner/name, as GitHub spells it.
func (pr PullRequest) Repository() string {
	if pr.Base.Repo.FullName != "" {
		return pr.Base.Repo.FullName
	}
	if pr.HTMLURL != "" {
		rest := pr.HTMLURL
		if i := strings.Index(rest, "://"); i >= 0 {
			rest = rest[i+3:]
		}
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) >= 5 && parts[len(parts)-2] == "pull" {
			return parts[len(parts)-4] + "/" + parts[len(parts)-3]
		}
	}
	return ""
}

// OpenListing is the result of a conditional listing of open pull requests.
type OpenListing struct {
	Pulls []PullRequest
	// ETag identifies this listing; pass it back to learn whether anything changed.
	ETag string
	// NotModified is true when GitHub answered 304 to the ETag passed in: nothing about the
	// open pull requests changed, and Pulls is empty. A 304 does not count against the rate
	// limit.
	NotModified bool
}

// OpenPullRequestsIfChanged lists open pull requests like OpenPullRequests, as a conditional
// request when etag is not empty.
func (c *Client) OpenPullRequestsIfChanged(ctx context.Context, installationID int64, owner, repo, etag string) (OpenListing, error) {
	var out OpenListing
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return out, err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return out, err
	}
	k := call{method: http.MethodGet, path: base + "/pulls?state=open&per_page=100&sort=updated&direction=desc", bearer: tok, out: &out.Pulls}
	if etag != "" {
		k.header = http.Header{"If-None-Match": {etag}}
	}
	status, hdr, err := c.send(ctx, k)
	if err != nil {
		return out, err
	}
	if status == http.StatusNotModified {
		out.ETag, out.NotModified = etag, true
		return out, nil
	}
	out.ETag = hdr.Get("ETag")
	return out, nil
}

// trailerRE matches the trailer the runner writes on its commits.
var trailerRE = regexp.MustCompile(`(?mi)^\s*Conductor-Task:\s*(T-\d+)\s*$`)

// maxCommitPages bounds the commits read for a trailer: GitHub lists at most 250 commits of a
// pull request anyway.
const maxCommitPages = 3

// ConductorTaskTrailer returns the task named by a Conductor-Task trailer on any commit of
// the pull request, or "" when none does. Commit messages are scanned and dropped.
func (c *Client) ConductorTaskTrailer(ctx context.Context, installationID int64, owner, repo string, number int) (string, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return "", err
	}
	base, err := repoPath(owner, repo)
	if err != nil {
		return "", err
	}
	for page := 1; page <= maxCommitPages; page++ {
		var batch []struct {
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		}
		p := fmt.Sprintf("%s/pulls/%d/commits?per_page=100&page=%d", base, number, page)
		if err := c.do(ctx, http.MethodGet, p, tok, nil, &batch); err != nil {
			return "", err
		}
		for _, cm := range batch {
			if m := trailerRE.FindStringSubmatch(cm.Commit.Message); m != nil {
				return strings.ToUpper(m[1]), nil
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	return "", nil
}

// Rollup is what the checks and review APIs say about one pull request right now.
type Rollup struct {
	ChecksTotal       int
	ChecksPassed      int
	ChecksFailed      int
	ChecksPending     int
	ReviewDecision    string // '' | approved | changes_requested | review_required
	UnresolvedThreads int
	Mergeable         string // '' | clean | dirty | blocked | behind | unknown
	// The size of the change.
	Additions, Deletions, ChangedFiles int
}

// rollupQuery asks for the four things in one call. Review threads are counted from their
// resolved flag (the connection takes no isResolved argument); the first hundred threads
// bound the count, which is far past where anyone counts. Contexts are check runs and commit
// statuses together, as the pull request's merge box shows them.
const rollupQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      reviewDecision
      additions deletions changedFiles
      mergeable
      mergeStateStatus
      reviewThreads(first:100){ totalCount nodes{ isResolved } }
      commits(last:1){ nodes{ commit{ statusCheckRollup{
        contexts(first:100){ totalCount nodes{
          __typename
          ... on CheckRun { status conclusion }
          ... on StatusContext { state }
        } }
      } } } }
    }
  }
}`

type rollupResponse struct {
	Data struct {
		Repository struct {
			PullRequest *struct {
				ReviewDecision   *string `json:"reviewDecision"`
				Additions        int     `json:"additions"`
				Deletions        int     `json:"deletions"`
				ChangedFiles     int     `json:"changedFiles"`
				Mergeable        string  `json:"mergeable"`
				MergeStateStatus string  `json:"mergeStateStatus"`
				ReviewThreads    struct {
					TotalCount int `json:"totalCount"`
					Nodes      []struct {
						IsResolved bool `json:"isResolved"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								Contexts struct {
									TotalCount int `json:"totalCount"`
									Nodes      []struct {
										Typename   string `json:"__typename"`
										Status     string `json:"status"`
										Conclusion string `json:"conclusion"`
										State      string `json:"state"`
									} `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// graphqlURL is where GraphQL is served: api.github.com/graphql, or, on GitHub Enterprise
// (whose REST API lives under /api/v3), /api/graphql beside it.
func (c *Client) graphqlURL() string {
	api := strings.TrimRight(c.API, "/")
	if strings.HasSuffix(api, "/api/v3") {
		return strings.TrimSuffix(api, "/v3") + "/graphql"
	}
	return api + "/graphql"
}

// PullRequestRollup reads a pull request's review decision, unresolved thread count,
// mergeability, and check rollup in one GraphQL call.
func (c *Client) PullRequestRollup(ctx context.Context, installationID int64, owner, repo string, number int) (Rollup, error) {
	var out Rollup
	if _, err := repoPath(owner, repo); err != nil {
		return out, err
	}
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return out, err
	}
	var resp rollupResponse
	body := map[string]any{"query": rollupQuery, "variables": map[string]any{"owner": owner, "name": repo, "number": number}}
	if _, _, err := c.send(ctx, call{method: http.MethodPost, url: c.graphqlURL(), bearer: tok, body: body, out: &resp}); err != nil {
		return out, err
	}
	return parseRollup(resp)
}

func parseRollup(resp rollupResponse) (Rollup, error) {
	var out Rollup
	pr := resp.Data.Repository.PullRequest
	if pr == nil {
		if len(resp.Errors) > 0 {
			return out, errors.New("github graphql: " + resp.Errors[0].Message)
		}
		return out, errors.New("github graphql: no such pull request")
	}
	out.Additions, out.Deletions, out.ChangedFiles = pr.Additions, pr.Deletions, pr.ChangedFiles
	switch deref(pr.ReviewDecision) {
	case "APPROVED":
		out.ReviewDecision = "approved"
	case "CHANGES_REQUESTED":
		out.ReviewDecision = "changes_requested"
	case "REVIEW_REQUIRED":
		out.ReviewDecision = "review_required"
	}
	for _, n := range pr.ReviewThreads.Nodes {
		if !n.IsResolved {
			out.UnresolvedThreads++
		}
	}
	switch pr.MergeStateStatus {
	case "CLEAN", "UNSTABLE", "HAS_HOOKS":
		out.Mergeable = "clean"
	case "DIRTY":
		out.Mergeable = "dirty"
	case "BEHIND":
		out.Mergeable = "behind"
	case "BLOCKED":
		out.Mergeable = "blocked"
	default:
		// DRAFT, UNKNOWN, or a GitHub that predates mergeStateStatus: fall back to mergeable.
		switch pr.Mergeable {
		case "CONFLICTING":
			out.Mergeable = "dirty"
		case "MERGEABLE":
			out.Mergeable = "clean"
		default:
			out.Mergeable = "unknown"
		}
	}
	if len(pr.Commits.Nodes) > 0 && pr.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		ctxs := pr.Commits.Nodes[0].Commit.StatusCheckRollup.Contexts
		for _, n := range ctxs.Nodes {
			switch n.Typename {
			case "CheckRun":
				switch {
				case n.Status != "COMPLETED":
					out.ChecksPending++
				case n.Conclusion == "SUCCESS" || n.Conclusion == "NEUTRAL" || n.Conclusion == "SKIPPED":
					out.ChecksPassed++
				default:
					out.ChecksFailed++
				}
			case "StatusContext":
				switch n.State {
				case "SUCCESS":
					out.ChecksPassed++
				case "FAILURE", "ERROR":
					out.ChecksFailed++
				default:
					out.ChecksPending++
				}
			}
		}
		out.ChecksTotal = ctxs.TotalCount
		// Contexts past the first hundred are counted but not read: they are pending until
		// proven otherwise, which keeps the parts summing to the whole.
		if seen := out.ChecksPassed + out.ChecksFailed + out.ChecksPending; out.ChecksTotal > seen {
			out.ChecksPending += out.ChecksTotal - seen
		} else {
			out.ChecksTotal = seen
		}
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
