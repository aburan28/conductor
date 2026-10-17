# Local-first P2P implementation roadmap

Status: proposed backlog, not implemented features. This accompanies the
[architecture and source audit](local-first-p2p-architecture.md), pinned to
Conductor `3244ecbbc7a01229e3a33588994ef445305e2660` and Cairn
`d782a50822b93ff7189cb260674e648eb6697074`. Each slice needs its own scoped claim,
PR, evidence and applicable human/security review. Do not treat this planning PR
as approval to change cryptography, identity or coordination semantics.

## First product acceptance

The first complete demonstration is one bounded user journey, not separate
passing login and packet tests:

1. On laptop A choose an explicit authority/profile, sign in using an allowed
   Google or GitHub account, create/select a private workspace, and produce a
   supported harness transcript, dirty tracked file and selected untracked file.
   The durable local catalog includes selected account/task/session metadata.
2. Restart the app/device. With the backend unavailable, open history and pinned
   files from local storage. Observe stale task/account state and denied online
   claims/publication. Reattach a supported live agent or resume a checkpoint only
   under fresh authority; never replay tools or historical jobs automatically.
3. Enroll laptop B through authenticated account access **and** verified device
   approval/key recovery. Retrieve verified content and native continuation where
   supported. Prove the saved contents match, omissions are visible, local root
   remapping is explicit, and OAuth alone cannot unlock the vault.
4. Repeat with A asleep: an opted-in encrypted backup/private replica allows
   restore; with neither replica nor backup, show unavailable bytes rather than
   empty/restored content. A placeholder remains a placeholder.
5. Disconnect B and edit a file on both devices. Reconnect to display both
   concurrent versions and an explicit resolution. Expired/reclaimed task fences
   are refused even if a queued content operation is accepted.
6. Revoke one device, then one membership. Remaining authorized devices work;
   future admission/key distribution is refused for revoked identities. Existing
   downloaded plaintext remains an explicit limitation.

This initial journey uses a constrained LAN/private WireGuard path. The later
public relay and cloud store-forward journeys extend it; they are not prerequisites
for proving the first direct two-device flow. If encrypted cloud persistence is
not selected by the user, substitute an explicitly chosen private durable node.
The public MVP release gate nevertheless requires Slice 6: two CGNAT/UDP-blocked
laptops must connect without requiring administrator/kernel VPN setup. A direct
LAN/WireGuard proof alone is not public-product readiness.

Provisional product choices: restore history/workspaces/files plus supported
continuation rather than live process migration; permit encrypted cloud persistence
only by explicit opt-in. Record the final choices before implementing retention/UI.

## Delivery rules

- Preserve existing PostgreSQL principal/project/task/lease/fencing semantics.
  A local read model is a cache/projection, not an offline coordination authority.
  Local-only and remote authority namespaces are explicit and never auto-merged.
- Reuse Cairn Lab's private operation/file machinery through a verified adapter.
  Do not use its public Lab HTTP publication routes, research ledger, public DHT
  or verifier blob path for confidential content.
- Personal sessions are private by default; team sharing requires explicit scoped
  visibility. Offline local content is available independently of cloud uptime.
- No sync engine, identity replacement, cipher changes, dependencies, device
  protocol or runtime feature is delivered by these two planning documents.
- Scope expansions, new schemas and security/crypto changes need review under
  `.conductor/WORKFLOW.md`. Add migrations to the existing database deliberately;
  do not create a parallel account database or multi-master PostgreSQL sync.
- Every feature claim requires an executable acceptance test using actual native
  components, including negative controls; mocks may test units but cannot prove
  the product journey. Preserve failed attempts and exact environment limits.

## Gate 0 — contracts and threat model (planning / spikes)

Deliver a reviewed decision record before selecting protocols or library changes.

