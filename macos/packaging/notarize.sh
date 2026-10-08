#!/usr/bin/env bash
# Notarize a .pkg, .dmg or zipped .app with Apple, then staple the ticket to it.
#
#   macos/packaging/notarize.sh dist/Conductor-1.2.3.dmg
#
# Credentials, one of (see signing.sh):
#   NOTARY_PROFILE                              a profile saved by `xcrun notarytool store-credentials`
#   NOTARY_KEY + NOTARY_KEY_ID + NOTARY_ISSUER  an App Store Connect API key
# With none set, the script refuses: a file that was not notarized must not be published as if
# it were. A partial API key, or a profile and a key at once, is refused too.
#
# `notarytool submit --wait` exits 0 once Apple has answered, whatever the answer was. The
# status in its JSON is read here, so a rejected file stops the release with Apple's log
# instead of reaching `stapler`, which would fail with nothing about why.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=signing.sh
. "$HERE/signing.sh"

FILE="${1:-}"
[ -n "$FILE" ] && [ -e "$FILE" ] || signing_die "usage: $0 FILE (a .pkg, .dmg or .zip)"

MODE="$(notary_mode)" || exit 1
[ "$MODE" != none ] || signing_die "no notary credentials for $FILE: set NOTARY_PROFILE, or NOTARY_KEY, NOTARY_KEY_ID and NOTARY_ISSUER"
[ "$(uname -s)" = "Darwin" ] || signing_die "notarytool exists only on macOS"

ARGS=()
while IFS= read -r line; do ARGS+=("$line"); done < <(notary_args)

echo "notarize: submitting $FILE ($MODE credentials); this waits for Apple's answer"
OUT="$(xcrun notarytool submit "$FILE" --wait --output-format json "${ARGS[@]}")" \
    || signing_die "notarytool submit failed: $OUT"
ID="$(printf '%s' "$OUT" | json_field id)"
STATUS="$(printf '%s' "$OUT" | json_field status)"
if ! printf '%s' "$OUT" | notary_accepted; then
    echo "notarize: Apple answered '${STATUS:-no status}' for $FILE (submission ${ID:-unknown}). Its log:" >&2
    [ -z "$ID" ] || xcrun notarytool log "$ID" "${ARGS[@]}" >&2 || true
    exit 1
fi
echo "notarize: accepted (submission $ID)"
xcrun stapler staple "$FILE"
xcrun stapler validate "$FILE"
