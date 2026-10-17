#!/usr/bin/env bash
# Opt-in: actual native service, native clients, TLS and team state over WireGuard.
set -euo pipefail
source "$(dirname "$0")/lib/wireguard-test.sh"

wg_test_init
daemon_binary=${WG_TEST_CONDUCTORD:-$repo/bin/conductord}
api_driver=${WG_TEST_API_DRIVER:-$repo/bin/wireguard-e2e}
db_image=${WG_TEST_POSTGRES_IMAGE:-postgres:17-alpine}
[[ -x "$daemon_binary" && -x "$api_driver" ]] || { echo 'Prebuild native conductord and the scripts/wireguard-e2e driver' >&2; exit 1; }
command -v openssl >/dev/null || { echo 'Preinstall openssl for the local fixture CA' >&2; exit 1; }
docker_local image inspect "$db_image" >/dev/null 2>&1 || { echo 'Preload the approved PostgreSQL image; no image will be pulled' >&2; exit 1; }
wg_test_overlay

phase=database-setup
openssl rand -hex 24 > "$fixture/database-password"
container_id=$(docker_local create --pull=never --name "$name-db" --network "$name" \
  --tmpfs /var/lib/postgresql/data:rw,mode=0700 \
  -e POSTGRES_USER=conductor -e POSTGRES_DB=conductor \
  -e POSTGRES_PASSWORD_FILE=/run/database-password "$db_image")
created_nodes+=("$container_id")
docker_local cp "$fixture/database-password" "$name-db:/run/database-password"
docker_local start "$container_id" >/dev/null
database_ready=0
for ((i=0; i<100; i++)); do
  if docker_local exec "$name-db" pg_isready -U conductor -d conductor -q; then database_ready=1; break; fi
  sleep 0.1
done
[[ "$database_ready" == 1 ]]
printf 'postgres://conductor:%s@%s-db:5432/conductor?sslmode=disable\n' "$(cat "$fixture/database-password")" "$name" > "$fixture/database.url"
docker_local cp "$fixture/database.url" "$name-gateway:/run/database.url"
docker_local cp "$daemon_binary" "$name-gateway:/run/conductord"
docker_local cp "$api_driver" "$name-gateway:/run/wireguard-e2e"
docker_local exec "$name-gateway" mkdir -p /run/repo

phase=local-tls-setup
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 -subj /CN=Conductor-WireGuard-Fixture-CA \
  -keyout "$fixture/ca.key" -out "$fixture/ca.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -sha256 -subj /CN=Conductor-WireGuard-Fixture \
  -keyout "$fixture/server.key" -out "$fixture/server.csr" >/dev/null 2>&1
printf 'subjectAltName=IP:10.88.0.1\nextendedKeyUsage=serverAuth\n' > "$fixture/server.ext"
openssl x509 -req -in "$fixture/server.csr" -CA "$fixture/ca.pem" -CAkey "$fixture/ca.key" \
  -CAcreateserial -days 1 -sha256 -extfile "$fixture/server.ext" -out "$fixture/server.pem" >/dev/null 2>&1
docker_local cp "$fixture/server.key" "$name-gateway:/run/server.key"
docker_local cp "$fixture/server.pem" "$name-gateway:/run/server.pem"

phase=local-native-bootstrap
bootstrap() {
  local principal=$1 project=$2 role=$3
  docker_local exec "$name-gateway" sh -c 'export DATABASE_URL="$(cat /run/database.url)"; exec /run/conductord bootstrap --org overlay-team --project "$1" --principal "$2" --role "$3" --repo /run/repo --endpoint https://10.88.0.1:8443 --no-login' \
    sh "$project" "$principal" "$role" > "$fixture/bootstrap-private.log" 2>&1
  # Bootstrap prints a login example containing the credential, so never tee it.
  grep -Eo 'cdt_[A-Za-z0-9_-]+' "$fixture/bootstrap-private.log" | tail -n 1 > "$fixture/$principal.token"
  [[ -s "$fixture/$principal.token" ]]
}
bootstrap alice overlay-main org_admin
# Rebootstrap rotates Alice's initial token; retain only her final credential.
bootstrap alice overlay-side org_admin
bootstrap outsider overlay-side contributor

for node in gateway a b; do
  docker_local cp "$fixture/ca.pem" "$name-$node:/run/ca.pem"
done
for node in a b; do
  docker_local cp "$api_driver" "$name-$node:/run/wireguard-e2e"
  docker_local cp "$conductor_binary" "$name-$node:/run/conductor"
done
docker_local cp "$fixture/alice.token" "$name-a:/run/alice.token"
docker_local cp "$fixture/outsider.token" "$name-b:/run/outsider.token"

phase=service-start
docker_local exec -d "$name-gateway" sh -c 'umask 077; echo $$ > /run/conductord.pid; export DATABASE_URL="$(cat /run/database.url)"; exec /run/conductord --addr 10.88.0.1:8443 --public-url https://10.88.0.1:8443 --tls-cert /run/server.pem --tls-key /run/server.key --security-mode enhanced --no-scheduler --github-poll=-1s --notify-poll=-1s --model-discovery-interval=-1s --delphi-interval=-1s > /run/conductord-private.log 2>&1'

