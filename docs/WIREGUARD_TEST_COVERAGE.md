# Private-network tests and coverage

This follow-up tests the existing product source from `8b9e99a68229aa4d5b3d42b45237dcfed72869de`.
It adds tests, test fixtures, and evidence; it does not add network or authorization features.

## Measured Go coverage

These are statement counts from `go test -coverprofile`, not a claim about all
Conductor behavior. The baseline uses the existing tests; the final column adds
the follow-up tests against the same product source.

| Scope | Baseline | Follow-up |
| --- | --- | --- |
| `internal/wireguard` | 84/91, 92.3% | 90/91, 98.9% |
| `cmd/conductor-mesh-dns` | 127/212, 59.9% | 137/212, 64.6% |
| `cmd/conductor/wireguard.go` | 34/43, 79.1% | 39/43, 90.7% |
| Entire `cmd/conductor` package | 1721/9213, 18.7% | 1726/9213, 18.7% |

`cmdWireGuard` reaches 100%; `renderWireGuardFiles` reaches 86.7%. DNS response,
question-name, configuration, and service-map parsing functions reach 100% in
this profile. DNS `main` and Tailscale subprocess functions remain at 0% in the
unit profile. Separate native CLI/DNS subprocess executions and the Docker
acceptance runs are uninstrumented and are not merged into these percentages.
The Linux-only real write-failure rollback test lives in
`wireguard_linux_test.go`; it introduces no runtime skip.

Reproduce the measurement:

```sh
go test -count=1 -coverprofile=private-network.cover ./internal/wireguard ./cmd/conductor-mesh-dns ./cmd/conductor
go tool cover -func=private-network.cover
```

The [coverage record](wireguard-validation/coverage-followup.txt) records the
commands, exact statement counts, function coverage, environment, and scope.

## Requirement-to-test matrix

| Requirement | Actual test/assertion | Execution gate |
| --- | --- | --- |
| Strict public inventory and safe routes | Reader failures, exact/oversized manifest bounds, multiple/unknown YAML fields, canonical keys, endpoint/node/subnet/listen-port boundaries, no default routes | Normal `make check` / `make ci` |
| Private file ownership and overwrite safety | Existing files/directories/symlinks preserved, private modes, missing/unreadable inputs, missing parents, actual CLI argument failures and offline rendering | Normal `make check` / `make ci` |
| Partial-output rollback | A child-only Linux file-size limit forces a real write failure; output directory is removed | Normal Linux Go tests; explicit Linux test file |
| Robust DNS core | Every truncated query prefix, malformed/count/type cases, IPv4/IPv6/mapped-address families, hostname and service-map boundaries | Normal `make check` / `make ci` |
| Explicit offline DNS daemon | Real daemon startup, PATH Tailscale tripwire, actual UDP SRV/A/AAAA queries, bounded readiness, process reaping and socket release | Normal `make check` / `make ci`; child execution uninstrumented |
| Approved and unapproved WireGuard identities | Two approved handshakes and real packets; rogue underlay reaches the gateway, but its unapproved key has zero handshake and cannot send overlay packets while an approved peer still works | Opt-in packet lifecycle gate |
| Selective network revocation | Explicit removal isolates worker B in both relevant paths; worker A still reaches the gateway | Opt-in packet lifecycle gate |
| Manual reapply and gateway recovery | `wg syncconf`, explicit worker peer reset and outbound initiation; actual gateway container stop/restart; bidirectional routes return | Opt-in packet lifecycle gate; no automatic-convergence claim |
| Actual application traffic over the overlay | Native `conductord` + fresh Postgres, API bound only to `10.88.0.1:8443`, two worker identities, trusted local CA, untrusted CA refusal, reachable underlay TCP port explicitly refused | Opt-in native-team gate |
| Native CLI and native API behavior | Actual CLI task listing contains the fixture task for Alice and Bob; native-client/API driver checks claim200, conflict409, contributor403, valid same-org nonmember404, missing-token401 | Opt-in native-team gate |
| Claim authority survives refusals | Fresh task/reservation reads preserve original owner, epoch, attempt count, fence and active reservation authority bindings; saved claim lease has not expired (persisted lease expiry is not reread) | Opt-in native-team gate |
| Token and membership revocation | Disposable token works then is refused401 after DELETE204; original remains valid. Project removal preserves side-project authentication but target becomes404; final membership removal invalidates token401 | Opt-in native-team gate |
| App revocation differs from network withdrawal | WireGuard packets still work after app revocation; later explicit peer removal blocks B's packets while A still reaches the application | Opt-in native-team gate |
| Shutdown and cleanup | SIGTERM exits native daemon and closes its listener while overlay still works; all owned containers/network/private fixture files are removed, including failure paths | Both opt-in gates |

