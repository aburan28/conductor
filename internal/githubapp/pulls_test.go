package githubapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pullsServer is a GitHub with one pull request, served the way GitHub serves it: an open list
// with an ETag, commits whose messages may carry a trailer, and one GraphQL endpoint.
func pullsServer(t *testing.T, graphql func(query string, vars map[string]any) any, commits []string, etag *atomic.Value, lists *atomic.Int64) *httptest.Server {
	t.Helper()
	m := http.NewServeMux()
	m.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs", "expires_at": time.Now().Add(time.Hour)})
	})
	m.HandleFunc("GET /repos/acme/w/pulls", func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		cur := etag.Load().(string)
		if r.Header.Get("If-None-Match") == cur {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", cur)
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number": 5, "state": "open", "title": "Retry backoff", "html_url": "https://github.example/acme/w/pull/5",
			"body": "Fixes T-9", "draft": false, "created_at": "2026-10-01T10:00:00Z",
			"user": map[string]any{"login": "octo", "id": 1234},
			"head": map[string]any{"ref": "feature/retry", "sha": "abc", "repo": map[string]any{"full_name": "acme/w"}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/w"}},
		}})
	})
	m.HandleFunc("GET /repos/acme/w/pulls/5/commits", func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]any
		for _, msg := range commits {
			out = append(out, map[string]any{"commit": map[string]any{"message": msg}})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	m.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		_ = json.NewEncoder(w).Encode(graphql(req.Query, req.Variables))
	})
	return httptest.NewServer(m)
}

func pullsClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	_, pemKey := testKey(t)
	c, err := New(srv.URL, Credentials{AppID: 1, PrivateKeyPEM: pemKey})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// One GraphQL call carries the review decision, the unresolved threads, mergeability, the
// size, and the check rollup — check runs and commit statuses together.
func TestPullRequestRollupIsOneGraphQLCall(t *testing.T) {
	var etag atomic.Value
	etag.Store(`"a"`)
	var lists, graphqlCalls atomic.Int64
	srv := pullsServer(t, func(query string, vars map[string]any) any {
		graphqlCalls.Add(1)
		for _, want := range []string{"reviewDecision", "reviewThreads", "mergeStateStatus", "statusCheckRollup", "additions"} {
			if !strings.Contains(query, want) {
				t.Errorf("the query does not ask for %s", want)
			}
		}
		// The query must not request text a stranger wrote.
		for _, banned := range []string{"bodyText", "body ", "comments", "diff", "message", "patch"} {
			if strings.Contains(query, banned) {
				t.Errorf("the query asks for %q, which Conductor never stores", banned)
			}
		}
		if vars["owner"] != "acme" || vars["name"] != "w" || vars["number"].(float64) != 5 {
			t.Errorf("variables = %v", vars)
		}
		return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewDecision": "CHANGES_REQUESTED", "mergeable": "MERGEABLE", "mergeStateStatus": "BEHIND",
			"additions": 120, "deletions": 30, "changedFiles": 5,
			"reviewThreads": map[string]any{"totalCount": 3, "nodes": []map[string]any{
				{"isResolved": false}, {"isResolved": true}, {"isResolved": false}}},
			"commits": map[string]any{"nodes": []map[string]any{{"commit": map[string]any{"statusCheckRollup": map[string]any{
				"contexts": map[string]any{"totalCount": 7, "nodes": []map[string]any{
					{"__typename": "CheckRun", "status": "COMPLETED", "conclusion": "SUCCESS"},
					{"__typename": "CheckRun", "status": "COMPLETED", "conclusion": "SKIPPED"},
					{"__typename": "CheckRun", "status": "COMPLETED", "conclusion": "FAILURE"},
					{"__typename": "CheckRun", "status": "IN_PROGRESS", "conclusion": ""},
					{"__typename": "StatusContext", "state": "SUCCESS"},
					{"__typename": "StatusContext", "state": "ERROR"},
				}}}}}}},
		}}}}
	}, nil, &etag, &lists)
	defer srv.Close()
	c := pullsClient(t, srv)

	got, err := c.PullRequestRollup(context.Background(), 7, "acme", "w", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := Rollup{ReviewDecision: "changes_requested", UnresolvedThreads: 2, Mergeable: "behind",
		// Six contexts were read of seven: the unread one counts as pending, so the parts sum
		// to the whole.
		ChecksTotal: 7, ChecksPassed: 3, ChecksFailed: 2, ChecksPending: 2,
		Additions: 120, Deletions: 30, ChangedFiles: 5}
	if got != want {
		t.Errorf("rollup = %+v\nwant     %+v", got, want)
	}
	if graphqlCalls.Load() != 1 {
		t.Errorf("%d GraphQL calls, want 1", graphqlCalls.Load())
	}
}

