#!/usr/bin/env bash
# Build the installer package: Conductor.app into /Applications, and links to its commands
# in /usr/local/bin (scripts/postinstall).
#
#   macos/packaging/build-pkg.sh --app macos/build/Conductor.app --version 1.2.3 [--out dist]
#
# The app is taken as it is: sign it first (sign.sh; build-dmg.sh does it all in order).
# With DEVELOPER_ID_INSTALLER set the package is signed with it; without it the package is
# unsigned and the script says so.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
# shellcheck source=signing.sh
. "$HERE/signing.sh"

IDENTIFIER="dev.conductor.app"
APP="" VERSION="" OUT="dist"
while [ $# -gt 0 ]; do
    case "$1" in
        --app)     [ $# -ge 2 ] || signing_die "--app needs a value";     APP="${2%/}"; shift 2 ;;
        --version) [ $# -ge 2 ] || signing_die "--version needs a value"; VERSION="${2#v}"; shift 2 ;;
        --out)     [ $# -ge 2 ] || signing_die "--out needs a value";     OUT="$2"; shift 2 ;;
        -h|--help) sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) signing_die "unknown argument: $1" ;;
    esac
done
[ -n "$APP" ] && [ -x "$APP/Contents/MacOS/Conductor" ] || signing_die "--app must name a built Conductor.app"
[ -n "$VERSION" ] || signing_die "--version is required"
[ "$(uname -s)" = "Darwin" ] || signing_die "pkgbuild and productbuild exist only on macOS"

ARCHS="$(lipo -archs "$APP/Contents/MacOS/Conductor" | tr ' ' '\n' | LC_ALL=C sort | paste -sd, -)"
case "$ARCHS" in
    arm64,x86_64) ARCH_SENTENCE="Apple silicon and Intel Macs" ;;
    arm64)        ARCH_SENTENCE="Apple silicon Macs" ;;
    x86_64)       ARCH_SENTENCE="Intel Macs" ;;
    *) signing_die "unexpected architectures in the app: $ARCHS" ;;
esac

WORK="$(mktemp -d "${TMPDIR:-/tmp}/conductor-pkg.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/pkgs" "$WORK/scripts" "$WORK/resources" "$OUT"
cp "$HERE/scripts/preinstall" "$HERE/scripts/postinstall" "$WORK/scripts/"
chmod 0755 "$WORK/scripts/preinstall" "$WORK/scripts/postinstall"

# Rooted at the bundle and installed *as* /Applications/Conductor.app. Rooted at
# /Applications instead, the payload's "." would be /Applications, recorded as root:wheel
# 0755, and every install would reset that folder's permissions. A bundle root also makes
# the package non-relocatable, so Installer never "upgrades" a copy in ~/Downloads instead.
pkgbuild --quiet \
    --root "$APP" \
    --install-location /Applications/Conductor.app \
    --identifier "$IDENTIFIER" \
    --version "$VERSION" \
    --scripts "$WORK/scripts" \
    --ownership recommended \
    "$WORK/pkgs/conductor-app.pkg"

fill() {
    sed -e "s|@VERSION@|$VERSION|g" -e "s|@IDENTIFIER@|$IDENTIFIER|g" \
        -e "s|@HOST_ARCHS@|$ARCHS|g" -e "s|@ARCH_SENTENCE@|$ARCH_SENTENCE|g" "$1"
}
fill "$HERE/distribution.xml" >"$WORK/distribution.xml"
fill "$HERE/resources/welcome.html" >"$WORK/resources/welcome.html"
cp "$REPO/LICENSE" "$WORK/resources/LICENSE.txt"

SIGN_ARGS=()
if [ -n "${DEVELOPER_ID_INSTALLER:-}" ]; then
    SIGN_ARGS=(--sign "$DEVELOPER_ID_INSTALLER" --timestamp)
    echo "pkg: signing with $DEVELOPER_ID_INSTALLER"
else
    echo "pkg: installer signing skipped: DEVELOPER_ID_INSTALLER is not set. The package is unsigned."
fi
PKG="$OUT/Install Conductor.pkg"
# The `+` form: an empty array is an unbound variable to `set -u` in macOS's bash 3.2.
productbuild --quiet \
    --distribution "$WORK/distribution.xml" \
    --package-path "$WORK/pkgs" \
    --resources "$WORK/resources" \
    ${SIGN_ARGS[@]+"${SIGN_ARGS[@]}"} \
    "$PKG"
echo "pkg: built $PKG ($ARCHS)"
