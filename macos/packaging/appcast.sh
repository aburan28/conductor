#!/usr/bin/env bash
# Build and verify Conductor's one-item Sparkle feed for a notarized DMG whose root
# contains Install Conductor.pkg. A package update needs installationType=package.
set -euo pipefail

die() { echo "appcast: $*" >&2; exit 1; }
mode="${1:-}"; shift || true
dmg="" version="" repo="" output="" key="" public=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dmg) dmg="${2:-}"; shift 2 ;;
    --version) version="${2:-}"; shift 2 ;;
    --repo) repo="${2:-}"; shift 2 ;;
    --out) output="${2:-}"; shift 2 ;;
    --key-file) key="${2:-}"; shift 2 ;;
    --public-key) public="${2:-}"; shift 2 ;;
    *) die "unknown argument $1" ;;
  esac
done
[[ "$mode" == generate || "$mode" == verify ]] || die "usage: appcast.sh <generate|verify> --dmg FILE --version vX.Y.Z --repo OWNER/REPO --out FILE --key-file FILE --public-key BASE64"
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "invalid version $version"
[[ "$repo" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "invalid repository $repo"
[ -f "$dmg" ] && [ -s "$dmg" ] || die "missing DMG $dmg"
[ -f "$key" ] && [ -s "$key" ] || die "missing private key file"
[ -n "$output" ] && [ -n "$public" ] || die "--out and --public-key are required"
[ "$(basename "$dmg")" = "Conductor-${version#v}-macos-universal.dmg" ] || die "DMG filename does not match version"
sign_update="${SPARKLE_SIGN_UPDATE:-$(cd "$(dirname "$0")/.." && pwd)/.build/artifacts/sparkle/Sparkle/bin/sign_update}"
[ -x "$sign_update" ] || die "Sparkle sign_update is missing at $sign_update"
actual_public="$(swift "$(dirname "$0")/public-key.swift" "$key")"
[ "$actual_public" = "$public" ] || die "private key does not match SUPublicEDKey"
url="https://github.com/$repo/releases/download/$version/$(basename "$dmg")"
bytes="$(wc -c < "$dmg" | tr -d ' ')"

if [ "$mode" = generate ]; then
  signed="$("$sign_update" --ed-key-file "$key" "$dmg")"
  signature="$(printf '%s\n' "$signed" | sed -n 's/.*sparkle:edSignature="\([A-Za-z0-9+/=]*\)".*/\1/p')"
  [ -n "$signature" ] || die "Sparkle did not return an EdDSA signature"
  mkdir -p "$(dirname "$output")"
  cat > "$output" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:sparkle="http://www.andymatuschak.org/xml-namespaces/sparkle">
  <channel>
    <title>Conductor updates</title>
    <item>
      <title>Conductor ${version#v}</title>
      <sparkle:version>${version#v}</sparkle:version>
      <sparkle:shortVersionString>${version#v}</sparkle:shortVersionString>
      <enclosure url="$url" length="$bytes" type="application/octet-stream" sparkle:edSignature="$signature" sparkle:installationType="package" />
    </item>
  </channel>
</rss>
EOF
fi

[ -s "$output" ] || die "feed is missing"
xmllint --noout "$output"
xml="$(<"$output")"
[[ "$xml" == *"<sparkle:version>${version#v}</sparkle:version>"* ]] || die "feed version mismatch"
[[ "$xml" == *"url=\"$url\""* ]] || die "feed URL mismatch"
[[ "$xml" == *"length=\"$bytes\""* ]] || die "feed length mismatch"
[[ "$xml" == *'sparkle:installationType="package"'* ]] || die "package installation type is missing"
signature="$(printf '%s\n' "$xml" | sed -n 's/.*sparkle:edSignature="\([A-Za-z0-9+/=]*\)".*/\1/p')"
[ -n "$signature" ] || die "feed signature is missing"
"$sign_update" --ed-key-file "$key" --verify "$dmg" "$signature" >/dev/null
echo "appcast: verified ${version} DMG signature, size, URL, and package installation type"
