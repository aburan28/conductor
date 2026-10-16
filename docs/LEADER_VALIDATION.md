# Leader / RDS / NAT validation

Validated on 2026-10-07 with Go 1.25.14, based on commit
`3ff2fe45094978c3844a20138f89daf4e06ed011`.

| Command | Exit | Result |
| --- | ---: | --- |
| `go test -race ./internal/nat ./cmd/conductord ./cmd/conductor ./internal/db ./internal/config -count=1` | 0 | Affected packages pass; Postgres-backed tests skip without a database. |
| `go test $(go list ./... \| rg -v '/internal/localstate$') -count=1` | 0 | All packages other than the independently reproduced process-state failure pass without a database. |
| `make check` | 2 | Formatting, vet, staticcheck and builds pass. Unit tests fail only at `internal/localstate.TestStopAndContinueGroup`. |
| `go -C /tmp/conductor-baseline test ./internal/localstate -run '^TestStopAndContinueGroup$' -count=1 -v` | 1 | The same timeout occurs on unchanged base commit, in a detached worktree. No localstate code was changed. |
| `make ci` | 2 | Formatting, vet, staticcheck, vulnerability scan and builds pass. Full run stops because it cannot start Postgres and Docker is unavailable. |
| `git diff --check` | 0 | No whitespace errors. |

The vulnerability scan reported no vulnerabilities. The changed packages also pass
the race detector. Tests cover verified RDS trust and hostname checks, multi-host
fallbacks, credential-safe error reporting, external database startup without Docker,
remote worker startup without a daemon, graceful termination, authentication behind
proxies, discovered SSO callback URLs, and configuration precedence.

NAT tests use injectable gateway and Tailscale-command adapters. They exercise mapping
creation, finite lease renewal, cleanup, changed ownership, conflicting routes,
carrier-grade NAT rejection, bounded shutdown, HTTPS proxy confirmation, and preservation
of another Tailscale service. These tests do not establish live Internet reachability.

Live Postgres migrations, an actual RDS connection, real UPnP router behavior, and
signed-in Tailscale connectivity remain unverified here. The configured leader does not
provide automatic leader election or local database failover. RDS IAM token refresh is
not implemented; password authentication uses the existing Postgres driver.

The repository task check was attempted before editing and returned 127 because the
CLI was unavailable. After building the CLI, the same scoped check returned 1 because
the local control plane could not be contacted. No active task card or claim was
available. Changes were kept on an isolated feature branch under the user's requested
offline implementation scope; no task metadata or terminal process dumps are published.

The database integration suite and live connectivity checks should run in an appropriate
deployment environment before this draft is promoted for merge. See [deployment
instructions](LEADER.md).
