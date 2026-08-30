#!/usr/bin/env bash
#
# End-to-end demonstration of the coordination invariants, on a throwaway repository.
#
# This is the scenario from DESIGN.md §32.5, minus the parts that need a real model: two
# people collide on territory, a duplicate is caught across different wording, private work
# blocks a collision without disclosing itself, and a worker executes a task in an isolated
# worktree with runner-attested validation.
#
# Requires: a running Postgres (make db-up) and the built binaries (make build).

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin"
PORT="${CONDUCTOR_E2E_PORT:-8091}"
ENDPOINT="http://127.0.0.1:$PORT"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/conductor-e2e.XXXXXX")"
export DATABASE_URL="${DATABASE_URL:-postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable}"

# A unique suffix keeps repeated runs from colliding in a shared database.
SUFFIX="$(date +%s)"
ORG="e2e-$SUFFIX"
PROJECT="demo-$SUFFIX"

SERVER_PID=""
MESH_PIDS=""
cleanup() {
  [[ -n "$SERVER_PID" ]] && kill "$SERVER_PID" 2>/dev/null || true
  [[ -n "$MESH_PIDS" ]] && kill $MESH_PIDS 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

step() { printf '\n\033[1m── %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }

for binary in conductord conductor; do
  [[ -x "$BIN/$binary" ]] || { echo "missing $BIN/$binary — run: make build" >&2; exit 1; }
done

step "Preparing a throwaway repository"
mkdir -p "$WORK/repo"
cd "$WORK/repo"
git init -q .
git config user.email e2e@example.com
git config user.name "E2E"
mkdir -p internal/router internal/api
echo "package router" > internal/router/router.go
echo "package api"    > internal/api/api.go
echo "# Demo"         > README.md
git add -A && git commit -qm "initial commit"
"$BIN/conductor" init --dir . >/dev/null
# Give the project a required check so runner-attested validation is exercised.
sed -i.bak 's/^required_checks: \[\]$/required_checks:\n  - test -f README.md/' .conductor/WORKFLOW.md
rm -f .conductor/WORKFLOW.md.bak
git add -A && git commit -qm "add conductor policy"
note "$WORK/repo"

step "Bootstrapping the control plane"
# bootstrap also writes the operator's login file. Alice runs it with a sandboxed HOME so
# the real login is untouched and the saved credentials can be asserted; Bob opts out.
ALICE_HOME="$WORK/alice-home"
mkdir -p "$ALICE_HOME"
ALICE_TOKEN=$(HOME="$ALICE_HOME" "$BIN/conductord" bootstrap --org "$ORG" --project "$PROJECT" \
  --principal alice --repo "$WORK/repo" | grep -o 'cdt_[A-Za-z0-9_-]*')
ALICE_CREDS="$ALICE_HOME/.conductor/credentials"
[[ -f "$ALICE_CREDS" ]]            || fail "bootstrap did not save the CLI login"
grep -q '"handle": *"alice"'       "$ALICE_CREDS" || fail "saved login has the wrong handle"
grep -q "\"project\": *\"$PROJECT\"" "$ALICE_CREDS" || fail "saved login has the wrong project"
grep -q "\"token\": *\"$ALICE_TOKEN\"" "$ALICE_CREDS" || fail "saved login does not carry the minted token"
note "bootstrap logged alice in on this machine (no token copy-paste)"

BOB_HOME="$WORK/bob-home"
mkdir -p "$BOB_HOME"
BOB_TOKEN=$(HOME="$BOB_HOME" "$BIN/conductord" bootstrap --org "$ORG" --project "$PROJECT" \
  --principal bob --role contributor --repo "$WORK/repo" --no-login | grep -o 'cdt_[A-Za-z0-9_-]*')
[[ ! -e "$BOB_HOME/.conductor/credentials" ]] || fail "--no-login still wrote a login file"
note "--no-login leaves the machine alone (bob authenticates via env vars)"

"$BIN/conductord" --addr "127.0.0.1:$PORT" >"$WORK/conductord.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 40); do
  curl -sf "$ENDPOINT/v1/health" >/dev/null 2>&1 && break
  sleep 0.25
done
curl -sf "$ENDPOINT/v1/health" >/dev/null || { echo "control plane did not start" >&2; cat "$WORK/conductord.log" >&2; exit 1; }
note "listening on $ENDPOINT"

alice() { CONDUCTOR_ENDPOINT=$ENDPOINT CONDUCTOR_TOKEN=$ALICE_TOKEN CONDUCTOR_PROJECT=$PROJECT "$BIN/conductor" "$@"; }
bob()   { CONDUCTOR_ENDPOINT=$ENDPOINT CONDUCTOR_TOKEN=$BOB_TOKEN   CONDUCTOR_PROJECT=$PROJECT "$BIN/conductor" "$@"; }

fail() { printf '\n\033[31mFAILED: %s\033[0m\n' "$*"; exit 1; }

step "1. Alice checks before editing — the field is clear"
alice check --summary "Add retry-aware model routing to the router" --scope dir:internal/router

step "2. Alice files and claims the work"
alice task create --title "Add retry-aware model routing" \
  --objective "Deterministic rules before any classifier" --scope dir:internal/router
alice task claim T-1 --scope dir:internal/router

step "3. Bob independently tries the same territory — blocked, with who and what to do"
set +e
bob check --summary "retry aware routing for models" --scope path:internal/router/router.go
BOB_EXIT=$?
set -e
[[ $BOB_EXIT -eq 3 ]] || fail "expected exit 3 (blocked), got $BOB_EXIT"

step "4. Alice files sensitive work as private"
alice task create --title "Rotate the leaked production key" \
  --objective "Credentials were exposed" --visibility private --scope dir:internal/api

step "5. Bob sees the territory but not the intent"
BOARD=$(bob task list)
echo "$BOARD"
echo "$BOARD" | grep -q "(private)"        || fail "private task title was not redacted"
echo "$BOARD" | grep -q "dir:internal/api" || fail "private task territory was hidden, so collisions are possible"
if echo "$BOARD" | grep -qi "leaked"; then fail "private task intent leaked to another principal"; fi
note "redacted title, visible territory — exactly the tradeoff the design makes"

step "6. Bob is still blocked from that territory"
set +e
bob check --summary "refactor api handlers" --scope dir:internal/api
BOB_EXIT=$?
set -e
[[ $BOB_EXIT -eq 3 ]] || fail "expected exit 3 on private-task territory, got $BOB_EXIT"

step "7. Duplicate detection across different wording, with no shared files"
alice task create --title "Implement the team invite flow with email invitations and acceptance" >/dev/null
DUP=$(bob check --summary "Build team invitation flow: send invite emails, accept invitations")
echo "$DUP"
echo "$DUP" | grep -q "suggest_join" || fail "reworded duplicate was not detected"

step "8. Unrelated work is not a false positive"
CLEAR=$(bob check --summary "Upgrade the Postgres connection pool and add query timeouts")
echo "$CLEAR"
echo "$CLEAR" | grep -q "Clear" || fail "unrelated work was incorrectly flagged"

step "9. Onboarding a coworker without database access"
INVITE=$(alice member add rachel --role contributor --json)
RACHEL_TOKEN=$(printf '%s' "$INVITE" | sed -n 's/.*"token": *"\([^"]*\)".*/\1/p')
[[ -n "$RACHEL_TOKEN" ]] || fail "invite did not mint a token"
rachel() { CONDUCTOR_ENDPOINT=$ENDPOINT CONDUCTOR_TOKEN=$RACHEL_TOKEN CONDUCTOR_PROJECT=$PROJECT "$BIN/conductor" "$@"; }
rachel task list >/dev/null || fail "the minted token does not work"
note "rachel can read the project with a token alice issued over the API"

set +e
rachel member add mallory --role project_admin >/dev/null 2>&1
ESCALATE=$?
set -e
[[ $ESCALATE -ne 0 ]] || fail "a contributor was able to invite an admin"
note "a contributor cannot invite; privilege does not escalate"

step "9b. One-link onboarding: invite prints a join link, join redeems it"
INVITE_JSON=$(alice invite dana --role contributor --json)
JOIN_URL=$(printf '%s' "$INVITE_JSON" | python3 -c 'import sys,json;print(json.load(sys.stdin)["join_url"])')
# The token must ride in the fragment (after '#'), never in the query string.
FRAG="${JOIN_URL#*#}"
case "$JOIN_URL" in *"?token="*) fail "join link leaks the token in the query string: $JOIN_URL";; esac
case "$FRAG" in *"token=cdt_"*) : ;; *) fail "join link does not carry the token in the fragment: $JOIN_URL";; esac
# Redeem it as the teammate would — sandboxed to a throwaway HOME so the real credentials
# file is never touched.
FAKE_HOME="$WORK/dana-home"
mkdir -p "$FAKE_HOME"
JOINED=$(HOME="$FAKE_HOME" "$BIN/conductor" join "$JOIN_URL" --json)
JOINED_HANDLE=$(printf '%s' "$JOINED" | python3 -c 'import sys,json;print(json.load(sys.stdin)["handle"])')
[[ "$JOINED_HANDLE" == "dana" ]] || fail "join did not authenticate as dana (got: $JOINED_HANDLE)"
[[ -f "$FAKE_HOME/.conductor/credentials" ]] || fail "join did not save credentials"
note "invite emitted a fragment-form link; join redeemed it and logged dana in"

