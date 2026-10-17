#!/bin/bash
# cloud-init user-data for Ubuntu 24.04: Postgres on loopback plus a pinned build of Conductor,
# both under systemd. launch.py fills in the three double-underscore values below before sending it.
# Progress goes to the serial console, so `ec2 get-console-output` shows it without SSH.
set -euo pipefail
exec > >(tee -a /var/log/conductor-bootstrap.log /dev/console) 2>&1
echo "conductor-bootstrap: start $(date -u +%FT%TZ)"

COMMIT="__COMMIT__"          # full commit sha of github.com/aburan28/conductor to build
GO_VERSION="1.26.9"          # go.mod's `go` line
GO_SHA256="__GO_SHA256__"    # from go.dev/dl, checked below
GOARCH_NAME="__GOARCH__"     # amd64 or arm64
PORT=8443

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q postgresql git curl ca-certificates openssl

# The build needs about 1.5 GB; a small instance gets swap so it cannot be OOM-killed.
if ! swapon --show | grep -q .; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile >/dev/null && swapon /swapfile
fi

# Go toolchain, checksum pinned.
cd /tmp
curl -fsSL -o go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH_NAME}.tar.gz"
echo "${GO_SHA256}  go.tgz" | sha256sum -c -
tar -C /usr/local -xzf go.tgz && rm go.tgz

# Conductor at the pinned commit.
git clone -q https://github.com/aburan28/conductor /opt/conductor-src
git -C /opt/conductor-src checkout -q "$COMMIT"
cd /opt/conductor-src
# cloud-init runs with no HOME, and go needs a module and build cache location.
export HOME=/root GOPATH=/root/go GOCACHE=/root/.cache/go-build
# Go's download and compile chatter would fill the console's 64 KB buffer, so it goes to the log
# file only; -p 1 keeps peak memory low enough for a 2 GB instance.
for bin in conductor conductord; do
  echo "conductor-bootstrap: building $bin"
  CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath /usr/local/go/bin/go build -p 1 \
    -ldflags "-s -w" -o "/usr/local/bin/$bin" "./cmd/$bin" >>/var/log/conductor-build.log 2>&1 \
    || { echo "conductor-bootstrap: FAILED building $bin"; tail -20 /var/log/conductor-build.log; exit 1; }
done
echo "conductor-bootstrap: built $(/usr/local/bin/conductor version | head -1)"

# Database: the distribution's Postgres, listening on localhost only.
id conductor >/dev/null 2>&1 || useradd --system --home-dir /var/lib/conductor --create-home --shell /usr/sbin/nologin conductor
DBPASS="$(openssl rand -hex 24)"
sudo -u postgres psql -v ON_ERROR_STOP=1 -q \
  -c "CREATE ROLE conductor LOGIN PASSWORD '${DBPASS}'" \
  -c "CREATE DATABASE conductor OWNER conductor"
install -d -m 0750 -o root -g conductor /etc/conductor
umask 077
printf 'DATABASE_URL=postgres://conductor:%s@127.0.0.1:5432/conductor?sslmode=disable\n' "$DBPASS" > /etc/conductor/conductord.env
openssl rand -base64 32 > /etc/conductor/secret.key
chown root:conductor /etc/conductor/conductord.env /etc/conductor/secret.key
chmod 0640 /etc/conductor/conductord.env /etc/conductor/secret.key

# This instance's public address (IMDSv2), for the certificate and --public-url.
IMDS_TOKEN="$(curl -fsS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 300')"
imds() { curl -fsS -H "X-aws-ec2-metadata-token: ${IMDS_TOKEN}" "http://169.254.169.254/latest/meta-data/$1"; }
PUBIP="$(imds public-ipv4)"
PUBDNS="$(imds public-hostname)"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 365 \
  -subj "/CN=conductor" -addext "subjectAltName=DNS:${PUBDNS},IP:${PUBIP}" \
  -keyout /etc/conductor/tls.key -out /etc/conductor/tls.crt 2>/dev/null
chown root:conductor /etc/conductor/tls.key /etc/conductor/tls.crt
chmod 0640 /etc/conductor/tls.key
chmod 0644 /etc/conductor/tls.crt
umask 022

cat > /etc/systemd/system/conductord.service <<UNIT
[Unit]
Description=Conductor control plane
After=network-online.target postgresql.service
Wants=network-online.target
Requires=postgresql.service

[Service]
User=conductor
Group=conductor
EnvironmentFile=/etc/conductor/conductord.env
Environment=CONDUCTOR_STATE_DIR=/var/lib/conductor
ExecStart=/usr/local/bin/conductord --addr 0.0.0.0:${PORT} --security-mode enhanced \\
  --tls-cert /etc/conductor/tls.crt --tls-key /etc/conductor/tls.key \\
  --secret-key-file /etc/conductor/secret.key --public-url https://${PUBIP}:${PORT}
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/conductor

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now conductord

echo "conductor-bootstrap: waiting for conductord"
for _ in $(seq 120); do
  [ "$(curl -ks -o /dev/null -w '%{http_code}' "https://127.0.0.1:${PORT}/v1/health")" = 200 ] && break
  sleep 1
done
HEALTH="$(curl -ks -o /dev/null -w '%{http_code}' "https://127.0.0.1:${PORT}/v1/health")"
[ "$HEALTH" = 200 ] || { echo "conductor-bootstrap: FAILED health=${HEALTH}"; journalctl -u conductord --no-pager -n 40; exit 1; }

# First project and administrator. The token is short-lived and printed once, to the console.
install -d -o conductor -g conductor /srv/conductor/main
sudo -u conductor env CONDUCTOR_STATE_DIR=/var/lib/conductor /usr/local/bin/conductor init --dir /srv/conductor/main >/dev/null
TOKEN_OUT="$(sudo -u conductor env CONDUCTOR_STATE_DIR=/var/lib/conductor USER=admin \
  bash -c 'set -a; . /etc/conductor/conductord.env; set +a; cd /srv/conductor/main && /usr/local/bin/conductord bootstrap --repo /srv/conductor/main --principal admin --project main --endpoint "https://'"${PUBIP}:${PORT}"'" --no-login --token-ttl 24h' 2>&1)"
TOKEN="$(printf '%s\n' "$TOKEN_OUT" | grep -oE 'cdt_[A-Za-z0-9_-]+' | head -1 || true)"
[ -n "$TOKEN" ] || { echo "conductor-bootstrap: FAILED to mint a token"; printf '%s\n' "$TOKEN_OUT" | sed 's/cdt_[A-Za-z0-9_-]*/cdt_<redacted>/g'; exit 1; }

echo "CONDUCTOR_URL=https://${PUBIP}:${PORT}"
echo "CONDUCTOR_TOKEN=${TOKEN}"
echo "CONDUCTOR_READY health=${HEALTH}"
