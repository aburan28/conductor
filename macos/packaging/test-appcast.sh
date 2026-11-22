#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/conductor-appcast-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
openssl rand 32 | openssl base64 -A > "$work/key"
public="$(swift "$here/public-key.swift" "$work/key")"
printf 'test DMG bytes\n' > "$work/Conductor-1.2.3-macos-universal.dmg"
"$here/appcast.sh" generate --dmg "$work/Conductor-1.2.3-macos-universal.dmg" \
  --version v1.2.3 --repo aburan28/conductor --out "$work/appcast.xml" \
  --key-file "$work/key" --public-key "$public"
"$here/appcast.sh" verify --dmg "$work/Conductor-1.2.3-macos-universal.dmg" \
  --version v1.2.3 --repo aburan28/conductor --out "$work/appcast.xml" \
  --key-file "$work/key" --public-key "$public"
# Change bytes without changing the length: the verifier must check the signature.
printf X | dd of="$work/Conductor-1.2.3-macos-universal.dmg" bs=1 seek=0 conv=notrunc 2>/dev/null
if "$here/appcast.sh" verify --dmg "$work/Conductor-1.2.3-macos-universal.dmg" \
  --version v1.2.3 --repo aburan28/conductor --out "$work/appcast.xml" \
  --key-file "$work/key" --public-key "$public" > /dev/null 2>&1; then
  echo 'appcast: changed DMG was accepted' >&2; exit 1
fi
printf 'test DMG bytes\n' > "$work/Conductor-1.2.3-macos-universal.dmg"
if "$here/appcast.sh" verify --dmg "$work/Conductor-1.2.3-macos-universal.dmg" \
  --version v1.2.3 --repo aburan28/conductor --out "$work/appcast.xml" \
  --key-file "$work/key" --public-key 'wrong' > /dev/null 2>&1; then
  echo 'appcast: mismatched public key was accepted' >&2; exit 1
fi
echo 'appcast tests passed'