step "10. A worker executes a task, reaching the control plane over HTTP only"
# No DATABASE_URL in this environment: the runner holds no database credential.
WORKER_LOG="$WORK/worker.log"
env -u DATABASE_URL CONDUCTOR_ENDPOINT=$ENDPOINT CONDUCTOR_TOKEN=$ALICE_TOKEN \
  CONDUCTOR_PROJECT=$PROJECT "$BIN/conductor" worker --dry-run succeed --once -v \
  >"$WORKER_LOG" 2>&1 || fail "worker exited non-zero"
grep -vE 'level=DEBUG' "$WORKER_LOG" | tail -6

grep -q "runner connecting over HTTP" "$WORKER_LOG" || fail "runner did not use the HTTP backend"
grep -q "claimed task" "$WORKER_LOG"                || fail "worker claimed nothing"
WORKED_REF=$(sed -n 's/.*msg="attempt finished" task=\([A-Za-z0-9-]*\).*/\1/p' "$WORKER_LOG" | head -1)
[[ -n "$WORKED_REF" ]] || fail "worker never finished an attempt"
note "worker completed $WORKED_REF with no database credential"

step "11. The attempt is verifiable: evidence, worktree, provenance, routing"
# DESIGN.md §31.9-§31.12. Until now this step printed output and asserted nothing, so a
# runner that silently did nothing would still have passed.
# Extract the first match only: the payload has several "id" fields (the task, then each
# artifact), and a multi-line capture turns the URL below into something curl rejects.
field() { printf '%s' "$2" | sed -n "s/.*\"$1\": *\"\([^\"]*\)\".*/\1/p" | head -1; }

