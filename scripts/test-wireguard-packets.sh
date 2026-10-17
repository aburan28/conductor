#!/usr/bin/env bash
set -euo pipefail
umask 077
repo=$(cd "$(dirname "$0")/.." && pwd)
conductor_binary=${WG_TEST_CONDUCTOR:-$repo/bin/conductor}
test_image=${WG_TEST_IMAGE:-conductor-wireguard-test:local}
[[ -x "$conductor_binary" ]] || { echo 'Build bin/conductor or set WG_TEST_CONDUCTOR first' >&2; exit 1; }
fixture=$(mktemp -d /tmp/conductor-wg-packets.XXXXXX)
name="conductor-wg-${fixture##*.}"
docker_local() { env -u DOCKER_HOST -u DOCKER_CONTEXT -u DOCKER_TLS -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker --host=unix:///var/run/docker.sock "$@"; }
created_nodes=()
network_created=0
cleanup() {
  for container_id in "${created_nodes[@]}"; do docker_local rm -f "$container_id" >/dev/null 2>&1 || true; done
  if [[ "$network_created" == 1 ]]; then docker_local network rm "$name" >/dev/null 2>&1 || true; fi
  rm -rf "$fixture"
}
trap cleanup EXIT
docker_local image inspect "$test_image" >/dev/null 2>&1 \
  || { echo 'Prebuild or preload the approved WireGuard test image; this test never pulls images' >&2; exit 1; }
docker_local network create --internal "$name" > /dev/null
network_created=1
for node in gateway a b; do
  flags=(--sysctl net.ipv4.conf.all.send_redirects=0 --sysctl net.ipv4.conf.default.send_redirects=0 --sysctl net.ipv4.conf.all.accept_redirects=0 --sysctl net.ipv4.conf.default.accept_redirects=0)
  [[ "$node" != gateway ]] || flags+=(--sysctl net.ipv4.ip_forward=1)
  container_id=$(docker_local create --pull=never --name "$name-$node" --network "$name" --cap-add NET_ADMIN --device-cgroup-rule "c 10:200 rwm" "${flags[@]}" "$test_image")
  created_nodes+=("$container_id")
  docker_local start "$container_id" >/dev/null
  docker_local exec "$name-$node" sh -c 'umask 077; wg genkey > /run/wg-private; wg pubkey < /run/wg-private' > "$fixture/$node.pub"
  docker_local exec "$name-$node" sh -c 'mkdir -p /dev/net; mknod /dev/net/tun c 10 200; chmod 600 /dev/net/tun; wireguard-go wg0'
done
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
for node in gateway a b; do
  case "$node" in gateway) config=gateway; address=10.88.0.1 ;; a) config=worker-a; address=10.88.0.2 ;; b) config=worker-b; address=10.88.0.3 ;; esac
  docker_local cp "$fixture/plan/$config.conf" "$name-$node:/run/template.conf"
  # Only public templates cross the container boundary. Private material stays
  # inside each node and flows through a pipe, never arguments or output logs.
  docker_local exec "$name-$node" sh -c 'wg-quick strip /run/template.conf | awk '\''BEGIN {getline key < "/run/wg-private"} /^PrivateKey = / {$0="PrivateKey = " key} {print}'\'' | wg setconf wg0 /dev/stdin'
  docker_local exec "$name-$node" ip address add "$address/32" dev wg0
  docker_local exec "$name-$node" ip link set wg0 mtu 1380 up
  docker_local exec "$name-$node" ip route add 10.88.0.0/24 dev wg0
done
packet_check() {
  docker_local exec "$1" ping -c 4 -W 3 "$2" | tee "$fixture/ping.log"
  grep -F ', 0% packet loss' "$fixture/ping.log" >/dev/null
}
packet_check "$name-a" 10.88.0.1
packet_check "$name-a" 10.88.0.3
packet_check "$name-b" 10.88.0.2
handshakes=$(docker_local exec "$name-gateway" wg show wg0 latest-handshakes | awk '$2>0 {n++} END {print n+0}')
[[ "$handshakes" == 2 ]]
docker_local exec "$name-gateway" wg set wg0 peer "$(cat "$fixture/b.pub")" remove
if docker_local exec "$name-a" ping -c 1 -W 2 10.88.0.3 > "$fixture/revoked.log" 2>&1; then
  echo 'Revoked WireGuard peer remained reachable through the gateway' >&2
  exit 1
fi
echo 'WIREGUARD_PACKET_SMOKE_PASS: native rendered templates, internal Docker underlay, 2 handshakes, 12/12 packets, routed worker reachability, explicit gateway peer revocation'
