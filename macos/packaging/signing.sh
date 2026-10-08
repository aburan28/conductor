# shellcheck shell=bash
# Shared by sign.sh, notarize.sh, build-pkg.sh and build-dmg.sh: which signing configuration
# the environment holds, refusing a partial one, and reading notarytool's answers.
#
# The variables, all or none for a release (build-dmg.sh):
#
#   DEVELOPER_ID_APPLICATION   "Developer ID Application: NAME (TEAMID)"  signs the app and the image
#   DEVELOPER_ID_INSTALLER     "Developer ID Installer: NAME (TEAMID)"    signs the .pkg
#   NOTARY_PROFILE             a keychain profile from `xcrun notarytool store-credentials`
#     or all three of
#   NOTARY_KEY                 path to an App Store Connect API key (.p8)
#   NOTARY_KEY_ID              that key's id
#   NOTARY_ISSUER              the issuer id it belongs to
#
# Some set and some not is refused rather than half done: a signed package that was never
# notarized is blocked by Gatekeeper exactly like an unsigned one, and looks finished.

signing_die() { echo "signing: $*" >&2; exit 1; }

# notary_mode prints "profile", "key" or "none", and refuses a partial or doubled one.
notary_mode() {
    local key="${NOTARY_KEY:-}" id="${NOTARY_KEY_ID:-}" issuer="${NOTARY_ISSUER:-}" profile="${NOTARY_PROFILE:-}"
    if [ -n "$key$id$issuer" ]; then
        if [ -z "$key" ] || [ -z "$id" ] || [ -z "$issuer" ]; then
            local missing=()
            [ -n "$key" ] || missing+=(NOTARY_KEY)
            [ -n "$id" ] || missing+=(NOTARY_KEY_ID)
            [ -n "$issuer" ] || missing+=(NOTARY_ISSUER)
            signing_die "an App Store Connect API key needs NOTARY_KEY, NOTARY_KEY_ID and NOTARY_ISSUER together; missing: ${missing[*]}"
        fi
        [ -z "$profile" ] || signing_die "set NOTARY_PROFILE or the NOTARY_KEY trio, not both"
        echo key
    elif [ -n "$profile" ]; then
        echo profile
    else
        echo none
    fi
}

# release_signing_mode prints "developer-id" when everything a notarized release needs is
# set, "none" when nothing is, and refuses anything in between, naming what is missing.
release_signing_mode() {
    local notary
    notary="$(notary_mode)" || exit 1
    local app="${DEVELOPER_ID_APPLICATION:-}" installer="${DEVELOPER_ID_INSTALLER:-}"
    if [ -z "$app$installer" ] && [ "$notary" = none ]; then
        echo none
        return
    fi
    local missing=()
    [ -n "$app" ] || missing+=(DEVELOPER_ID_APPLICATION)
    [ -n "$installer" ] || missing+=(DEVELOPER_ID_INSTALLER)
    [ "$notary" != none ] || missing+=("NOTARY_PROFILE (or NOTARY_KEY, NOTARY_KEY_ID and NOTARY_ISSUER)")
    if [ "${#missing[@]}" -gt 0 ]; then
        signing_die "some signing variables are set and some are not; missing: ${missing[*]}. Set all of them for a signed, notarized release, or none for an unsigned one."
    fi
    echo developer-id
}

# notary_args prints the notarytool credential arguments, one per line.
notary_args() {
    case "$(notary_mode)" in
        profile) printf '%s\n' --keychain-profile "$NOTARY_PROFILE" ;;
        key)     printf '%s\n' --key "$NOTARY_KEY" --key-id "$NOTARY_KEY_ID" --issuer "$NOTARY_ISSUER" ;;
        *)       return 1 ;;
    esac
}

# json_field FIELD reads a top-level string field from notarytool's --output-format json on
# stdin. plutil reads JSON on a Mac; the sed fallback is for testing this on Linux. plutil's
# output is used only when it succeeds: the plutil in Swift's Linux toolchain has no -extract
# and prints its usage to stdout before failing, which must not reach the caller.
json_field() {
    local input value
    input="$(cat)"
    if command -v plutil >/dev/null 2>&1 &&
        value="$(printf '%s' "$input" | plutil -extract "$1" raw -o - - 2>/dev/null)"; then
        printf '%s\n' "$value"
        return 0
    fi
    printf '%s' "$input" | tr '\n' ' ' | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"
}

# notary_accepted succeeds only when notarytool's JSON says "Accepted". `notarytool submit
# --wait` exits 0 once Apple has answered, whatever the answer, so its status must be read.
notary_accepted() {
    [ "$(json_field status)" = "Accepted" ]
}
