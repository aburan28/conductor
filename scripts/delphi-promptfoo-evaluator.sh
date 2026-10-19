#!/usr/bin/env bash
# Feed a Delphi draft to Promptfoo's echo provider and return Delphi's evaluator JSON.
# Set DELPHI_PROMPTFOO_ASSERTIONS to an absolute path containing a JSON array of assertions.
set -euo pipefail

: "${DELPHI_PROMPTFOO_ASSERTIONS:?set DELPHI_PROMPTFOO_ASSERTIONS to a JSON assertions file}"
command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
command -v promptfoo >/dev/null || { echo "promptfoo is required" >&2; exit 1; }
jq -e 'type == "array" and length > 0' "$DELPHI_PROMPTFOO_ASSERTIONS" >/dev/null

work="$(mktemp -d "${TMPDIR:-/tmp}/delphi-promptfoo.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cat > "$work/input.json"
jq -e '.draft | type == "string"' "$work/input.json" >/dev/null
jq -n --slurpfile input "$work/input.json" \
  --slurpfile assertions "$DELPHI_PROMPTFOO_ASSERTIONS" \
  '{prompts:["{{logged_output}}"], providers:["echo"], tests:[{vars:{logged_output:$input[0].draft}, assert:$assertions[0]}]}' \
  > "$work/config.json"

rc=0
promptfoo eval --config "$work/config.json" --output "$work/results.jsonl" \
  --no-share --no-write --no-progress-bar --no-table --no-cache >&2 || rc=$?
# Promptfoo uses 100 for a valid evaluation with failing assertions.
if [[ "$rc" -ne 0 && "$rc" -ne 100 ]]; then
  echo "promptfoo eval failed with exit code $rc" >&2
  exit "$rc"
fi

round="$(jq -r '.round' "$work/input.json")"
hash="$(jq -r '.draft_sha256' "$work/input.json")"
cp "$work/results.jsonl" "promptfoo-r${round}-${hash:0:12}.jsonl"
jq -s '
  if length == 0 then error("promptfoo returned no result rows")
  elif any(.[]; (.success | type) != "boolean" or (.success == false and .gradingResult == null) or .response.error != null) then
    error("promptfoo returned an error or an ungraded row")
  else
    {passed: all(.[]; .success == true),
     score: ((map(.score // (if .success then 1 else 0 end)) | add) / length),
     feedback: (map(select(.success != true) | (.gradingResult.reason // "assertion failed"))
                | join("; ") | if . == "" then "all assertions passed" else . end)}
  end
' "$work/results.jsonl"
