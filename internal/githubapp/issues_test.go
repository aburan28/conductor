package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// issuesServer answers an installation token and an issues listing whose ETag changes only
// when the listing does.
func issuesServer(t *testing.T, etag *atomic.Value, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	m := http.NewServeMux()
	m.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs", "expires_at": time.Now().Add(time.Hour)})
	})
	m.HandleFunc("GET /repos/acme/w/issues", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cur := etag.Load().(string)
		if r.Header.Get("If-None-Match") == cur {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.URL.Query().Get("direction") != "asc" || r.URL.Query().Get("sort") != "updated" {
			t.Errorf("listing is not oldest-change-first: %s", r.URL.RawQuery)
		}
		w.Header().Set("ETag", cur)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 1, "title": "one", "state": "open", "updated_at": "2026-01-01T00:00:00Z"},
			{"number": 2, "title": "a pull request", "state": "open", "pull_request": map[string]any{"url": "x"}},
		})
	})
	return httptest.NewServer(m)
}

func TestListIssuesUsesConditionalRequests(t *testing.T) {
	_, pemKey := testKey(t)
	var etag atomic.Value
	etag.Store(`"v1"`)
	var calls atomic.Int64
	srv := issuesServer(t, &etag, &calls)
	defer srv.Close()
	c, err := New(srv.URL, Credentials{AppID: 1, PrivateKeyPEM: pemKey})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := c.ListIssues(ctx, 7, "acme", "w", IssueQuery{MaxPages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Issues) != 2 || first.ETag != `"v1"` || first.NotModified || !first.Issues[1].IsPullRequest() || first.Issues[0].IsPullRequest() {
		t.Fatalf("first listing = %+v", first)
	}
	again, err := c.ListIssues(ctx, 7, "acme", "w", IssueQuery{ETag: first.ETag})
	if err != nil || !again.NotModified || len(again.Issues) != 0 || again.ETag != `"v1"` {
		t.Fatalf("unchanged listing = %+v, %v", again, err)
	}
	etag.Store(`"v2"`)
	changed, err := c.ListIssues(ctx, 7, "acme", "w", IssueQuery{ETag: first.ETag})
	if err != nil || changed.NotModified || changed.ETag != `"v2"` {
		t.Fatalf("changed listing = %+v, %v", changed, err)
	}
}

// A secondary rate limit stops every call until GitHub's wait is over, without calling it.
func TestRateLimitBacksOff(t *testing.T) {
	_, pemKey := testKey(t)
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/limited":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, _ := New(srv.URL, Credentials{AppID: 1, PrivateKeyPEM: pemKey})
	c.Now = func() time.Time { return now }
	ctx := context.Background()

	err := c.do(ctx, http.MethodGet, "/forbidden", "t", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.Forbidden() || apiErr.RateLimited() {
		t.Fatalf("a permission 403 = %v", err)
	}
	err = c.do(ctx, http.MethodGet, "/limited", "t", nil, nil)
	if !errors.As(err, &apiErr) || !apiErr.RateLimited() || apiErr.RetryAfter != 2*time.Minute {
		t.Fatalf("a secondary limit = %v (%+v)", err, apiErr)
	}
	before := calls.Load()
	if err := c.do(ctx, http.MethodGet, "/ok", "t", nil, nil); !errors.As(err, &apiErr) || !apiErr.RateLimited() {
		t.Fatalf("a call during the backoff = %v", err)
	}
	if calls.Load() != before {
		t.Error("a call during the backoff reached GitHub")
	}
	now = now.Add(121 * time.Second)
	if err := c.do(ctx, http.MethodGet, "/ok", "t", nil, nil); err != nil {
		t.Fatalf("after the backoff: %v", err)
	}
}

func TestIssueKeys(t *testing.T) {
	key := IssueKey("acme", "widgets", 12)
	o, r, n, ok := ParseIssueKey(key)
	if !ok || o != "acme" || r != "widgets" || n != 12 {
		t.Errorf("ParseIssueKey(%q) = %q %q %d %v", key, o, r, n, ok)
	}
	for _, bad := range []string{"acme/widgets", "acme/widgets#0", "../x#1", "acme/widgets#12/../1", "a/b#" + strconv.Itoa(1<<40)} {
		if _, _, _, ok := ParseIssueKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	if u, ok := IssueURL("", key); !ok || u != "https://github.com/acme/widgets/issues/12" {
		t.Errorf("IssueURL = %q", u)
	}
	if !Grants(map[string]string{"issues": "write"}, "issues", "read") || Grants(map[string]string{"issues": "read"}, "issues", "write") ||
		Grants(nil, "issues", "read") {
		t.Error("Grants")
	}
}
