#!/usr/bin/env bash
# Run after authorizing this host in Tailscale. This does not start conductord.
set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
  echo 'usage: sudo configure-tailnet.sh' >&2
  exit 2
fi
dns=$(tailscale status --json | jq -r '.Self.DNSName // empty | rtrimstr(".")')
if [[ -z $dns || $dns != *.ts.net ]]; then
  echo 'Tailscale has no registered tailnet DNS name; authorize the host first' >&2
  exit 1
fi
printf 'CONDUCTOR_PUBLIC_URL=https://%s\n' "$dns" >/etc/conductor/conductor.env
chown conductor:conductor /etc/conductor/conductor.env
chmod 0600 /etc/conductor/conductor.env
tailscale serve --bg --https=443 http://127.0.0.1:8080
echo "Conductor tailnet URL: https://${dns}"
