#!/usr/bin/env bash
# Run as root on the Ubuntu 24.04 EC2 host after uploading this directory to S3.
set -euo pipefail

if [[ ${EUID} -ne 0 || $# -ne 1 ]]; then
  echo "usage: sudo prepare-host.sh <private-backup-bucket>" >&2
  exit 2
fi
bucket=$1
export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y ca-certificates curl jq postgresql-common unzip
if ! command -v aws >/dev/null 2>&1; then
  curl -fsSL https://awscli.amazonaws.com/v2/install.sh -o /tmp/conductor-aws-cli-install.sh
  bash /tmp/conductor-aws-cli-install.sh --system
fi
install -d -m 0755 /usr/share/postgresql-common/pgdg
curl -fsSLo /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
  https://www.postgresql.org/media/keys/ACCC4CF8.asc
cat >/etc/apt/sources.list.d/pgdg.sources <<'EOF'
Types: deb
URIs: https://apt.postgresql.org/pub/repos/apt
Suites: noble-pgdg
Architectures: amd64
Components: main
Signed-By: /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
EOF
apt-get update
apt-get install -y postgresql-18 postgresql-client-18
curl -fsSL https://tailscale.com/install.sh | sh

if ! id conductor >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/conductor --create-home --shell /usr/sbin/nologin conductor
fi
install -d -m 0700 -o conductor -g conductor /var/lib/conductor /etc/conductor /var/backups/conductor
install -d -m 0755 /opt/conductor/bin /opt/conductor/bootstrap
aws s3 sync "s3://${bucket}/bootstrap/" /opt/conductor/bootstrap/ --exclude 'secret.key'
(cd /opt/conductor/bootstrap && sha256sum --check SHA256SUMS)
chmod 0755 /opt/conductor/bootstrap/backup.sh /opt/conductor/bootstrap/restore.sh \
  /opt/conductor/bootstrap/configure-tailnet.sh /opt/conductor/bootstrap/table-counts.sh \
  /opt/conductor/bootstrap/smoke-restore.sh
install -m 0755 /opt/conductor/bootstrap/conductord-linux-amd64 /opt/conductor/bin/conductord
install -m 0755 /opt/conductor/bootstrap/conductor-linux-amd64 /opt/conductor/bin/conductor

systemctl enable --now postgresql
if ! pg_lsclusters -h | awk '$1 == "18" && $2 == "main" && $4 == "online" { ok=1 } END { exit !ok }'; then
  echo "PostgreSQL 18/main is not online" >&2
  exit 1
fi
if ! runuser -u postgres -- psql -Atqc "SELECT 1 FROM pg_roles WHERE rolname = 'conductor'" | grep -qx 1; then
  runuser -u postgres -- createuser conductor
fi
if ! runuser -u postgres -- psql -Atqc "SELECT 1 FROM pg_database WHERE datname = 'conductor'" | grep -qx 1; then
  runuser -u postgres -- createdb --owner=conductor conductor
fi

printf 'CONDUCTOR_BACKUP_BUCKET=%s\n' "$bucket" >/etc/conductor/backup.env
chown conductor:conductor /etc/conductor/backup.env
chmod 0600 /etc/conductor/backup.env
install -m 0644 /opt/conductor/bootstrap/conductor.service /etc/systemd/system/conductor.service
install -m 0644 /opt/conductor/bootstrap/backup.service /etc/systemd/system/conductor-backup.service
install -m 0644 /opt/conductor/bootstrap/backup.timer /etc/systemd/system/conductor-backup.timer
systemctl daemon-reload

echo "Host prepared. Install the existing sealing key and restore a verified dump before starting conductor.service."