| Work item | Concrete output | Acceptance / stop condition |
| --- | --- | --- |
| Product scope | Final restore and cloud-persistence choices; privacy/default-sharing policy | Explicit distinction between content/history, credential restore, native continuation and live process reattachment |
| Authority and cache contract | Authority IDs, selected local account/task/session read model, proposed outbox states and offline rules | No queued claim/renew/finish can acquire authority by replay; external credential failure cannot select a different database |
| Lab adapter spike | Test internal Lab `write_file`/`delete_file`/`read` and conditional expectations through library/CLI or authenticated local IPC; capability/auth mapping to canonical Conductor IDs | Conductor removal/revocation cannot leave an independently authorized shadow Lab member. Otherwise stop for an ADR |
| Confidentiality spike | Versioned encrypted payload/manifest/key-envelope compatibility with Lab causal authorization and conflict resolution | Preserve canonical signatures/authorization; disclose remaining clear headers; no generic E2EE claim if compatibility is unproven |
| Trust bootstrap | Verified-device admission, first-device TOFU boundary, owner recovery and optional independent witnesses | Malicious directory key substitution cannot silently add a reader; document all-witness-loss/rollback limits |
| Transport / license ADR | One direct transport and later relay option respecting Cairn cipher policy; exact dependency versions/SBOM/notices | No prohibited TLS stack silently added to Cairn; no NetBird AGPL component presented as BSD; no Headscale multi-tenant claim |
| Durable storage ADR | Lab store guarantees, derived local index transaction boundary, queue/receipt recovery, schema migration and quota policy | Specify fsync/atomic publication, corruption recovery, index rebuild and complete-vs-cache semantics |

Existing inspection targets: `internal/api/sso.go`, `internal/db/identity.go`,
`internal/db/claim.go`, `internal/localstate`, `internal/checkpoint`,
`macos/Sources/ConductorKit/Supervisor.swift`, `cairn/src/lab/{api,store,sync}.rs`.
These identify reuse seams; new module/API names are design outputs, not existing
implementations. Result: approved contracts and fixture prototypes, not a claim
that the full journey works.

## Slice 1 — explicit startup profile and honest local restoration

Candidate PR: `feat: select an explicit authority and restore the local catalog`.

Scope: desktop/CLI profile selection, local read-model lifecycle and existing
resume/checkpoint UX. Reuse `Supervisor`, `up`, client credentials, localstate and
checkpoint inventories. Fix authority selection before adding hidden retry logic.

Required behavior:

- Persist authority identity, endpoint and mode independently of secret retrieval.
  A configured external DSN with an unavailable Keychain entry fails visibly.
  A remote API outage does not boot an unrelated local authority.
- Restore an authenticated device-local catalog/read model before remote probes;
  selected metadata has provenance, fetched version/time and offline indicators.
  Do not persist provider secrets/transcripts in unprotected browser storage.
- Distinguish locked vault, signed-out account, stale offline membership, available
  history/files, omitted checkpoint layers and unavailable content.
- Reproduce the static `resume.All` executable-selection concern under an actual
  daemon before fixing it; route to the intended CLI/runtime explicitly if proven.
  Restore UI never reruns jobs/tools or reuses old lease epochs.

Acceptance: same-laptop launch/restart while backend DNS/TCP is denied, failed
credential retrieval, corrupted/old local index, surviving vs stopped harness,
unsupported format, missing files and revoked token. Existing local work must
survive; online mutations must fail closed. Unit and native subprocess tests run
in normal CI; platform Keychain/launchd assertions need a macOS gate with evidence.

## Slice 2 — crash-safe local content and export

Candidate PR: `feat: retain selected workspace history as durable local content`.
Depends on Gate 0 storage/adapter contracts and Slice 1.

Scope: Lab-backed private workspace catalog, checkpoint adapter and derived
metadata/outbox index. Keep existing complete local files authoritative on the
device; add explicit retention/pinning and portable export.

Required behavior:

- Ingest native transcript/checkpoint manifests with completeness inventory,
  virtual workspace IDs, repository/base commit and supported harness version.
  Preserve existing capture size/exclusion behavior as visible omissions.
- Transactionally publish durable blob references and queue/read-model updates.
  Recover a crash between blob write, fsync, rename, index commit and ACK.
- Add a bounded filesystem watcher plus periodic reconciliation into the durable
  outbox; coalesce bursts and recover missed events after restart. Existing Lab
  explicit commit/sync is not an automatic watcher or background synchronizer.
- Retain personal/private defaults and selected local task/account/session
  snapshots. The account snapshot never becomes a credential or offline RBAC
  source. Outbox items are content operations or explicit pending user proposals.
- Export a verified portable bundle without credentials, absolute machine roots,
  `.git` internals or PostgreSQL directories. Require unlock for private content.

Acceptance: kill/power-loss fault injection at every durable boundary, disk-full,
corrupt/truncated manifest/blob, index rebuild, missing/evicted object, portable
export/import, accidental secret inclusion and cross-project path rejection.
An ACK must not precede the documented persistence guarantee. No network sync
claim is made at this slice.

## Slice 3 — device admission, recipient verification and recovery

