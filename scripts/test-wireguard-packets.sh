#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib/wireguard-test.sh"

wg_test_init
wg_test_overlay
phase=authorized-packets
wg_test_ping a 10.88.0.1
wg_test_ping a 10.88.0.3
wg_test_ping b 10.88.0.2
handshakes=$(docker_local exec "$name-gateway" wg show wg0 latest-handshakes | awk '$2>0 {n++} END {print n+0}')
[[ "$handshakes" == 2 ]]
wg_test_note 'PASS: 2 authenticated gateway handshakes and 12/12 initial packets'

phase=unapproved-key
wg_test_node rogue
wg_test_apply rogue worker-b 10.88.0.4
underlay=$(docker_local inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name-gateway")
docker_local exec "$name-rogue" ping -c 1 -W 2 "$underlay" >/dev/null
wg_test_denied rogue 10.88.0.1
rogue_handshake=$(docker_local exec "$name-rogue" wg show wg0 latest-handshakes | awk '{print $2}')
[[ "$rogue_handshake" == 0 ]]
wg_test_ping a 10.88.0.1
wg_test_note 'PASS: unapproved private key cannot handshake or send overlay traffic; approved worker remains connected'

phase=selective-revocation
docker_local exec "$name-gateway" wg set wg0 peer "$(cat "$fixture/b.pub")" remove
wg_test_denied a 10.88.0.3
wg_test_denied b 10.88.0.1
wg_test_ping a 10.88.0.1
wg_test_note 'PASS: removed peer is isolated while another approved peer still reaches the gateway'

phase=reapply-approved-configuration
wg_test_apply gateway gateway 10.88.0.1
wg_test_reset_worker b worker-b 10.88.0.3
wg_test_ping b 10.88.0.1
wg_test_ping a 10.88.0.3
wg_test_ping b 10.88.0.2
wg_test_note 'PASS: explicit gateway reapply plus affected worker peer reset/reapply restores the withdrawn peer'

phase=gateway-outage
docker_local stop --time 1 "$name-gateway" >/dev/null
wg_test_denied a 10.88.0.1
wg_test_denied b 10.88.0.2
wg_test_note 'PASS: gateway outage removes gateway and routed worker reachability'

phase=gateway-recovery
docker_local start "$name-gateway" >/dev/null
wg_test_tun gateway
wg_test_apply gateway gateway 10.88.0.1
wg_test_reset_worker a worker-a 10.88.0.2
wg_test_reset_worker b worker-b 10.88.0.3
wg_test_ping a 10.88.0.1
wg_test_ping b 10.88.0.1
wg_test_ping a 10.88.0.3
wg_test_ping b 10.88.0.2
wg_test_note 'PASS: gateway restart and explicit gateway/worker peer reset/reapply restore bidirectional routing'
wg_test_note 'WIREGUARD_LIFECYCLE_PASS: authorized traffic, unapproved-key denial, selective withdrawal, configuration reapply, gateway outage and recovery'
