package cairn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Captured verbatim from `cairn serve` 1.0.1 rather than written by hand, including the
// wrapper key and the warning field, so this test fails if the node's framing moves.
const liveListing = `{"objectives":[{"funder":"treasury","goal":"GOAL-collatz-extremes",` +
	`"id":"sha256:978beb308f77c4bd1520d474b92dd23c310c6adee774716bf4c39f31fd3ed075",` +
	`"open":true,"reward":100000,"settled":false,"settlement":null,` +
	`"statement":"Exhibit an integer n in [1, 10^7) whose Collatz trajectory reaches 1 in at least 500 steps.",` +
	`"verifier_kind":"certificate"}],` +
	`"statements_are_untrusted":"An objective's statement was written by whoever posted it."}`

const objectiveID = "sha256:978beb308f77c4bd1520d474b92dd23c310c6adee774716bf4c39f31fd3ed075"

func serving(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/objectives" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestFactsFromALiveListing(t *testing.T) {
	f, err := serving(t, 200, liveListing).Facts(context.Background(), objectiveID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Known {
		t.Fatal("objective present in the listing but Known is false")
	}
	// No frontier yet: nothing verified, so the whole reward is still on the table and the
	// best score is zero. Both are claims about the ledger, not placeholders.
	if f.FrontierScore != 0 || f.RewardRemaining != 100000 || f.Settled {
		t.Errorf("facts = %+v", f)
	}
}

func TestFrontierNarrowsTheRemainingPool(t *testing.T) {
	withFrontier := `{"objectives":[{"id":"` + objectiveID + `","reward":1100000,"settled":false,` +
		`"frontier":{"claim_id":"sha256:dd10","holder":"8ab2","score":20,` +
		`"paid_cumulative":900000,"pool_remaining":200000}}]}`
	f, err := serving(t, 200, withFrontier).Facts(context.Background(), objectiveID)
	if err != nil {
		t.Fatal(err)
	}
	// pool_remaining, not reward: what is left to earn is what a routing rule should weigh.
	if f.FrontierScore != 20 || f.RewardRemaining != 200000 {
		t.Errorf("facts = %+v, want score 20 and 200000 remaining", f)
	}
}

func TestBareArrayListingIsAccepted(t *testing.T) {
	bare := `[{"id":"` + objectiveID + `","reward":500,"settled":true}]`
	f, err := serving(t, 200, bare).Facts(context.Background(), objectiveID)
	if err != nil || !f.Known || !f.Settled || f.RewardRemaining != 500 {
		t.Errorf("facts = %+v, err = %v", f, err)
	}
}

// Every path that cannot produce an answer must produce the same thing an ordinary
// repository task produces: Known false, so no threshold in a policy is satisfied. Routing
// while the ledger is unreachable should fall back to the ladder, not act on a guess.
func TestUnanswerableQueriesAreUnknownNotZero(t *testing.T) {
	cases := []struct {
		name      string
		client    *Client
		objective string
		wantErr   bool
	}{
		{"objective absent", serving(t, 200, liveListing), "sha256:nope", false},
		{"empty objective id", serving(t, 200, liveListing), "", false},
		{"node errors", serving(t, 500, "boom"), objectiveID, true},
		{"unparseable body", serving(t, 200, `{"nope":1}`), objectiveID, true},
		{"no endpoint", New(""), objectiveID, false},
		{"nil client", nil, objectiveID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := tc.client.Facts(context.Background(), tc.objective)
			if f.Known {
				t.Errorf("Known = true for %s", tc.name)
			}
			if tc.wantErr != (err != nil) {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