func TestParseRollupMapsGitHubVocabulary(t *testing.T) {
	mk := func(decision, mergeable, state string) rollupResponse {
		t.Helper()
		var d any
		if decision != "" {
			d = decision
		}
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewDecision": d, "mergeable": mergeable, "mergeStateStatus": state}}}})
		var r rollupResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, c := range []struct{ decision, mergeable, state, wantDecision, wantMergeable string }{
		{"APPROVED", "MERGEABLE", "CLEAN", "approved", "clean"},
		{"REVIEW_REQUIRED", "MERGEABLE", "BLOCKED", "review_required", "blocked"},
		{"", "CONFLICTING", "DIRTY", "", "dirty"},
		{"", "MERGEABLE", "UNSTABLE", "", "clean"},
		// A GitHub that predates mergeStateStatus, or a draft: fall back to mergeable.
		{"", "CONFLICTING", "", "", "dirty"},
		{"", "MERGEABLE", "DRAFT", "", "clean"},
		{"", "UNKNOWN", "UNKNOWN", "", "unknown"},
	} {
		got, err := parseRollup(mk(c.decision, c.mergeable, c.state))
		if err != nil {
			t.Fatal(err)
		}
		if got.ReviewDecision != c.wantDecision || got.Mergeable != c.wantMergeable {
			t.Errorf("%q %s %s -> decision %q mergeable %q, want %q %q", c.decision, c.mergeable, c.state,
				got.ReviewDecision, got.Mergeable, c.wantDecision, c.wantMergeable)
		}
	}
}

func TestPullRequestRollupReportsGraphQLErrors(t *testing.T) {
	var etag atomic.Value
	etag.Store(`"a"`)
	var lists atomic.Int64
	srv := pullsServer(t, func(string, map[string]any) any {
		return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}},
			"errors": []map[string]any{{"message": "Resource not accessible by integration"}}}
	}, nil, &etag, &lists)
	defer srv.Close()
	_, err := pullsClient(t, srv).PullRequestRollup(context.Background(), 7, "acme", "w", 5)
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Errorf("err = %v; a permission gap must say what GitHub said", err)
	}
}

// GitHub Enterprise serves GraphQL beside its REST API, not under it.
func TestGraphQLURL(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":                "https://api.github.com/graphql",
		"https://api.github.com/":               "https://api.github.com/graphql",
		"https://ghe.example.com/api/v3":        "https://ghe.example.com/api/graphql",
		"http://127.0.0.1:5000":                 "http://127.0.0.1:5000/graphql",
		"https://ghe.example.com/api/v3/":       "https://ghe.example.com/api/graphql",
		"https://ghe.example.com/prefix/api/v3": "https://ghe.example.com/prefix/api/graphql",
	} {
		c := &Client{API: api}
		if got := c.graphqlURL(); got != want {
			t.Errorf("graphqlURL(%s) = %s, want %s", api, got, want)
		}
	}
}

// An unchanged open list costs a 304, which GitHub does not count against the rate limit.
func TestOpenPullRequestsIfChanged(t *testing.T) {
	var etag atomic.Value
	etag.Store(`"v1"`)
	var lists atomic.Int64
	srv := pullsServer(t, nil, nil, &etag, &lists)
	defer srv.Close()
	c := pullsClient(t, srv)
	ctx := context.Background()

	first, err := c.OpenPullRequestsIfChanged(ctx, 7, "acme", "w", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.NotModified || first.ETag != `"v1"` || len(first.Pulls) != 1 {
		t.Fatalf("first = %+v", first)
	}
	p := first.Pulls[0]
	if p.Title != "Retry backoff" || p.User.ID != 1234 || p.User.Login != "octo" || p.Body != "Fixes T-9" ||
		p.Repository() != "acme/w" || p.CreatedAt.IsZero() || p.Base.Ref != "main" || p.FromFork() {
		t.Errorf("pull request = %+v", p)
	}
	again, err := c.OpenPullRequestsIfChanged(ctx, 7, "acme", "w", first.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified || len(again.Pulls) != 0 || again.ETag != `"v1"` {
		t.Fatalf("second = %+v; want a 304", again)
	}
	etag.Store(`"v2"`)
	third, err := c.OpenPullRequestsIfChanged(ctx, 7, "acme", "w", first.ETag)
	if err != nil || third.NotModified || third.ETag != `"v2"` || len(third.Pulls) != 1 {
		t.Fatalf("third = %+v, %v", third, err)
	}
}

// The trailer the runner writes links a pull request to its task; other text is ignored and
// nothing about the messages is returned.
func TestConductorTaskTrailer(t *testing.T) {
	var etag atomic.Value
	etag.Store(`"a"`)
	var lists atomic.Int64
	for _, c := range []struct {
		name    string
		commits []string
		want    string
	}{
		{"none", []string{"fix retry", "more work"}, ""},
		{"a trailer", []string{"fix retry", "wire it\n\nConductor-Task: T-42\nCo-Authored-By: x"}, "T-42"},
		{"case and spacing", []string{"subject\n\nconductor-task:   t-7  "}, "T-7"},
		{"only a trailer counts", []string{"subject mentions Conductor-Task: T-9 inline"}, ""},
		{"the first commit that has one", []string{"a\n\nConductor-Task: T-1", "b\n\nConductor-Task: T-2"}, "T-1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := pullsServer(t, nil, c.commits, &etag, &lists)
			defer srv.Close()
			got, err := pullsClient(t, srv).ConductorTaskTrailer(context.Background(), 7, "acme", "w", 5)
			if err != nil || got != c.want {
				t.Errorf("trailer = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestPullRequestRepositoryFallsBackToTheURL(t *testing.T) {
	pr := PullRequest{HTMLURL: "https://ghe.example.com/acme/widgets/pull/12"}
	if got := pr.Repository(); got != "acme/widgets" {
		t.Errorf("Repository() = %q", got)
	}
	if got := (PullRequest{}).Repository(); got != "" {
		t.Errorf("Repository() of nothing = %q", got)
	}
}
