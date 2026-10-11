#!/usr/bin/env bash
#
# Generate a Conductor peer mesh: one CA and one certificate per daemon.
#
#   scripts/gen-peer-certs.sh alpha beta
#
# writes .conductor/certs/ca.pem plus .conductor/certs/<name>/{cert.pem,key.pem}
# (override the output directory with CONDUCTOR_CERT_DIR). Every daemon cert carries both
# serverAuth and clientAuth — one certificate, two roles: served as the daemon's TLS
# certificate and presented when dialing peers.
#
# SANs include localhost/127.0.0.1 so two daemons on one machine can peer for testing.
# For real hosts, extend the SAN list with -addext "subjectAltName=DNS:host.example.com".

set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <daemon-name> [<daemon-name>...]" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CERTS="${CONDUCTOR_CERT_DIR:-$ROOT/.conductor/certs}"
mkdir -p "$CERTS"
umask 077

if [ ! -f "$CERTS/ca.pem" ]; then
  echo "==> mesh CA"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$CERTS/ca.key" -out "$CERTS/ca.pem" \
    -subj "/CN=conductor mesh CA" -days 3650 \
    -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" >/dev/null 2>&1
else
  echo "==> reusing existing mesh CA ($CERTS/ca.pem)"
fi

for name in "$@"; do
  dir="$CERTS/$name"
  mkdir -p "$dir"
  if [ -f "$dir/cert.pem" ]; then
    echo "==> $name: certificate already exists, skipping"
    continue
  fi
  echo "==> $name: mesh certificate"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$dir/key.pem" -out "$dir/csr.pem" \
    -subj "/CN=$name" >/dev/null 2>&1
  openssl x509 -req -in "$dir/csr.pem" \
    -CA "$CERTS/ca.pem" -CAkey "$CERTS/ca.key" -CAcreateserial \
    -out "$dir/cert.pem" -days 825 \
    -extfile <(printf '%s\n' \
      "basicConstraints=critical,CA:FALSE" \
      "keyUsage=critical,digitalSignature,keyAgreement" \
      "extendedKeyUsage=serverAuth,clientAuth" \
      "subjectAltName=DNS:$name,DNS:localhost,IP:127.0.0.1") >/dev/null 2>&1
  rm -f "$dir/csr.pem"
done

echo
echo "Mesh ready. Per daemon, run conductord with:"
for name in "$@"; do
  echo "  --peer-ca $CERTS/ca.pem --peer-cert $CERTS/$name/cert.pem --peer-key $CERTS/$name/key.pem"
done
echo "and add each remote daemon with --peer <name>=https://<host>:<port>."
