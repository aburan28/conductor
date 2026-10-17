# WireGuard routing validation

The [follow-up coverage and acceptance matrix](WIREGUARD_TEST_COVERAGE.md)
adds adversarial unit tests, real daemon DNS startup, gateway lifecycle checks,
and actual native Conductor team authorization over the generated overlay.
Its failed recovery attempts and successful manual procedure are preserved.

On 2026-10-17, the native `conductor wireguard render` output was exercised on
three Linux containers: one hub and two workers, each with its own locally
generated key. Their underlay was a Docker `--internal` network. No internet
endpoint or existing host route was used. The gateway had namespace-local IPv4
forwarding enabled. This is a functional routing check, not a throughput or
latency benchmark.

The host did not provide the kernel WireGuard device type. The successful run
used Debian's `wireguard-go` with a TUN device and `wireguard-tools`. The generated
public templates crossed into their containers; private keys stayed in each
container and were supplied through a pipe to `wg setconf`, never through shell
arguments or output logs. The test removes only its own containers, network, and
temporary files on exit.

| Attempt | Observed outcome | Evidence |
| --- | --- | --- |
| Kernel WireGuard | `ip link add ... type wireguard` returned `Unknown device type`; no kernel-path result established | [kernel-unavailable.log](wireguard-validation/kernel-unavailable.log) |
| Userspace, initial forwarding defaults | Both gateway handshakes and worker routes worked, but gateway ICMP redirects coincided with 50% loss in each two-packet worker sample; partial outcome despite the historical harness's success marker | [userspace-redirects-partial.log](wireguard-validation/userspace-redirects-partial.log) |
| Userspace, send/accept redirects disabled on all/default before interface creation | Two authenticated gateway handshakes, gateway reachability, bidirectional worker routing, 12/12 packets delivered | [userspace-no-redirects.log](wireguard-validation/userspace-no-redirects.log) |
| Final reusable harness, strict zero-loss assertions and explicit peer removal | Same two handshakes and 12/12 packets, then withdrawn gateway peer is unreachable; no automatic image pull and cleanup limited to its own resources | [userspace-final-harness.log](wireguard-validation/userspace-final-harness.log) |

The failed and partial attempts remain unchanged. Deployment instructions now
require appropriate redirect settings for a shared-interface hub; the final
reusable harness additionally requires zero loss, rather than accepting `ping`'s
exit status when only one packet succeeds.

## Reproduce on a Linux development host

Build `bin/conductor` normally. Build the test image through the approved package
source (or transfer a prebuilt approved image into an isolated environment):

```sh
docker build --secret id=proxy_ca,src=/etc/ssl/certs/ca-certificates.crt \
  -f deploy/wireguard-test.Dockerfile -t conductor-wireguard-test:local .
bash scripts/test-wireguard-packets.sh
```

The CA bundle is a build secret mount and is not copied into a layer. Preserve
your Docker proxy/registry settings. The test uses the local Docker daemon on
`/var/run/docker.sock`; it clears endpoint/context selectors to avoid running
network-admin tests on an unrelated remote daemon. Its containers have NET_ADMIN
and access only to the TUN character-device class, without privileged mode, host
networking, or host filesystem mounts. `WG_TEST_IMAGE` can select a preinstalled
test image and `WG_TEST_CONDUCTOR` an explicitly chosen executable.
The image must already exist: the harness inspects it locally and uses
`--pull=never`, so a missing image fails without a registry fallback.

This initial routing-only check establishes the generated hub/spoke configuration's userspace packet path
on this Linux test setup. It does not establish kernel-module compatibility,
customer firewall isolation, a production deployment, HA/failover, safe internet
exit routing, service authorization, automatic control-plane revocation propagation,
or key rotation. The follow-up separately tests bounded native application
authorization and token/member revocation; it does not generalize those results
to customer identity/network policies. The manual peer-removal check covers only the explicitly updated
gateway. Test the remaining lifecycle behavior
separately against the intended installation. ICMP timings in raw tool output
are not used to make any performance claim.