TASK_JSON=$(alice task show "$WORKED_REF" --json) || fail "task show failed"
TASK_ID=$(field id "$TASK_JSON")
[[ -n "$TASK_ID" ]] || fail "could not read the task id from task show output"
ATTEMPTS_JSON=$(curl -sf -H "Authorization: Bearer $ALICE_TOKEN" \
  "$ENDPOINT/v1/tasks/$TASK_ID/attempts") || fail "could not read attempts for $WORKED_REF"

# §31.9 a completed task has a commit and validation evidence
printf '%s' "$TASK_JSON" | grep -q '"commit_sha"'  || fail "no commit recorded for $WORKED_REF"
printf '%s' "$TASK_JSON" | grep -q '"validation"'  || fail "no validation evidence for $WORKED_REF"
printf '%s' "$TASK_JSON" | grep -q '"exit_code"'   || fail "validation carries no runner-observed exit code"

# §31.11 autonomous runs execute in per-task worktrees
WORKTREE=$(field worktree_path "$ATTEMPTS_JSON")
[[ "$WORKTREE" == *"worktrees"* ]] || fail "attempt did not run in a worktree (got '$WORKTREE')"
BRANCH=$(field branch "$ATTEMPTS_JSON")
[[ "$BRANCH" == agent/* ]] || fail "attempt branch '$BRANCH' is not a per-task agent branch"

# §31.10 workflow and config hashes are recorded on every attempt
[[ -n "$(field workflow_sha "$ATTEMPTS_JSON")" ]]        || fail "no workflow hash recorded"
[[ -n "$(field project_config_sha "$ATTEMPTS_JSON")" ]]  || fail "no project config hash recorded"

# §31.12 model alias and reasoning effort are recorded
[[ -n "$(field model_alias "$ATTEMPTS_JSON")" ]]      || fail "no model alias recorded"
[[ -n "$(field reasoning_effort "$ATTEMPTS_JSON")" ]] || fail "no reasoning effort recorded"

note "commit + evidence, worktree $BRANCH, workflow/config hashes, alias $(field model_alias "$ATTEMPTS_JSON")/$(field reasoning_effort "$ATTEMPTS_JSON")"

step "12. Presence shows who is live, on what, without exposing chats"
SESSION=$(curl -s -X POST -H "Authorization: Bearer $BOB_TOKEN" -H 'Content-Type: application/json' \
  -d '{"harness":"claude-code","branch":"feature/bob"}' "$ENDPOINT/v1/projects/$PROJECT/sessions")
[[ -n "$SESSION" ]] || fail "could not register a session"
PRESENCE=$(alice presence --json)
printf '%s' "$PRESENCE" | grep -q '"principal": *"bob"' || fail "bob is not visible in presence"
printf '%s' "$PRESENCE" | grep -q 'feature/bob'          || fail "presence does not show the branch"
printf '%s' "$PRESENCE" | grep -q 'last_heartbeat'       || fail "presence does not show liveness"
for banned in prompt transcript conversation reasoning; do
  if printf '%s' "$PRESENCE" | grep -qi "\"$banned"; then
    fail "presence exposed a '$banned' field"
  fi
done
note "presence shows principal, harness, branch, and heartbeat — and nothing else"

step "12b. The project's session history can be exported, including sessions presence no longer shows"
BOB_SESSION_ID=$(field id "$SESSION")
curl -s -o /dev/null -X POST -H "Authorization: Bearer $BOB_TOKEN" "$ENDPOINT/v1/sessions/$BOB_SESSION_ID/close"
alice sessions export --output "$WORK/sessions.json" >/dev/null
[[ -s "$WORK/sessions.json" ]] || fail "sessions export wrote nothing"
grep -q "\"$BOB_SESSION_ID\"" "$WORK/sessions.json" || fail "the closed session is missing from the export"
grep -q '"state": *"closed"' "$WORK/sessions.json"      || fail "the closed session is not marked closed"
grep -q '"exported_at"' "$WORK/sessions.json"           || fail "the export has no envelope"
for banned in prompt transcript conversation reasoning_text; do
  if grep -qi "\"$banned" "$WORK/sessions.json"; then
    fail "the session export exposed a '$banned' field"
  fi
done
ONE=$(alice sessions export "${BOB_SESSION_ID:0:8}" --output -)
printf '%s' "$ONE" | grep -q '"count": *1' || fail "exporting one session by prefix did not select exactly one: $ONE"
note "exported $(grep -c '"principal"' "$WORK/sessions.json") session(s) to $WORK/sessions.json; a closed one is still in it"

step "13. Work is offered to a session that meets a capability floor"
# Two sessions, same person, different models: one cheap, one frontier. The catalog decides
# what each is worth — the session's own claim about its tier is ignored.
CHEAP_SESSION=$(curl -s -X POST -H "Authorization: Bearer $BOB_TOKEN" -H 'Content-Type: application/json' \
  -d '{"harness":"claude","capabilities":{"model":"claude-haiku-4-5-20251001","reasoning_effort":"low","tier":"T4"}}' \
  "$ENDPOINT/v1/projects/$PROJECT/sessions")
STRONG_SESSION=$(curl -s -X POST -H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
  -d '{"harness":"claude","capabilities":{"model":"claude-opus-5","reasoning_effort":"high","max_reasoning_effort":"xhigh"}}' \
  "$ENDPOINT/v1/projects/$PROJECT/sessions")
STRONG_ID=$(field id "$STRONG_SESSION")
[[ -n "$STRONG_ID" ]] || fail "could not register the frontier session"

# The cheap session declared T4. The catalog says haiku is T1, and the catalog wins.
printf '%s' "$CHEAP_SESSION" | grep -q '"tier": *"T1"' \
  || fail "a session promoted itself past the catalog: $CHEAP_SESSION"

CAPS=$(alice capabilities --json)
printf '%s' "$CAPS" | grep -q '"max_reasoning_effort": *"xhigh"' \
  || fail "the inventory does not report the xhigh ceiling"

alice task create --title "Design the escalation ladder" >/dev/null
ASSIGN=$(alice task assign T-4 --require-tier T3 --require-effort xhigh --json)
printf '%s' "$ASSIGN" | grep -q "$STRONG_ID" \
  || fail "work was not offered to the frontier session: $ASSIGN"
printf '%s' "$ASSIGN" | grep -q 'is below the required' \
  || fail "the refusal of the cheaper session was not explained: $ASSIGN"

# A floor nothing live can meet is refused with the ceiling that actually exists, not a
# silent downgrade to whoever is free.
set +e
UNPLACED=$(alice task assign T-3 --require-tier T4 --require-effort max --json 2>&1)
UNPLACED_CODE=$?
set -e
[[ "$UNPLACED_CODE" == "3" ]] || fail "an unservable floor should exit 3, got $UNPLACED_CODE"
printf '%s' "$UNPLACED" | grep -q 'max_tier' \
  || fail "the refusal did not report what is actually available: $UNPLACED"

# The offer reaches the session it was made to, and nobody else's.
INBOX=$(alice inbox --session "$STRONG_ID" --json)
printf '%s' "$INBOX" | grep -q '"task_ref": *"T-4"' || fail "the offer is not in the session's inbox"
DENIED=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $BOB_TOKEN" \
  "$ENDPOINT/v1/sessions/$STRONG_ID/assignments")
[[ "$DENIED" == "404" ]] || fail "another principal could read the inbox ($DENIED)"
note "T-4 offered to the xhigh-capable session; a T4/max floor is refused with the real ceiling"

step "14. Auth throttling protects the credential endpoint"
for _ in $(seq 1 12); do
  curl -s -o /dev/null -H "Authorization: Bearer cdt_wrong" "$ENDPOINT/v1/whoami" || true
done
THROTTLED=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer cdt_wrong" "$ENDPOINT/v1/whoami")
[[ "$THROTTLED" == "429" ]] || fail "expected 429 after repeated auth failures, got $THROTTLED"
VALID=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ALICE_TOKEN" "$ENDPOINT/v1/whoami")
[[ "$VALID" == "200" ]] || fail "a valid token was throttled ($VALID); one bad actor would lock out a shared NAT"
note "bad credentials throttled, good ones unaffected"

step "15. Project status"
alice status

step "16. Policy files validate, and routing is explainable before a token is spent"
alice policy lint >/dev/null || fail "the scaffolded policy files did not lint clean"
# Route preview: a task's model choice is answerable without dispatching it.
alice route T-1 >/dev/null || fail "route preview failed for T-1"
note 'policy lint passed; conductor route explained the decision without running anything'

step "17. The admission queue admits work and tracks it"
T_A=$(alice task create --title "queued work A" --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["ref"])')
TICK=$(curl -s -H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"kind\":\"attempt\",\"task\":\"$T_A\"}" "$ENDPOINT/v1/projects/$PROJECT/queue")
STATE=$(echo "$TICK" | python3 -c 'import sys,json;print(json.load(sys.stdin)["state"])')
case "$STATE" in granted|queued) ;; *) fail "queue returned an unexpected ticket state: $STATE";; esac
alice queue >/dev/null || fail "queue view failed"
note "the admission queue issued a ticket ($STATE) and lists it in conductor queue; a session cap makes the rest wait"

step "18. The swarm view rolls up who is contributing capacity"
alice swarm >/dev/null || fail "swarm view failed"
note "runners, live sessions, and shareable budget in one view"

step "19. Daemon-to-daemon peering: two control planes, one mesh, mutual TLS"
# A second and third daemon join a private CA and dial each other. This is the mesh layer:
# authenticated connectivity between daemons, with no data replicated across the link.
MESH_PORT=$((PORT + 400))
MESH_CERTS="$WORK/mesh-certs"
CONDUCTOR_CERT_DIR="$MESH_CERTS" "$ROOT/scripts/gen-peer-certs.sh" mesh-a mesh-b >/dev/null
note "generated a mesh CA and two daemon certificates"

"$BIN/conductord" --addr "127.0.0.1:$MESH_PORT" --no-scheduler \
  --peer-ca "$MESH_CERTS/ca.pem" --peer-cert "$MESH_CERTS/mesh-a/cert.pem" \
  --peer-key "$MESH_CERTS/mesh-a/key.pem" --peer "mesh-b=https://127.0.0.1:$((MESH_PORT + 1))" \
  >"$WORK/mesh-a.log" 2>&1 &
MESH_PIDS="$MESH_PIDS $!"
"$BIN/conductord" --addr "127.0.0.1:$((MESH_PORT + 1))" --no-scheduler \
  --peer-ca "$MESH_CERTS/ca.pem" --peer-cert "$MESH_CERTS/mesh-b/cert.pem" \
  --peer-key "$MESH_CERTS/mesh-b/key.pem" --peer "mesh-a=https://127.0.0.1:$MESH_PORT" \
  >"$WORK/mesh-b.log" 2>&1 &
MESH_PIDS="$MESH_PIDS $!"
sleep 2

MESH_A="https://127.0.0.1:$MESH_PORT"
# The link table shows mesh-b as up, with the identity its certificate carries.
PEERS=$(curl -sf --cacert "$MESH_CERTS/ca.pem" -H "Authorization: Bearer $ALICE_TOKEN" "$MESH_A/v1/peers")
echo "$PEERS" | grep -q '"state": *"up"'     || fail "peer link did not come up: $PEERS"
echo "$PEERS" | grep -q '"name": *"mesh-b"' || fail "peer did not report its mesh identity: $PEERS"

# Peer endpoints are authenticated by certificate, not by bearer token.
NO_CERT=$(curl -s -o /dev/null -w '%{http_code}' --cacert "$MESH_CERTS/ca.pem" "$MESH_A/v1/peer/info")
[[ "$NO_CERT" == "401" ]] || fail "peer endpoint answered without a client certificate ($NO_CERT)"
INFO=$(curl -sf --cacert "$MESH_CERTS/ca.pem" --cert "$MESH_CERTS/mesh-b/cert.pem" \
  --key "$MESH_CERTS/mesh-b/key.pem" "$MESH_A/v1/peer/info")
echo "$INFO" | grep -q '"name": *"mesh-a"' || fail "peer info did not identify the answering daemon: $INFO"

# And the CLI sees the same mesh.
CLI_PEERS=$(CONDUCTOR_ENDPOINT="$MESH_A" CONDUCTOR_CA_CERT="$MESH_CERTS/ca.pem" \
  CONDUCTOR_TOKEN="$ALICE_TOKEN" "$BIN/conductor" peers)
echo "$CLI_PEERS" | grep -q "mesh-b" || fail "conductor peers did not list mesh-b: $CLI_PEERS"
note "$CLI_PEERS" | grep -m1 mesh-b

printf '\n\033[32mAll end-to-end checks passed.\033[0m\n'
