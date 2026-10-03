package cairn

import (
	"context"
	"os"
	"testing"
)

// Opt-in live check against a running node: CAIRN_LIVE=http://127.0.0.1:8099 go test ./internal/cairn/
func TestLiveNode(t *testing.T) {
	endpoint := os.Getenv("CAIRN_LIVE")
	if endpoint == "" {
		t.Skip("set CAIRN_LIVE to a running cairn node to run this")
	}
	id := os.Getenv("CAIRN_LIVE_OBJECTIVE")
	c := New(endpoint)
	f, err := c.Facts(context.Background(), id)
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	t.Logf("known=%v score=%d reward_remaining=%d settled=%v", f.Known, f.FrontierScore, f.RewardRemaining, f.Settled)
	if !f.Known {
		t.Fatalf("live node did not know objective %q", id)
	}
	if miss, err := c.Facts(context.Background(), "sha256:"+"00"); err != nil || miss.Known {
		t.Errorf("unknown objective: known=%v err=%v; want known=false, no error", miss.Known, err)
	}
	if down, err := New("http://127.0.0.1:9").Facts(context.Background(), id); err == nil || down.Known {
		t.Errorf("unreachable node: known=%v err=%v; want known=false with an error", down.Known, err)
	}
}
