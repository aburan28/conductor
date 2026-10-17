#!/usr/bin/env bash
# Shared opt-in Linux fixture. No private WireGuard key leaves its container.

docker_local() {
  env -u DOCKER_HOST -u DOCKER_CONTEXT -u DOCKER_TLS -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH \
    docker --host=unix:///var/run/docker.sock "$@"
}

wg_test_note() { printf '%s\n' "$*" | tee -a "$evidence/results.log"; }

wg_test_init() {
  umask 077
  repo=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
  conductor_binary=${WG_TEST_CONDUCTOR:-$repo/bin/conductor}
  test_image=${WG_TEST_IMAGE:-conductor-wireguard-test:local}
  [[ -x "$conductor_binary" ]] || { echo 'Build bin/conductor or set WG_TEST_CONDUCTOR first' >&2; return 1; }
  docker_local image inspect "$test_image" >/dev/null 2>&1 \
    || { echo 'Prebuild or preload the approved WireGuard test image; this test never pulls images' >&2; return 1; }
  fixture=$(mktemp -d /tmp/conductor-wg-fixture.XXXXXX)
  name="conductor-wg-${fixture##*.}"
  created_nodes=()
  network_created=0
  phase=initialization
  evidence=${WG_TEST_EVIDENCE_DIR:-}
  trap 'wg_test_finish "$?"' EXIT
  evidence=${evidence:-$(mktemp -d /tmp/conductor-wg-evidence.XXXXXX)}
  mkdir -p "$evidence"
  wg_test_note "Evidence: $evidence"
  docker_local network create --internal "$name" >/dev/null
  network_created=1
}

wg_test_tun() {
  docker_local exec "$name-$1" sh -c 'mkdir -p /dev/net; test -c /dev/net/tun || mknod /dev/net/tun c 10 200; chmod 600 /dev/net/tun; wireguard-go wg0'
}

wg_test_node() {
  local node=$1 container_id
  local flags=(--sysctl net.ipv4.conf.all.send_redirects=0 --sysctl net.ipv4.conf.default.send_redirects=0 --sysctl net.ipv4.conf.all.accept_redirects=0 --sysctl net.ipv4.conf.default.accept_redirects=0)
  [[ "$node" != gateway ]] || flags+=(--sysctl net.ipv4.ip_forward=1)
  container_id=$(docker_local create --pull=never --name "$name-$node" --network "$name" \
    --cap-add NET_ADMIN --device-cgroup-rule 'c 10:200 rwm' "${flags[@]}" "$test_image")
  created_nodes+=("$container_id")
  docker_local start "$container_id" >/dev/null
  docker_local exec "$name-$node" sh -c 'umask 077; wg genkey > /run/wg-private; wg pubkey < /run/wg-private' > "$fixture/$node.pub"
  wg_test_tun "$node"
}

wg_test_render() {
  local underlay
  underlay=$(docker_local inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name-gateway")
  cat > "$fixture/mesh.yaml" <<MANIFEST
version: 1
network:
  cidr: 10.88.0.0/24
gateway:
  name: gateway
  address: 10.88.0.1
  public_key: $(cat "$fixture/gateway.pub")
  endpoint: $underlay:51820
nodes:
  - name: worker-a
    address: 10.88.0.2
    public_key: $(cat "$fixture/a.pub")
  - name: worker-b
    address: 10.88.0.3
    public_key: $(cat "$fixture/b.pub")
MANIFEST
  "$conductor_binary" wireguard render --manifest "$fixture/mesh.yaml" --out "$fixture/plan"
}

wg_test_apply() {
  local node=$1 config=$2 address=$3
  docker_local cp "$fixture/plan/$config.conf" "$name-$node:/run/template.conf"
  # Fill the private key entirely inside its owning node, through a pipe.
  docker_local exec "$name-$node" sh -c 'wg-quick strip /run/template.conf | awk '\''BEGIN {getline key < "/run/wg-private"} /^PrivateKey = / {$0="PrivateKey = " key} {print}'\'' | wg syncconf wg0 /dev/stdin'
  docker_local exec "$name-$node" ip address replace "$address/32" dev wg0
  docker_local exec "$name-$node" ip link set wg0 mtu 1380 up
  docker_local exec "$name-$node" ip route replace 10.88.0.0/24 dev wg0
}

wg_test_reset_worker() {
  local node=$1 config=$2 address=$3
  # A newly re-added gateway peer has forgotten its learned endpoint/session.
  # Explicitly reset the worker peer too; this is manual recovery, not convergence.
  docker_local exec "$name-$node" wg set wg0 peer "$(cat "$fixture/gateway.pub")" remove
  wg_test_apply "$node" "$config" "$address"
}

wg_test_overlay() {
  phase=overlay-setup
  local node
  for node in gateway a b; do wg_test_node "$node"; done
  wg_test_render
  wg_test_apply gateway gateway 10.88.0.1
  wg_test_apply a worker-a 10.88.0.2
  wg_test_apply b worker-b 10.88.0.3
}

wg_test_ping() {
  local node=$1 destination=$2
  docker_local exec "$name-$node" ping -c 4 -W 3 "$destination" | tee "$fixture/ping.log"
  grep -F ', 0% packet loss' "$fixture/ping.log" >/dev/null
}

wg_test_denied() {
  local node=$1 destination=$2
  if docker_local exec "$name-$node" ping -c 1 -W 2 "$destination" > "$fixture/denied.log" 2>&1; then
    wg_test_note "FAIL: $phase unexpectedly reached $destination from $node"
    return 1
  fi
  # Command/setup failures are not evidence that a real packet was denied.
  grep -Eq '1 packets transmitted, 0 received, (\+1 errors, )?100% packet loss' "$fixture/denied.log"
}

wg_test_cleanup() {
  local container_id failed=0
  for container_id in "${created_nodes[@]}"; do docker_local rm -f "$container_id" >/dev/null 2>&1 || failed=1; done
  if [[ "$network_created" == 1 ]]; then docker_local network rm "$name" >/dev/null 2>&1 || failed=1; fi
  for container_id in "${created_nodes[@]}"; do
    if docker_local inspect "$container_id" >/dev/null 2>&1; then failed=1; fi
  done
  if [[ "$network_created" == 1 ]] && docker_local network inspect "$name" >/dev/null 2>&1; then failed=1; fi
  rm -rf "$fixture" || failed=1
  created_nodes=()
  network_created=0
  return "$failed"
}

wg_test_finish() {
  local result=$1
  trap - EXIT
  if ! wg_test_cleanup; then
    wg_test_note 'FAIL: fixture cleanup left a resource or could not complete'
    result=1
  else
    wg_test_note 'PASS: all owned containers, network and private fixture files removed'
  fi
  if [[ "$result" != 0 ]]; then wg_test_note "FAIL: phase=$phase exit=$result"; fi
  exit "$result"
}