Candidate PRs: `feat: enroll devices against existing project authority` and
`feat: verify content recipients and recover a device vault`.
Depends on Gate 0 authorization/E2EE/trust decisions.

Scope: additive device/admission records in the existing authority, signed
challenge/approval bindings, local OS vault adapters and key-envelope handling.
Use existing Google/GitHub/OIDC sign-in and account-linking flows rather than
adding a second provider identity system.

Required behavior:

- Provider login alone cannot enroll a content reader or decrypt old files.
  Bind device keys to account, authority, project scopes, nonce and expiry.
- Verify approval/key fingerprints using an already trusted device, independent
  recovery secret or declared enterprise escrow. Persist roster high-water marks
  and expose initial TOFU/witness trust assumptions.
- Define token, principal, membership, device and provider-unlink effects
  separately. Reject revoked enrollment/publication; rotate future recipient
  epochs without claiming past downloads can be revoked.
- Handle multiple linked providers under current issuer/subject uniqueness and
  single-organization principal semantics. Tenant switch never leaks keys/caches.
- Local/air-gapped authentication uses an approved local/internal identity path;
  no public provider is a hidden required dependency.

Acceptance: Alice/Bob/outsider, wrong account/device/project, spoofed issuer,
replayed/expired challenge, malicious backend substituted key, old signed roster,
parallel approvals, last-device loss, unavailable recovery custodian, explicit
escrow audit and no-secret logs. Prove updated online authorities/key distributors
refuse new key envelopes after app admission revocation, and stale distributors
stop issuance at snapshot expiry, while an unaffected device still reads its
permitted content. Previously cached keys/plaintext remain outside that refusal.
Human security/cryptography review is required before broad rollout.

## Slice 4 — direct two-device content synchronization

Candidate PR: `feat: reconcile private workspace content between verified devices`.
Depends on Slices 1–3 and the reviewed encrypted Lab adapter.

Scope: supported authenticated direct LAN/private-WireGuard carrier and explicit
device discovery candidates, verified operation reconciliation and UI conflict
states. Reuse Lab operation IDs, causal closure, conditional writes and
multi-value registers. Do not build another CRDT in Go.

Required behavior:

- Enforce scope before inventory/bytes; use signed/verified membership-device
  bindings and encrypted private content without public Lab publication routes.
- Deduplicate/idempotently ingest operations and causal predecessors. Reject bad
  signatures, ID/body mismatches, unrelated authority/key epochs and unreferenced
  uploads. Record durable frontier/object receipts.
- Transfer selected content only; maintain pinned/full vs placeholder/cache states.
  Duplicate/reordered notifications never turn into duplicate side effects.
- Preserve concurrent edits/deletes/renames with explicit resolution. Handle
  case/Unicode/name/symlink boundaries without overwriting another file/root.
- Sync content after online RBAC revalidation; revalidate task claims independently.
  A valid content signature never authorizes task completion or secret retrieval.

Acceptance: execute the first product journey through same-laptop restart and
approved second-laptop restore, backend outage and no-peer/no-backup refusal.
Use the real Conductor authority and Lab engine; retain current WireGuard fixture
as an isolated transport option. Include concurrent text/binary edits, delete/edit,
partial causal history, stale membership, explicit revocation, private session
visibility, cross-tenant inventory refusal and unaffected-peer positive controls.
No Internet traversal, relay, automatic failover or process migration claim yet.

## Slice 5 — bounded, resumable large-content transfer

Candidate PR: `feat: resume verified encrypted content transfers after interruption`.
Depends on Slice 4; may be developed alongside its bounded pilot.

Scope: versioned encrypted chunk manifests, durable transfer journal and receiver
resource controls. Extend Lab through reviewed format/protocol versioning; its
current whole-blob streaming is not crash-resumable chunk transfer.

Required behavior:

- Ciphertext-addressed chunks, authenticated total/order/root and bounded lengths.
  Keep plaintext filenames/hashes out of public discovery/store indexes.
- Resume from verified chunk boundaries after process/network interruption;
  persisted receipts are idempotent and do not trust an HTTP byte offset alone.
- Reserve disk budget before accepting data; implement concurrency/backpressure,
  timeouts, fair scheduling, retry budgets, per-workspace/device/tenant quotas and
  user cancellation. Prevent resource/decompression bombs.
- GC respects pins, retained versions, tombstones, offline-replica windows and
  backup receipts. Evictable cache never silently removes a promised local copy.

