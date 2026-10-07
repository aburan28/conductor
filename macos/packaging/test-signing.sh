#!/usr/bin/env bash
# Tests for signing.sh: the all-or-nothing rule and reading notarytool's answer. They need
# no Mac and no certificate, so CI's Linux job runs them beside `swift test`.
#
#   macos/packaging/test-signing.sh
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
FAILED=0

# Runs a snippet with signing.sh loaded and only the given variables set.
run() {
    env -i PATH="$PATH" SNIPPET="$SNIPPET" "$@" bash -c '. "$0/signing.sh"; eval "$SNIPPET"' "$HERE" 2>&1
}

expect() {
    local name="$1" want_status="$2" want_text="$3" got status
    shift 3
    got="$(run "$@")"
    status=$?
    if [ "$status" -ne "$want_status" ] || ! printf '%s' "$got" | grep -qF -- "$want_text"; then
        echo "FAIL $name: status $status (want $want_status), output: $got"
        FAILED=1
    else
        echo "ok   $name"
    fi
}

SNIPPET='release_signing_mode'
expect "nothing set is unsigned" 0 "none"
expect "everything with a profile" 0 "developer-id" \
    DEVELOPER_ID_APPLICATION="Developer ID Application: A (T)" DEVELOPER_ID_INSTALLER="Developer ID Installer: A (T)" NOTARY_PROFILE=p
expect "everything with an API key" 0 "developer-id" \
    DEVELOPER_ID_APPLICATION=a DEVELOPER_ID_INSTALLER=i NOTARY_KEY=/k.p8 NOTARY_KEY_ID=id NOTARY_ISSUER=iss
expect "an identity alone is refused" 1 "missing: DEVELOPER_ID_INSTALLER NOTARY_PROFILE" \
    DEVELOPER_ID_APPLICATION=a
expect "notarization alone is refused" 1 "missing: DEVELOPER_ID_APPLICATION DEVELOPER_ID_INSTALLER" \
    NOTARY_PROFILE=p
expect "a partial API key is refused" 1 "missing: NOTARY_ISSUER" \
    DEVELOPER_ID_APPLICATION=a DEVELOPER_ID_INSTALLER=i NOTARY_KEY=/k.p8 NOTARY_KEY_ID=id
expect "a profile and a key at once are refused" 1 "not both" \
    DEVELOPER_ID_APPLICATION=a DEVELOPER_ID_INSTALLER=i NOTARY_PROFILE=p NOTARY_KEY=/k NOTARY_KEY_ID=id NOTARY_ISSUER=iss

SNIPPET='notary_mode'
expect "notary: none" 0 "none"
expect "notary: profile" 0 "profile" NOTARY_PROFILE=p
expect "notary: key" 0 "key" NOTARY_KEY=/k NOTARY_KEY_ID=id NOTARY_ISSUER=iss
expect "notary: partial key" 1 "missing: NOTARY_KEY_ID NOTARY_ISSUER" NOTARY_KEY=/k

SNIPPET='notary_args | tr "\n" " "'
expect "notary args: profile" 0 "--keychain-profile p" NOTARY_PROFILE=p
expect "notary args: key" 0 "--key /k --key-id id --issuer iss" NOTARY_KEY=/k NOTARY_KEY_ID=id NOTARY_ISSUER=iss

# What `xcrun notarytool submit --wait --output-format json` prints. It exits 0 for all
# three, which is why the status is read.
ACCEPTED='{"id":"2efe2717-52ef-43a5-96dc-0797e4ca1041","message":"Processing complete","status":"Accepted"}'
INVALID='{
  "id": "6a5b0d6c-1a1e-4d52-9a3c-3d1fd3a0f9a2",
  "message": "Processing complete",
  "status": "Invalid"
}'
PROGRESS='{"id":"x","message":"Submission is in progress","status":"In Progress"}'
SNIPPET="printf '%s' '$ACCEPTED' | notary_accepted && echo accepted"
expect "notarytool: Accepted passes" 0 "accepted"
SNIPPET="printf '%s' '$INVALID' | notary_accepted || echo rejected"
expect "notarytool: Invalid is a failure" 0 "rejected"
SNIPPET="printf '%s' '$PROGRESS' | notary_accepted || echo rejected"
expect "notarytool: In Progress is a failure" 0 "rejected"
SNIPPET="printf '%s' '$INVALID' | json_field id"
expect "notarytool: the submission id" 0 "6a5b0d6c-1a1e-4d52-9a3c-3d1fd3a0f9a2"
SNIPPET="printf '%s' 'Error: HTTP status code: 401' | notary_accepted || echo rejected"
expect "notarytool: no JSON is a failure" 0 "rejected"

for f in "$HERE"/*.sh "$HERE"/../build.sh "$HERE"/../fetch-postgres.sh; do
    if bash -n "$f"; then echo "ok   bash -n $(basename "$f")"; else echo "FAIL bash -n $f"; FAILED=1; fi
done
for f in "$HERE"/scripts/*; do
    if sh -n "$f"; then echo "ok   sh -n scripts/$(basename "$f")"; else echo "FAIL sh -n $f"; FAILED=1; fi
done

exit "$FAILED"