worker_api() {
  local node=$1
  shift
  docker_local exec -e CONDUCTOR_ENDPOINT=https://10.88.0.1:8443 -e CONDUCTOR_CA_CERT=/run/ca.pem \
    -e CONDUCTOR_PROJECT=overlay-main -e CONDUCTOR_SIDE_PROJECT=overlay-side \
    -e NO_PROXY=10.88.0.1 -e no_proxy=10.88.0.1 \
    "$name-$node" /run/wireguard-e2e "$@"
}

service_ready=0
for ((i=0; i<40; i++)); do
  if worker_api a health > "$fixture/readiness-private.log" 2>&1; then service_ready=1; break; fi
  sleep 0.1
done
[[ "$service_ready" == 1 ]]
phase=overlay-and-tls
worker_api a health
worker_api b health
worker_api a ca-validation
underlay=$(docker_local inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name-gateway")
docker_local exec "$name-a" ping -c 1 -W 2 "$underlay" >/dev/null
worker_api a underlay-denied --address "$underlay:8443"
wg_test_note 'PASS: both worker clients use overlay HTTPS with validated local CA; reachable underlay has no application listener'

phase=team-setup-and-claim
worker_api a setup --token-file /run/alice.token --output /run/bob.token
docker_local cp "$name-a:/run/bob.token" "$fixture/bob.token"
docker_local cp "$fixture/bob.token" "$name-b:/run/bob.token"
worker_api a claim --token-file /run/alice.token --state /run/claim.json
docker_local cp "$name-a:/run/claim.json" "$fixture/claim.json"
docker_local cp "$fixture/claim.json" "$name-b:/run/claim.json"

native_cli() {
  local node=$1 actor=$2
  docker_local exec -e CONDUCTOR_ENDPOINT=https://10.88.0.1:8443 -e CONDUCTOR_CA_CERT=/run/ca.pem \
    -e CONDUCTOR_PROJECT=overlay-main -e NO_PROXY=10.88.0.1 -e no_proxy=10.88.0.1 \
    "$name-$node" sh -c 'export CONDUCTOR_TOKEN="$(cat "/run/$1.token")"; exec /run/conductor task list --json > /run/cli-result-private.json' sh "$actor"
  docker_local exec "$name-$node" grep -F 'WireGuard native authorization fixture' /run/cli-result-private.json >/dev/null
}
native_cli a alice
native_cli b bob
wg_test_note 'PASS: actual native conductor CLI task list exits 0 and contains the created task for both independent worker identities over overlay TLS'

phase=authorization-refusals
worker_api b conflict --token-file /run/bob.token --state /run/claim.json
worker_api b outsider --token-file /run/outsider.token --state /run/claim.json
worker_api b missing-token --state /run/claim.json
worker_api a verify-owner --token-file /run/alice.token --state /run/claim.json

phase=application-token-revocation
worker_api b token-revocation --token-file /run/bob.token
wg_test_ping b 10.88.0.1
worker_api a health
wg_test_note 'PASS: application token revocation is enforced while both WireGuard peers and the original member token remain usable'

phase=application-membership-revocation
worker_api a grant-side --token-file /run/alice.token
worker_api a remove-main --token-file /run/alice.token
worker_api b after-remove-main --token-file /run/bob.token --state /run/claim.json
worker_api a remove-side --token-file /run/alice.token
worker_api b after-remove-all --token-file /run/bob.token
wg_test_ping b 10.88.0.1
worker_api a verify-owner --token-file /run/alice.token --state /run/claim.json
wg_test_note 'PASS: membership-only removal preserves authentication to the side project; last-membership removal revokes credentials; WireGuard connectivity is unchanged'

phase=wireguard-peer-withdrawal
docker_local exec "$name-gateway" wg set wg0 peer "$(cat "$fixture/b.pub")" remove
wg_test_denied b 10.88.0.1
worker_api a health
wg_test_ping a 10.88.0.1
wg_test_note 'PASS: explicit WireGuard peer withdrawal separately blocks packets; remaining worker still reaches the native service'

phase=native-graceful-shutdown
docker_local exec "$name-gateway" sh -c 'kill -TERM "$(cat /run/conductord.pid)"'
service_stopped=0
for ((i=0; i<40; i++)); do
  if worker_api a underlay-denied --address 10.88.0.1:8443 > "$fixture/shutdown-private.log" 2>&1; then service_stopped=1; break; fi
  sleep 0.1
done
[[ "$service_stopped" == 1 ]]
daemon_exited=0
for ((i=0; i<40; i++)); do
  if docker_local exec "$name-gateway" sh -c 'test ! -d "/proc/$(cat /run/conductord.pid)"'; then daemon_exited=1; break; fi
  sleep 0.1
done
[[ "$daemon_exited" == 1 ]]
wg_test_ping a 10.88.0.1
wg_test_note 'PASS: native SIGTERM exits the daemon and closes its listener while the remaining WireGuard peer remains reachable'
wg_test_note 'WIREGUARD_NATIVE_TEAM_PASS: real service/database, two worker identities, overlay TLS, CLI smoke, claim/conflict, nonmember/missing-token refusals, token/member revocation, selective network withdrawal and shutdown'
