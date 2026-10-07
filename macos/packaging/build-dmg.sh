#!/usr/bin/env bash
# Build the release disk image: a .dmg holding "Install Conductor.pkg", which installs
# Conductor.app and links its commands into /usr/local/bin.
#
#   macos/packaging/build-dmg.sh --app macos/build/Conductor.app --version 1.2.3 [--out dist]
#
# Order: copy the app, stamp the version (and Sparkle's SUPublicEDKey from
# SPARKLE_PUBLIC_ED_KEY), sign it (sign.sh), build and sign the package (build-pkg.sh),
# notarize and staple the package, build the image, sign it, notarize and staple it.
#
# Signing is all or nothing (signing.sh): DEVELOPER_ID_APPLICATION, DEVELOPER_ID_INSTALLER
# and notary credentials (NOTARY_PROFILE, or NOTARY_KEY + NOTARY_KEY_ID + NOTARY_ISSUER).
# With none of them the image is built unsigned, which Gatekeeper will question, and the
# README inside says how to open it. With some of them the script refuses to start.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
# shellcheck source=signing.sh
. "$HERE/signing.sh"

APP="" VERSION="" OUT="dist"
while [ $# -gt 0 ]; do
    case "$1" in
        --app)     [ $# -ge 2 ] || signing_die "--app needs a value";     APP="${2%/}"; shift 2 ;;
        --version) [ $# -ge 2 ] || signing_die "--version needs a value"; VERSION="${2#v}"; shift 2 ;;
        --out)     [ $# -ge 2 ] || signing_die "--out needs a value";     OUT="$2"; shift 2 ;;
        -h|--help) sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) signing_die "unknown argument: $1" ;;
    esac
done
[ -n "$APP" ] && [ -x "$APP/Contents/MacOS/Conductor" ] || signing_die "--app must name a built Conductor.app (macos/build.sh)"
[ -n "$VERSION" ] || signing_die "--version is required"
[ "$(uname -s)" = "Darwin" ] || signing_die "hdiutil, pkgbuild and codesign exist only on macOS"

# Refuses a partial configuration before anything is built.
MODE="$(release_signing_mode)" || exit 1
if [ "$MODE" = none ]; then
    echo "dmg: Developer ID signing and notarization skipped: none of DEVELOPER_ID_APPLICATION, DEVELOPER_ID_INSTALLER, NOTARY_PROFILE (or NOTARY_KEY/NOTARY_KEY_ID/NOTARY_ISSUER) is set."
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/conductor-dmg.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
STAGE="$WORK/image"
mkdir -p "$STAGE" "$OUT"

# A copy, so the stamp and the signature land on this release's bundle only.
ditto "$APP" "$WORK/Conductor.app"
PLIST="$WORK/Conductor.app/Contents/Info.plist"
plutil -replace CFBundleShortVersionString -string "$VERSION" "$PLIST"
plutil -replace CFBundleVersion -string "$VERSION" "$PLIST"
if [ -n "${SPARKLE_PUBLIC_ED_KEY:-}" ]; then
    plutil -replace SUPublicEDKey -string "$SPARKLE_PUBLIC_ED_KEY" "$PLIST"
    UPDATES="Sparkle updates on (SUPublicEDKey set)"
else
    echo "dmg: Sparkle updates off: SPARKLE_PUBLIC_ED_KEY is not set, so the app will not check for updates."
    UPDATES="Sparkle updates off (no SUPublicEDKey)"
fi
"$HERE/sign.sh" "$WORK/Conductor.app"

"$HERE/build-pkg.sh" --app "$WORK/Conductor.app" --version "$VERSION" --out "$STAGE"
PKG="$STAGE/Install Conductor.pkg"
# The package carries its own ticket too: copied out of the image (to another Mac, a
# deployment tool) it then passes Gatekeeper without asking Apple online.
[ "$MODE" = none ] || "$HERE/notarize.sh" "$PKG"

if [ "$MODE" = none ]; then NOTE="$HERE/gatekeeper-unsigned.txt"; else NOTE="$HERE/gatekeeper-signed.txt"; fi
{ cat "$HERE/dmg-README.txt"; echo; cat "$NOTE"; } | sed -e "s|@VERSION@|$VERSION|g" >"$STAGE/README.txt"
cp "$REPO/LICENSE" "$STAGE/LICENSE.txt"

ARCHS="$(lipo -archs "$WORK/Conductor.app/Contents/MacOS/Conductor" | tr ' ' '\n' | LC_ALL=C sort | paste -sd, -)"
case "$ARCHS" in
    arm64,x86_64) LABEL=universal ;;
    *) LABEL="${ARCHS//,/-}" ;;
esac
DMG="$OUT/Conductor-$VERSION-macos-$LABEL.dmg"

# Retried: on hosted macOS runners `hdiutil create` sometimes fails with "Resource busy"
# for reasons unrelated to its arguments. HFS+ and UDZO open on every macOS the app runs on.
attempt=1
until hdiutil create -quiet -ov -volname "Conductor $VERSION" -srcfolder "$STAGE" \
        -fs HFS+ -format UDZO -imagekey zlib-level=9 "$DMG"; do
    [ "$attempt" -lt 3 ] || signing_die "hdiutil create failed three times; see its output above"
    echo "dmg: hdiutil create failed (attempt $attempt of 3); retrying" >&2
    attempt=$((attempt + 1))
    sleep 5
done

if [ "$MODE" != none ]; then
    codesign --force --timestamp --sign "$DEVELOPER_ID_APPLICATION" "$DMG"
    "$HERE/notarize.sh" "$DMG"
fi

echo "built $DMG"
echo "    architectures  $ARCHS"
echo "    installs       /Applications/Conductor.app, commands linked from /usr/local/bin"
echo "    updates        $UPDATES"
if [ "$MODE" = none ]; then
    echo "    signing        ad hoc app, unsigned package and image; the README inside says how to open it"
else
    echo "    signing        Developer ID; package and image notarized and stapled"
fi
