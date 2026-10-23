#!/usr/bin/env bash
# Isolated container acceptance: concurrent replicas, database loss/recovery, replica loss.
# Requires Docker. This does not test RDS promotion, TLS, ALB, or availability-zone loss.
set -euo pipefail
image=${1:?usage: smoke.sh IMAGE}
prefix="conductor-ha-smoke-$$"
network="$prefix-network"
database="$prefix-db"
replica_a="$prefix-a"
replica_b="$prefix-b"
cleanup() {
  docker rm -f -v "$replica_a" "$replica_b" "$database" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT
docker network create "$network" >/dev/null
docker run -d --name "$database" --network "$network" \
  -e POSTGRES_USER=conductor -e POSTGRES_DB=conductor \
  -e POSTGRES_HOST_AUTH_METHOD=trust postgres:17.11-bookworm >/dev/null
for ((i=0; i<60; i++)); do
  if docker exec "$database" pg_isready -U conductor -d conductor >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$database" pg_isready -U conductor -d conductor >/dev/null

for replica in "$replica_a" "$replica_b"; do
  docker run -d --name "$replica" --network "$network" --read-only \
    -p 127.0.0.1::8080 -e "PGHOST=$database" -e PGUSER=conductor \
    -e PGDATABASE=conductor -e PGSSLMODE=disable \
    -e CONDUCTOR_SECRET_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= \
    "$image" --addr 0.0.0.0:8080 --insecure --security-mode enhanced \
    --database external >/dev/null
done
port_a=$(docker port "$replica_a" 8080/tcp)
port_b=$(docker port "$replica_b" 8080/tcp)
url_a="http://$port_a"
url_b="http://$port_b"
status() { curl --silent --output /dev/null --max-time 4 --write-out '%{http_code}' "$1" || true; }
await_status() {
  local url=$1 expected=$2
  for ((i=0; i<60; i++)); do
    if [[ $(status "$url") == "$expected" ]]; then return; fi
    sleep 1
  done
  echo "Timed out waiting for HTTP $expected at $url" >&2
  return 1
}
await_status "$url_a/v1/readyz" 200
await_status "$url_b/v1/readyz" 200
for replica in "$replica_a" "$replica_b"; do
  docker exec "$replica" /usr/local/bin/conductor-healthcheck
done
echo 'PASS: both replicas started against one database'

docker stop --time 10 "$database" >/dev/null
for url in "$url_a" "$url_b"; do
  await_status "$url/v1/readyz" 503
  [[ $(status "$url/v1/live") == 200 ]]
done
for replica in "$replica_a" "$replica_b"; do
  docker exec "$replica" /usr/local/bin/conductor-healthcheck
done
docker start "$database" >/dev/null
await_status "$url_a/v1/readyz" 200
await_status "$url_b/v1/readyz" 200
for replica in "$replica_a" "$replica_b"; do
  [[ $(docker inspect --format '{{.RestartCount}}' "$replica") == 0 ]]
done
echo 'PASS: both replicas recovered database connections without restarting'

docker stop --time 35 "$replica_a" >/dev/null
await_status "$url_b/v1/readyz" 200
echo 'PASS: surviving replica remains ready after peer shutdown'