Acceptance: multi-gigabyte fixture at supported resource limits, disconnect/kill
at each chunk boundary, reordered/corrupt/truncated/replayed chunks, manifest
substitution, disk-full/low-space, slow receiver, concurrent tenants, restart and
quota exhaustion. Measure retransmitted bytes, memory high-water mark, retained
queue size and final verified root; publish measured bounds rather than invent
throughput or chunk-size guarantees.

## Slice 6 — explicit relay and optional encrypted store-forward

Candidate PRs: `feat: relay authenticated private content over restricted networks`
and `feat: retain opted-in ciphertext for sleeping devices`.
Depends on Slices 3–5 and transport/license ADR.

Scope: separate transport adapter/relay service if needed to respect Cairn's
cipher/dependency policy, plus optional S3-compatible encrypted spool. Avoid a
second metadata/account authority; relay capabilities derive from canonical
admission and are short-lived/bounded.
Evaluate a Go-side HTTPS/WebSocket/object-store carrier through the versioned
local IPC bridge, preserving inner endpoint-authenticated encryption. No raw FFI,
new competing overlay control plane or Cairn TLS-policy exception is assumed.

Required behavior:

- Direct-first dialing with explicit policy-approved candidates and bounded
  fallback to configured relays. Private/air-gap profiles cannot try public seeds,
  DHTs, public relay URLs or hidden provider endpoints.
- Distinguish packet forwarding, encrypted object store-forward and permanent
  backup. Holder receipts specify object set, retention and durable acknowledgment.
- Cloud cannot decrypt payloads or silently substitute recipients; apply verified
  device roster/key-envelope policy even if hosting is compromised.
- Hosted tenant quotas, abuse controls, cleanup/expiration, upload authorization
  and cost caps are enforced before receiving arbitrary content.

Acceptance: distinct NAT/private-network fixtures, blocked UDP, proxy-only HTTPS
where supported, direct-path failure, relay interruption/reconnect, source asleep,
expired receipt, hostile/unavailable relay, malicious directory and tenant isolation.
Egress tripwires prove no public fallback under private/air-gap profiles. Prove
restore works from ciphertext with an approved device key and fails with only an
OAuth credential or storage administrator access. No claim that every carrier,
firewall or country permits the chosen transport.
Require two CGNAT/UDP-blocked laptops in the public MVP evidence; hole punching
can improve direct reachability later without blocking the relay-first journey.

## Slice 7 — deliberate cross-device session continuation

Candidate PR: `feat: restore a selected session with verified content and fresh authority`.
Depends on Slices 2–5; sleeping-device restoration also requires Slice 6 or a
chosen private durable replica.

Scope: checkpoint/continuation import, compatibility inventory and explicit local
path mapping. Reuse existing same-harness native resume and cross-harness
continuation support. Keep provider logins/credentials device-local.

Acceptance matrix per supported harness/version: original live process reachable,
process dead but native checkpoint complete, unsupported native format with
continuation-only fallback, incomplete transcript/workspace, no provider login,
missing repository commit, dirty target checkout and revoked membership. Acquire
fresh authority before protected execution. Never replay transcript tool calls,
re-run all historical jobs, synthesize a terminal snapshot, restore an old fence
or claim live process-memory migration. The UI reports exactly which layers were
restored and which are unavailable.

## Slice 8 — enterprise deployment and operating controls

Candidate PRs: `feat: enforce private deployment profiles` and
`ops: deploy and recover the optional hosted coordination and ciphertext services`.
Depends on the relevant network/key capabilities above.

Scope: hosted/self-hosted/private/air-gap profiles, deployment templates and
operating runbooks. Existing AWS single-node provisioning remains a supported
small deployment; it is not silently relabeled HA hosting.

Acceptance: internal OIDC/local account bootstrap with public egress denied,
explicit local trust/key provisioning, signed offline packages/model weights,
no online installers/updates/telemetry/provider fallback, TLS/key rotation,
membership/employee removal and recovery-custodian audit. Test database PITR,
local vault loss, missing backup objects, relay failure, node loss and restore
drills with declared RPO/RTO. Document what an unavailable authority prevents.
AWS budgets measure DB, relay bytes/egress, object requests/storage, retries and
retention; operator-selected caps precede unattended transfer. Do not promise
multi-region active-active task writes.

## Slice 9 — platform lifecycle and distribution

Candidate PRs: `feat: support the local workspace lifecycle on Linux` and, after
an explicit scope decision, `feat: support the workspace client on Windows`.

