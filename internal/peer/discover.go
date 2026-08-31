package peer

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// ResolveSRV turns a DNS SRV record set (RFC 2782) into peer candidates: one per target,
// under a provisional name (the DNS target itself) that a successful probe replaces with
// whatever identity the peer's own mesh certificate and /v1/peer/info report — the mesh
// never trusts a name DNS handed it, only one backed by the shared CA.
//
// name is looked up directly rather than built from a service/proto pair, so it works
// against an SRV record published under any name, not only the canonical
// "_service._proto.name" a bare service/proto pair would construct.
func ResolveSRV(ctx context.Context, r SRVResolver, name string) ([]Peer, error) {
	if r == nil {
		r = net.DefaultResolver
	}
	_, records, err := r.LookupSRV(ctx, "", "", name)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", name, err)
	}
	peers := make([]Peer, 0, len(records))
	for _, rec := range records {
		target := strings.TrimSuffix(rec.Target, ".")
		if target == "" {
			continue
		}
		peers = append(peers, Peer{
			Name: target,
			URL:  fmt.Sprintf("https://%s:%d", target, rec.Port),
		})
	}
	return peers, nil
}
