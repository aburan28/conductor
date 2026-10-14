#!/usr/bin/env bash
# Sign Conductor.app, every piece of code inside it first.
#
#   macos/packaging/sign.sh path/to/Conductor.app
#
# With DEVELOPER_ID_APPLICATION set ("Developer ID Application: NAME (TEAMID)"), everything
# is signed with that identity, the hardened runtime and a secure timestamp, which is what
# notarization requires of every executable in the bundle: the Postgres binaries and
# libraries, the Go commands, Sparkle's helpers, then the app with Conductor.entitlements
# (no App Sandbox).
#
# Without it, Developer ID signing is skipped, saying so, and the bundle is signed ad hoc so
# it runs on the Mac that built it: Apple silicon runs no unsigned code, and gluing slices
# together with lipo leaves an x86_64 slice the linker never signed.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=signing.sh
. "$HERE/signing.sh"

APP="${1:-}"
[ -n "$APP" ] && [ -d "$APP/Contents/MacOS" ] || signing_die "usage: $0 path/to/Conductor.app"
[ "$(uname -s)" = "Darwin" ] || signing_die "codesign exists only on macOS"
ENTITLEMENTS="$HERE/../Conductor.entitlements"
[ -f "$ENTITLEMENTS" ] || signing_die "missing $ENTITLEMENTS"

IDENTITY="${DEVELOPER_ID_APPLICATION:-}"
if [ -n "$IDENTITY" ]; then
    SIGN=(codesign --force --options runtime --timestamp --sign "$IDENTITY")
    echo "sign: Developer ID ($IDENTITY), hardened runtime"
else
    SIGN=(codesign --force --sign -)
    echo "sign: Developer ID signing skipped: DEVELOPER_ID_APPLICATION is not set. Signing ad hoc, which runs on this Mac only."
fi

is_macho() { file -b "$1" 2>/dev/null | grep -q 'Mach-O'; }

# Innermost first: a signature covers what is inside it, so nothing may change after.
sign_tree() {
    local dir="$1"
    [ -d "$dir" ] || return 0
    # Real files only: a symlink to a library is covered by signing its target.
    find "$dir" -type f \( -name '*.dylib' -o -name '*.so' \) -print0 | while IFS= read -r -d '' f; do
        "${SIGN[@]}" "$f"
    done
    find "$dir" -type f -perm -u+x -print0 | while IFS= read -r -d '' f; do
        case "$f" in *.dylib|*.so) continue ;; esac
        if is_macho "$f"; then "${SIGN[@]}" "$f"; fi
    done
}

sign_tree "$APP/Contents/Resources/postgres"
sign_tree "$APP/Contents/Resources/bin"

SPARKLE="$APP/Contents/Frameworks/Sparkle.framework"
if [ -d "$SPARKLE" ]; then
    # Sparkle's documented order: XPC services, Autoupdate, Updater.app, the framework.
    for part in "$SPARKLE"/Versions/B/XPCServices/*.xpc \
                "$SPARKLE/Versions/B/Autoupdate" \
                "$SPARKLE/Versions/B/Updater.app" \
                "$SPARKLE"; do
        [ -e "$part" ] || continue
        "${SIGN[@]}" --preserve-metadata=entitlements "$part"
    done
fi

"${SIGN[@]}" --entitlements "$ENTITLEMENTS" "$APP"
codesign --verify --strict --deep "$APP" || signing_die "$APP does not verify after signing"
echo "sign: $APP verifies"