The normal Go checks run the added unit and native DNS/CLI subprocess tests.
They compile/vet the native API driver, but **do not run the Docker packet or
team acceptance gates**. No new CI workflow is claimed for those opt-in gates.

## Reproduce the opt-in acceptance gates

Use an approved prebuilt WireGuard test image and PostgreSQL 17 image. The
scripts inspect images locally and use `--pull=never`; they never install or
download a replacement. Build the three test executables explicitly first:

```sh
CGO_ENABLED=0 go build -o ./bin/ ./cmd/conductor ./cmd/conductord ./scripts/wireguard-e2e
bash scripts/test-wireguard-packets.sh
bash scripts/test-wireguard-conductor.sh
```

`CGO_ENABLED=0` makes copied fixture executables independent of the test image's
libc. Native binaries can also be selected with `WG_TEST_CONDUCTOR`,
`WG_TEST_CONDUCTORD`, and `WG_TEST_API_DRIVER`. Image selectors are `WG_TEST_IMAGE`
and `WG_TEST_POSTGRES_IMAGE`. The team fixture also needs preinstalled `openssl`
on the development host to generate its temporary CA.

These Linux tests use only the local Docker daemon, an isolated `--internal`
bridge, NET_ADMIN and the TUN device class. They publish no application/database
ports and mount no host filesystem into the containers. WireGuard private keys
are generated and consumed within their owning nodes. Ephemeral bearer tokens,
TLS keys and bootstrap output stay in owner-only fixture files and are removed;
the evidence contains status labels and public packet output. Set
`WG_TEST_EVIDENCE_DIR` to retain the public result summary at a chosen path.

All task/member/token API requests are issued from workers to the fixed overlay
HTTPS origin with the native client's verified custom-CA transport. Bootstrap
administration is local to the database-capable gateway. The real CLI checks
are labeled separately from the native-client/API driver; no curl/mock service
is substituted for the application.

## Observed runs and recovery limits

The following raw attempts are preserved without editing away failures:

| Attempt | Result | Evidence |
| --- | --- | --- |
| Packet 1 | Approved traffic, rogue denial and selective removal passed; immediate gateway-only reapply failed to restore routed worker traffic | [log](wireguard-validation/followup-packet-attempt-1.log) |
| Packet 2 | Adding affected-worker reset alone did not restore the immediate routed path | [log](wireguard-validation/followup-packet-attempt-2.log) |
| Packet 3 | A worker-to-gateway initiation succeeded, but destructive `wg setconf` reapply still left another worker path unavailable | [log](wireguard-validation/followup-packet-attempt-3.log) |
| Packet 4 | `wg syncconf` plus explicit affected-worker reset/outbound initiation passed selective recovery, actual gateway outage/restart, bidirectional routes and cleanup | [log](wireguard-validation/followup-packet-attempt-4.log) |
| Packet 5 | Final repeat adds positive rogue underlay reachability before overlay denial; all lifecycle checks and cleanup passed | [log](wireguard-validation/followup-packet-attempt-5.log) |
| Native team 1 | Real overlay/TLS/native CLI/API authorization and revocation scenarios passed; listener shutdown and cleanup passed | [log](wireguard-validation/followup-team-attempt-1.log) |
| Native team 2 | Repeated with explicit native process-exit assertion; all team, TLS, revocation, shutdown and cleanup checks passed | [log](wireguard-validation/followup-team-attempt-2.log) |
| Native team 3 | Final repeat verifies the actual CLI output contains the fixture task for both identities; all team, TLS, revocation, shutdown and cleanup checks passed | [log](wireguard-validation/followup-team-attempt-3.log) |

`make check` and `make ci` both passed against the follow-up Go source. The latter
ran race checks and the existing native collaboration/mTLS E2E suite using a fresh
Postgres fixture. The final opt-in packet and native-team runs above passed
separately. An early invalid evidence-directory failure also confirmed temporary
fixture cleanup before any Docker resource or secret is created.

The tested manual recovery procedure uses `wg syncconf` to preserve unaffected
peer state. A withdrawn or restarted peer is reset/reapplied explicitly and
initiates traffic toward the gateway so its endpoint is learned again. The
successful run does not establish immediate gateway-only recovery or automatic
convergence. Earlier unsuccessful one-sided/destructive reapply attempts remain
part of the evidence.

The packet provider in these runs is Linux `wireguard-go`, not a kernel-module
validation. DNS is UDP-only; TCP service is not implemented or positively
tested. Internet NAT traversal, customer firewall/workspace isolation, external
identity providers, automatic key rotation/revocation distribution, gateway
failover, IPv6 overlays, exit routing, performance and production readiness remain
outside these assertions. Passing tests and high coverage of selected functions
do not establish those capabilities.