Scope: reuse Go daemon/API/web UI and OS-independent lifecycle contracts; provide
OS-specific service, process, key-vault and filesystem adapters. macOS has a real
SwiftUI/WKWebView app and launchd/bundled PostgreSQL; Linux currently has CLI/service
support, not equivalent desktop UX. Windows needs process-group, path, key-store,
installer and packaging work rather than an untested cross-compilation claim.
Ship compatible Conductor/Cairn capability versions and signed offline artifacts;
the crypto development/bootstrap integration is not a production cipher library.
Mobile support is a separate product/lifecycle decision, not inferred from P2P.

Acceptance: install/upgrade/uninstall/restart, locked vault, offline startup,
corrupt local metadata, update signature/provenance, compatible dependency/SBOM
notices, multi-user loopback authorization, shutdown/reaping, disk and path cases
on each supported OS. Declare unsigned/ad-hoc vs notarized macOS builds truthfully.
Do not add a release matrix entry without exercising its runtime acceptance.

## Cross-cutting requirement-to-evidence matrix

| Requirement | Minimum evidence | Gate |
| --- | --- | --- |
| Local ownership and outage use | Durable catalog/export and pinned bytes open with backend blocked; empty/placeholder not misreported | Slices 1–2, first journey |
| Account login vs content recovery | Google/GitHub login and local unlock separate; new device without approval/recovery cannot decrypt | Slice 3 |
| Backend key-substitution resistance | Invalid roster/key addition refused; verified approval or declared TOFU/witness policy visible | Gate 0, Slice 3 |
| Native two-device flow | Real Conductor + Lab + supported harness checkpoint over generated WG/direct carrier; exact app/transport statuses | Slices 4, 7 |
| Offline task safety | Lease expiry/reclamation and new owner while peer offline; old epoch cannot publish | Slices 1, 4, 7 |
| Team isolation / personal defaults | Outsider/cross-project metadata and bytes refused; private session not auto-published; remaining authorized peer works | Slices 3–4 |
| Conflicts / file safety | Concurrent values, causal gaps, tombstones, delete/edit, path/symlink/case attacks and safe import preview | Slices 2, 4 |
| Transfer integrity / durability | Crash/disk-full/corruption, bounded memory/queue, verified resumed root and acknowledged persistence | Slices 2, 5 |
| Recovery limits | Asleep source with replica succeeds; no key/replica/backup visibly fails; old plaintext not revocable | Slices 3, 6–7 |
| Profile isolation | Installer/provider/seed/update/telemetry tripwires and real private endpoints; no public fallback | Slices 6, 8 |
| Platform claims | Real OS lifecycle/key-store/filesystem/harness tests with signed-distribution status | Slices 1, 9 |
| Privacy-aware operations | Redacted metrics/diagnostics, queue/frontier/key-epoch status, backup receipts and restore drills | Every slice |

Normal unit/contract/fault-injection cases belong in regular `make check`/`make ci`
where deterministic. Native multi-node/NAT/platform acceptance needs a reproducible
opt-in gate first, preserved successful and failed evidence, then a verified CI
job where runner capabilities permit. Do not claim continuous coverage from an
unexercised workflow or weaken existing suites to accommodate the fixtures.

## Planning-change validation and publication

This planning change is documentation only. Delegated source audits, independent
auth/storage review and root correctness review informed it. Local document links,
cited source paths, fence/newline/whitespace structure and `git diff --check` passed.
Before publication, the required repository checks ran using Go 1.26.9 on Linux
with workspace-local caches and a managed throwaway PostgreSQL fixture:

| Command | Outcome | Scope |
| --- | --- | --- |
| `make check` | Exit 0, 39s | Formatting, vet, staticcheck, build, all-package no-database tests |
| `make ci` | Exit 0, 88s | Formatting, vet, staticcheck, govulncheck (no vulnerabilities found), build, PostgreSQL-enabled tests, race detector and existing native collaboration/mTLS E2E |

Both required repository gates passed on their first run, with no test failures.
These gates validate the existing source
baseline; they do not implement or prove the future features proposed here. Audit
source hashes and provisional restoration/cloud-persistence choices remain
explicit above. Later implementation PRs require their own applicable checks and
human security/cryptography approvals.

The required `conductor check` for these exact two document paths was attempted
before editing and failed because no local control plane was listening on
`localhost:8080`. The authorized isolated-branch fallback is documented; no task,
claim or reservation is falsely reported as acquired. Publication of this reviewed
plan does not authorize merging future security/cryptography implementation work.
