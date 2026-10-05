# Conductor

A coordination control plane for teams where humans and coding agents work the same
repository at the same time.

Coding agents got fast; coordination did not. On a three-person team all running Claude Code,
Codex, or OpenCode against one repo, the expensive failures are not bad code — they are two
people building the same thing, two branches rewriting the same file, two migrations racing
the same table, and nobody able to answer "what is actually in flight right now?"

The obvious fix — share the chats — is the wrong fix. Prompts and model output are the most
private thing a developer produces. **Conductor never shares them.** It shares intent and
territory: who holds what, what shape of work they are doing, and where two efforts are about
to collide.

The full architecture is in [docs/DESIGN.md](docs/DESIGN.md). This README is how to run it and
what is actually built.

---

## Install

You need:

- **git**, and **curl** for the release installer;
- **PostgreSQL 16 or newer** that you already run, *or* **Docker**, in which case
  `conductor up` starts Postgres 17 in a container for you;
- **Go 1.25.14 or newer** only to build from source (an older `go` downloads the right
  toolchain itself unless `GOTOOLCHAIN=local` is set).

Pick one way to get the three binaries (`conductor`, `conductord`, `conductor-mcp`) onto your
PATH:

```bash
# A release build for macOS or Linux (amd64/arm64), no Go needed. Checks SHA256SUMS before
# installing into ~/.local/bin; add that directory to PATH if it is not already there.
curl -fsSL https://raw.githubusercontent.com/aburan28/conductor/main/scripts/install-release.sh \
  | bash -s -- aburan28/conductor ~/.local/bin

# From a clone: `make install` downloads the release as above, `make install-local` builds
# this checkout. Both add ~/.local/bin to PATH in your shell's startup file (zsh, bash, fish).
git clone https://github.com/aburan28/conductor && cd conductor && make install-local

# With Go, straight from the module:
go install github.com/aburan28/conductor/cmd/...@latest
```

Every release archive carries a GitHub build-provenance attestation:
`gh attestation verify conductor_vX.Y.Z_linux_amd64.tar.gz --repo aburan28/conductor`.
`conductor version` (or `--version` on any of the three binaries) says which build you have.

## Quickstart

From the repository you want to coordinate:

```bash
conductor up          # Postgres (Docker only if none is reachable), the control plane, your login
conductor init        # optional: scaffold .conductor/ policy files into this repository
conductor status      # what is in flight
conductor dashboard   # prints a ready-to-open link
conductor wrap claude # run Claude Code as a registered session (or codex, opencode)
conductor down        # stop the control plane (--db also stops the Postgres container)
```

`conductor up` is the whole setup. It reuses a Postgres that already answers at the DSN
(`--dsn`, or `DATABASE_URL`) and starts one in Docker only when none does; it starts the
control plane (API, SSE, dashboard, scheduler) on `127.0.0.1:8080` in the background, with
its log and pidfile under `~/.conductor/runtime/`; and it saves your CLI login at
`~/.conductor/credentials`. No token to copy, no second terminal. Running it again reuses
whatever is already up. In a clone of this repository, `make up` and `make down` do the same.

`conductor doctor` checks everything at once: the control plane and its version, the
database, `conductord`, git, Docker, and which coding tools are installed and connected.

### Getting help

```bash
conductor help                # the commands you need first
conductor help all            # every command
conductor help task           # one command, with an example (same as: conductor task -h)
source <(conductor completion bash)   # tab completion; also zsh and fish
```

---

## What it does

```
$ conductor check --summary "add retry-aware model routing" --scope dir:internal/router

block_conflict: scope conflict on dir:internal/router

  alice holds dir:internal/router for T-1 (write_exclusive).
  Wait for it, split your scope, or join their task.
```

That is the whole product in one command. Everything else exists to make that answer correct,
fast, and safe to trust.

It also catches the harder case — two people describing the same work in different words, with
no overlapping files yet:

```
$ conductor check --summary "Build team invitation flow: send invite emails, accept invitations"

suggest_join: similar work already in flight

  T-3 (owner alice) looks like the same work. Join it, or narrow your scope.
```

Alice wrote "Implement the team invite flow with email invitations and acceptance". The server
never saw either sentence in a form it can read back: both were reduced to HMAC'd token sets
under a per-tenant key and compared with MinHash. Detection without disclosure.

### Work goes where the capability is

A session advertises what it is driving. Conductor resolves that against the organization's
model catalog — so a session cannot promote itself by asserting a tier — and work that needs a
particular ceiling is offered to a session that has one.

```
$ conductor capabilities

demo

  3 session(s), 2 accepting work
  ceiling: tier T4, reasoning effort xhigh

  alice        claude    online_idle
       claude-opus-5 · tier T4 · effort xhigh (running high)
  rachel       codex     working
       gpt-5-codex · tier T2 · effort medium
       on T-12

$ conductor task assign T-42 --require-tier T4 --require-effort xhigh

T-42 offered to alice (claude-opus-5 on claude).
  requirement: tier ≥ T4, effort ≥ xhigh
  1 of 3 live session(s) qualified

  Not chosen:
    rachel       tier T2 is below the required T4
```

A floor is a floor: an idle cheap session never wins a selection it does not qualify for.
Above the floor the *cheapest* qualifying session wins, so a frontier session is still there
when something actually needs it. From inside a run, an agent that hits work beyond its own
ceiling calls `coord_delegate` — the same continuation bundle as a handoff, plus a floor the
receiver must meet. If nothing live qualifies, the bundle is still written and the caller is
told what the ceiling actually is.

### The CLI dispatches to the right model, by policy

`conductor capabilities` is about the sessions live *right now*. The other half is a repository
policy that says which concrete model each kind of work should go to — declared in
`.conductor/dispatch.yaml`, versioned with the code, and hashed onto every attempt so a routing
decision is always explainable.

```yaml
# .conductor/dispatch.yaml
lanes:
  implement:
    role: implementer
    candidates:
      - model: ollama/qwen3:27b        # a local model for small, low-risk work
        harness: opencode
        tags: [local]
        when: task.estimated_files <= 3 && !task.security_sensitive
        max_concurrent: 1              # one GPU, one attempt at a time
      - model: claude-sonnet-5
        harness: claude
      - model: claude-opus-5           # escalation walks down the ladder on failure
        harness: claude

rules:
  - id: docs-local
    when: task.labels has "docs"
    prefer: { tag: local }
  - id: routing-changes
    when: task.paths any "internal/router/**"
    require: { tier: T3 }

defaults: { lane: implement, on_failure: escalate, max_escalations: 2 }
```

The `when` expressions read deterministic facts derived from the ledger — scopes, labels,
attempt history, budget position — never a prompt. `conductor route T-42` shows what the policy
would decide and why, before a token is spent:

```
$ conductor route T-42

T-42 would route to:

  claude-sonnet-5 on claude  (lane implement, effort medium, tier T2)

  Considered:
    ✗ ollama/qwen3:27b on opencode — condition not met: task.estimated_files <= 3 && !task.security_sensitive
    ✓ claude-sonnet-5 on claude
    · claude-opus-5 on claude

  Rationale:
    • lane implement (3 candidates)
    • chose claude-sonnet-5 on claude at effort medium
```

A dispatch candidate can never route *below* a hard floor: a security-sensitive task keeps its
T4 floor whatever the ladder says. `conductor policy lint` validates the whole file — unknown
facts, undefined lanes, malformed expressions — and `conductor models discover` finds the local
models (Ollama today) worth adding to the ladder.

### Budgets bound the team, not the person

With `budget.member.monthly_tokens` set, every member gets the same token allowance over a
rolling 30-day window — and it is *transferable*. Alice heading into a slack week hands her
headroom to Bob mid-refactor, in one command, with no admin in the loop:

```
$ conductor budget share bob 2m --note "finishing the router refactor"

Shared 2m tokens with bob.

  you    1.1m remaining
  bob    3.4m remaining
```

A member whose window balance is spent cannot claim new work (HTTP 402, exit-code fail from
the CLI) — until a teammate shares theirs. Balances are pure arithmetic over two ledgers
(attempt spend and budget grants), so there is no counter to drift and no way to mint tokens:
grants are checked against the giver's live balance under the same lock the claim path takes.
`conductor budget` shows everyone's position; `conductor budget grants` is the transfer
history; a `budget.shared` event lands on the team stream. As everywhere else, amounts and
identities are shared — what the tokens were spent *on* is not.

### The team pools capacity, and queues when it is full

Coworkers connect to each other through Conductor: a teammate joins the same control plane and
their machines and sessions become shared capacity — a *swarm*. `conductor swarm` rolls up who
is contributing what, and who has budget to spare:

```
$ conductor swarm

Capacity: 2 runner(s), 3 session(s) accepting work, 5 free slot(s), 1 waiting in queue

  WHO            KIND     STATE       LOAD        BUDGET LEFT
  alice          session  working     ready       3.4m
  rachel         runner   online      1/4         1.1m
  bob            session  online_idle ready       0

Share budget with a teammate: conductor budget share <who> <tokens>
```

A teammate joins from the link `conductor invite <them>` prints — `conductor join "<link>"`
(`conductor swarm join "<link>"` is the same thing) — and then contributes interactive capacity
with `conductor wrap`, autonomous capacity with `conductor worker`, or spare tokens with
`conductor budget share`.

When too many sessions or attempts are running at once, new work does not fail — it takes a
place in an **admission queue** and waits. Set the caps in `.conductor/policies.yaml`:

```yaml
concurrency:
  max_active_sessions: 6          # across the whole team
  max_sessions_per_principal: 2   # so one person cannot take every slot
  max_concurrent_attempts: 4
```

Past the cap, `conductor wrap` parks with its place in line (`waiting for a session slot,
position 2…`) and starts the moment a slot frees up; the scheduler grants tickets in arrival
order, and a granted slot that stops heartbeating is handed on rather than held forever.
`conductor queue` shows the whole line. As everywhere else, a ticket carries identity, a kind,
and a model name — never what the work is about.

### Privacy is structural, not procedural

A teammate looking at Alice's private task sees:

```
T-2      ready      alice      (private)
         dir:internal/api
```

Enough to not collide. Nothing about what it is. And the schema has no column for a prompt,
the event payload passes an allowlist, and the harness stream adapters drop assistant text at
the parse boundary before it can reach the store. Three tests assert this mechanically:
`TestNoTranscriptFieldsInSharedTypes`, `TestNoTranscriptColumnsInSchema`, and
`TestEventTypeHasNoContentField`.

---

## Setting up, in detail

[Quickstart](#quickstart) covers the one-command path. The rest of this section is what
happens underneath, and how to do it by hand or for a team.

### Signing in on your own machine needs no token

Open `http://127.0.0.1:8080` and you are in. On the machine that runs `conductord`, its owner
(whoever first ran `conductord bootstrap` there) is signed in automatically. The dashboard,
the CLI, and the macOS app call `POST /v1/local/session` and get an ordinary token without
anyone pasting one. Delete `~/.conductor/credentials` and the next `conductor` command signs
itself back in.

Only that one endpoint changed; every other request still needs a token. It answers only a
request that:

- arrives on loopback, from a daemon that is not behind a proxy;
- names `localhost` or `127.0.0.1` as its Host, which defeats DNS rebinding;
- sends a JSON body, which a cross-site form cannot;
- if it carries a browser `Origin`, carries this same origin;
- came through no proxy: forwarding headers (`X-Forwarded-For`, `Forwarded`, `Via`, …) or
  HTTP/1.0, which is how a stock nginx talks to its upstream, refuse it.

If you put a reverse proxy in front of a loopback-bound `conductord`, pass `--behind-proxy`
(which turns local sign-in off) or run `conductor security enhanced`. Do the same before
forwarding the port at the TCP level (`ssh -R`, `socat`, `kubectl port-forward`): such
forwarders add no headers, so nothing distinguishes their traffic from local traffic.
Loopback also cannot tell apart two OS users on one machine.

A token issued by local sign-in works only while local sign-in is allowed. In enhanced
mode it is rejected, revoked or not, and so is any token it was used to mint. That is what **enhanced
security mode** is for:

```bash
conductor security               # which mode, who owns the machine
conductor security enhanced      # tokens only, everywhere; revokes every token local sign-in issued
conductor security local         # back to automatic sign-in (the owner only)
```

A daemon listening on loopback defaults to `local`; one reachable from a network defaults to
`enhanced`. `conductord --security-mode` pins either. Any project admin can tighten the mode;
only the owner can loosen it. The same switch is on the dashboard's Settings page.

### Manual, or on another repository

```bash
make db-up && make build                     # Postgres + bin/conductord, bin/conductor, bin/conductor-mcp

cd /path/to/your/repo
conductor init                               # scaffold .conductor/ policy files

export DATABASE_URL="postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable"
conductord bootstrap --org acme --project myrepo --principal $USER --repo .
# saves your login at ~/.conductor/credentials — no copy-paste
# (--no-login skips that; --endpoint saves a different URL than http://localhost:8080)

conductord &                                 # or: make serve, same thing in the foreground
conductor dashboard                          # prints a ready-to-open link
```

### Adding your coworkers

The fastest way is one link. `conductor invite` mints a teammate their own token and bundles
the endpoint, project, and token into a single join link; they redeem it with `conductor join`
(or by opening it in a browser) and they are in:

```bash
$ conductor invite rachel --role maintainer --expires 7d

Invited rachel as maintainer on myrepo.

Send them this link, once, over a channel you trust:

  https://conductor.team/#project=myrepo&token=cdt_QaltIz7t…

They run:  conductor join "<link>"     (or open it in a browser)

The token expires 2026-09-03T19:37:59Z.
```

```bash
# on the teammate's machine
conductor join "https://conductor.team/#project=myrepo&token=cdt_QaltIz7t…"
# Joined https://conductor.team as rachel. Then: conductor wrap claude / conductor worker.
```

The token rides in the URL **fragment** (after `#`), which a browser never sends to the server —
so the credential stays out of every request line and access log, unlike a query-string link.
The same link opens the web dashboard: it reads the fragment on load, then strips it from the
address bar. If the endpoint you are logged in against is loopback (`127.0.0.1`), `invite` warns
that a teammate cannot reach it and shows how to expose the control plane and pass a public
`--endpoint`.

A token is minted only for a **new** account. If the handle already belongs to someone in your
organization (they are in another project, say), `invite` and `member add` add them to this
project and print no token or link: they keep signing in with their own credentials, which now
reach this project (`conductor login --project myrepo` switches their default). Handing the
inviter a fresh token for an existing account would let any project admin sign in as anyone.

Inviting someone who is already a member is refused rather than quietly changing their role.
Roles change with `conductor member role`, which never grants a role above your own, never
touches someone who outranks you, and never demotes the project's last administrator:

```bash
conductor member role rachel maintainer
```

The longer form still works, and is what a script or CI wants:

```bash
conductor member add rachel --role contributor   # a new account: prints a `conductor login …` line, once
conductor member list
conductor member role rachel reviewer            # change a member's role
conductor member remove rachel                   # also revokes their tokens
conductor token create --save                    # mint one more; the old ones stay valid
conductor token reset --save                     # rotate: one replacement, everything else revoked
```

A joined teammate contributes to the swarm — `conductor wrap` for interactive work, `conductor
worker` for autonomous work, `conductor budget share` for spare tokens (see below).

`conductord` binds loopback by default and **refuses to serve a reachable address in
plaintext**, because bearer tokens would cross the network in the clear. To expose it:

```bash
conductord --addr 0.0.0.0:8080 --tls-cert cert.pem --tls-key key.pem
conductord --addr 0.0.0.0:8080 --behind-proxy      # your proxy terminates TLS
```

Clients using a private CA set `CONDUCTOR_CA_CERT=/path/to/ca.pem`. Failed authentication is
throttled per client; a correct token is never throttled, so one person mistyping theirs
cannot lock out an office behind a shared NAT.

Prove the whole execution loop with no API key and no vendor CLI installed:

```bash
conductor task create --title "Try Conductor" --scope path:README.md
conductor worker --dry-run succeed --once -v
```

The built-in fake harness claims a task, creates a worktree, edits a file, runs your required
checks, commits, and submits evidence — exercising every coordination path with a deterministic
stand-in for a model. The task ends in `verifying`: the work is finished but not merged, so it
keeps `README.md` reserved. `conductor task show T-1` shows the branch and commit; once that
branch is merged, `conductor task done T-1` completes the task and frees the file (with the
GitHub App linked, the merge does it for you).

Or run the scripted demo, which reproduces the scenario above end to end:

```bash
make e2e
```

### Single sign-on

A team with Google Workspace, GitHub, Okta, Auth0, Keycloak or any other OpenID Connect
provider can sign in through it instead of passing tokens around. A sign-in ends with an
ordinary Conductor token (named `sso:<provider>`, 12 hours by default), so roles, project
scopes and enhanced security mode apply to it exactly as to any other token. Nobody gets in
just by having an account at the provider: an administrator registers each person's address
first, and their first sign-in links to that account.

Every provider needs one redirect URI registered with it, exactly:

```
<--public-url>/v1/sso/<name>/callback        e.g. https://conductor.example.com/v1/sso/google/callback
```

`conductord` logs it for each provider at startup. SSO needs `--public-url` to be `https`
(plain `http` only on a loopback address, for trying it out).

**Google.** In the Google Cloud console, open *APIs & Services → OAuth consent screen* and
configure it (*Internal* keeps it to your Workspace). Then *Credentials → Create credentials →
OAuth client ID*, type *Web application*, and add the redirect URI above under *Authorized
redirect URIs*. Start `conductord` with the client id, and the secret in the environment:

```bash
export CONDUCTOR_SSO_GOOGLE_CLIENT_SECRET='GOCSPX-…'      # or client-secret-file=/run/secrets/google
conductord --addr 0.0.0.0:8443 --tls-cert cert.pem --tls-key key.pem \
  --public-url https://conductor.example.com \
  --sso-provider name=google,issuer=https://accounts.google.com,client-id=1234-abc.apps.googleusercontent.com,domain=example.com
```

**GitHub.** GitHub is not an OpenID Connect provider for people, so it has its own type. Create
an OAuth App (*Settings → Developer settings → OAuth Apps → New OAuth App*, or the same under
your organization's settings), with *Homepage URL* your public URL and *Authorization callback
URL* `https://conductor.example.com/v1/sso/github/callback`. Generate a client secret, then:

```bash
export CONDUCTOR_SSO_GITHUB_CLIENT_SECRET='…'
conductord … --sso-provider name=github,client-id=Ov23li…,org=acme
```

`org=` admits only active members of that organization (repeat it to allow several); if the
organization restricts OAuth App access, an owner has to approve the app once. GitHub Enterprise
Server adds `api-url=https://HOST/api/v3,web-url=https://HOST`.

**Any other OpenID Connect provider** (Okta, Auth0, Keycloak, …): register a web application
with the redirect URI, and pass its issuer exactly as its
`/.well-known/openid-configuration` states it, e.g.
`--sso-provider name=okta,issuer=https://acme.okta.com,client-id=0oa…`. The provider must assert
`email_verified`; one that does not is refused rather than trusted with unverified addresses —
except a single Microsoft Entra ID tenant you explicitly trust (below).

| `--sso-provider` key | Meaning |
|---|---|
| `name` | lowercase id, used in the redirect URI, URLs and `conductor login --sso NAME` |
| `type` | `oidc` (default) or `github` (the default when `name=github` and no issuer is given) |
| `issuer`, `client-id` | the provider's issuer and this application's client id |
| `client-secret-file`, `client-secret-env` | where the secret is; by default `CONDUCTOR_SSO_<NAME>_CLIENT_SECRET`. Never on the command line, where every user can read it |
| `domain` | admit only verified addresses in this domain (repeatable) |
| `org` | GitHub: admit only active members of this organization (repeatable) |
| `label` | the button text, "Sign in with …" |
| `api-url`, `web-url` | GitHub Enterprise Server |
| `groups-claim` | the ID token claim listing the account's groups, for group → role mapping (default `groups`) |
| `trust-email-domain` | Entra ID only: accept this tenant's sign-in names in this domain without `email_verified` (repeatable) |

**Microsoft Entra ID.** Entra issues no `email_verified` claim, so it needs an explicit trust.
In the Entra admin center, *App registrations → New registration*, single tenant, with a *Web*
redirect URI `https://conductor.example.com/v1/sso/entra/callback`; under *Certificates &
secrets* create a client secret; under *Token configuration* add the optional `email` claim (and
a groups claim if you will map groups to roles). Then use the tenant's own issuer — with its
tenant id, never `common` or `organizations` — and list the domains whose addresses it may vouch
for:

```bash
export CONDUCTOR_SSO_ENTRA_CLIENT_SECRET='…'
conductord … --sso-provider name=entra,issuer=https://login.microsoftonline.com/<tenant-id>/v2.0,client-id=<application-id>,trust-email-domain=example.com
```

The address is taken from `upn`, then `preferred_username`, then `email`, and only in a listed
domain; the token's `tid` must be the tenant, and guests from other directories are refused.
Why that is safe enough, and what it does not protect against, is in
[DESIGN.md §25.7](docs/DESIGN.md).

Several providers are several `--sso-provider` flags, or one `CONDUCTOR_SSO_PROVIDERS` variable
with the specs separated by `;`.

**Registering people.** A first sign-in links to the account whose registered address the
provider verified, and only if that account does not sign in some other way already — an email
address alone never takes over an account that has an identity:

```bash
conductor member add rachel --role contributor --email rachel@example.com --no-token
conductor sso email bob bob@example.com         # an existing member
```

Setting someone's sign-in address, or unlinking their identity, takes an administrator of every
project they belong to. Then they sign in — in the dashboard with **Sign in with Google**, or
from a terminal (a browser opens, and the CLI listens on `127.0.0.1` for the result):

```bash
conductor login --endpoint https://conductor.example.com --sso google
conductor sso status                    # providers, and the identities linked to you
conductor sso link github               # add another provider to the account you are signed in as
conductor sso unlink bob google         # administrators: remove an identity and end its sessions
```

To skip registering people, `--sso-auto-provision contributor --sso-default-project acme/web`
gives a first sign-in that matches no registered address a new account in that project. It
grants contributor, reviewer or observer, never more, and requires every provider to be
restricted with `domain=` or `org=` — otherwise anyone with a Google account would be let in.
`--sso-token-ttl` sets how long a sign-in lasts; signing in again re-checks the provider (an
address still allowed, a membership still active). The design and threat model are in
[DESIGN.md §25.7](docs/DESIGN.md).

### For enterprises

Everything an organization's administrator needs is in the dashboard's **Admin** area (shown
to `org_admin`s only) and under `/v1/admin`. The first person bootstrapped into a new
organization is its `org_admin`; promote others with `conductor member role HANDLE org_admin`.

| Admin section | What it does |
|---|---|
| Organization | display name, accent color (checked for contrast), logo (PNG/JPEG/GIF, 64 KiB), sign-in banner |
| Authentication | providers, **require single sign-on**, allowed email domains and GitHub organizations, account provisioning, group → role mapping, token lifetimes |
| Provisioning | SCIM tokens and the SCIM base URL for Okta or Entra ID |
| Members & roles | every account, its projects and roles, linked identities; change a role, unlink, deactivate |
| Features | which advanced areas (queue, swarm, budget sharing, mesh, local models, checkpoints by account) the dashboard shows. A new organization starts with none; turning one off hides it, it never revokes access |
| Audit log | filter by actor, action and time; export as CSV or JSON lines |
| Configuration | the server's effective configuration, secrets redacted, and what its config file locks |

**A config file.** `conductord --config /etc/conductor/conductor.yaml` (or `CONDUCTOR_CONFIG`)
holds what otherwise takes a page of flags, and can *lock* organization settings: a locked
setting is read-only in the admin area, marked "managed by your administrator's config file".
Secrets are never written in it — only the environment variable or file that holds them:

```yaml
version: 1
server:
  addr: 0.0.0.0:8443
  public_url: https://conductor.example.com
  tls_cert: /etc/conductor/tls/cert.pem
  tls_key: /etc/conductor/tls/key.pem
  security_mode: enhanced
database:
  url_env: DATABASE_URL            # or url_file: /run/secrets/database-url
retention:
  events_days: 90
  audit_days: 400
metrics:
  token_file: /run/secrets/metrics-token
sso:
  token_ttl: 10h
  providers:
    - name: okta
      issuer: https://example.okta.com
      client_id: 0oa1b2c3
      client_secret_env: OKTA_CLIENT_SECRET
      domains: [example.com]
    - name: entra
      issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
      client_id: <application-id>
      client_secret_file: /run/secrets/entra
      trusted_email_domains: [example.com]
features:            # locked for every organization on this server
  queue: true
branding:            # locked; logo_file is read and checked at startup
  display_name: Example Engineering
  login_banner: Authorized use only. Activity is logged.
policy:              # locked
  require_sso: true
  human_token_max_ttl: 30d
  allowed_domains: [example.com]
```

A command-line flag wins over its environment variable, which wins over the file, which wins
over the default. `conductord config check --config FILE` validates a file — every unknown key,
wrong type, inline secret, empty reference and invalid policy is reported — without touching the
database, and prints each effective setting with where it came from. The full key list is in
[docs/OPERATIONS.md](docs/OPERATIONS.md#the-config-file).

**Require single sign-on.** Admin → Authentication, or `policy.require_sso` in the file. A
person's token is then accepted only if single sign-on minted it (or a token minted from such a
session, which never outlives it); runners and other service accounts keep their tokens, and
local sign-in on the server's own machine follows the security mode. Older tokens are refused,
not revoked, so turning the policy off — say, while the identity provider is down — restores
them; anyone whose access should end for good is deactivated. Turn it on while signed in
through single sign-on: the server refuses to do it from a token session, so you cannot lock
yourself out.

**SCIM provisioning.** Admin → Provisioning → *New token*, then:

- **Okta:** in the app, *General → App Settings → Provisioning: SCIM*; under *Provisioning →
  Integration*, *SCIM connector base URL* `https://conductor.example.com/scim/v2`, *Unique
  identifier field for users* `userName`, supported actions *Push New Users*, *Push Profile
  Updates*, *Push Groups*, authentication mode *HTTP Header* with the token. Enable *Create
  Users*, *Update User Attributes* and *Deactivate Users* under *To App*.
- **Microsoft Entra ID:** *Enterprise applications → (your app) → Provisioning → Automatic*,
  *Tenant URL* `https://conductor.example.com/scim/v2`, *Secret Token* the token, *Test
  Connection*, then start provisioning.

Provisioning creates and updates people (they join the policy's default project with its default
role, contributor at most), deactivates them (`active=false`: every token revoked and refused at
once; history and memberships kept), and deletes them (memberships and identities removed, the
account kept for the audit trail). Groups it pushes feed group → role mapping. SCIM never grants
`org_admin`, and refuses to deactivate a project's last administrator.

**Group → role mapping.** Rules such as `engineering → contributor in app` apply at every sign-in,
from the ID token's groups claim, GitHub teams (`org/team-slug`) and SCIM groups. A mapped role
never exceeds the configured ceiling (maintainer by default, never `org_admin`), never touches a
role above it, never demotes a project's last administrator, and never removes anyone.

**Audit export.** Admin → Audit log, or:

```bash
curl -H "Authorization: Bearer $TOKEN" "https://conductor.example.com/v1/admin/audit?format=csv&since=2026-09-01" > audit.csv
curl -H "Authorization: Bearer $TOKEN" "https://conductor.example.com/v1/admin/audit?format=jsonl&action=sso." > sso.jsonl
```

Filters: `actor=HANDLE`, `action=` (exact, or a family ending in `.`), `since=`/`until=` (RFC 3339
or a date). Exports are audited, and CSV cells that a spreadsheet would run as formulas are
defused.

---

## Daily use

```bash
conductor check --summary "…" --scope dir:internal/api    # before you edit. exit 3 = stop
conductor task claim --next                               # take work and its territory
conductor wrap claude                                     # register a session + heartbeat, then launch
conductor task done T-42                                  # it merged: close it and free its files
conductor task reopen T-42                                # review wants changes: back to the queue
conductor serve qwen                                      # local vLLM for OpenCode (also: flash, glm53)
conductor wrap opencode --model vllm/qwen3.8-27b
conductor presence --watch                                # who is live, on what
conductor conflicts                                       # what is contested and what to do
conductor task handoff T-42 --to codex --next "write tests"
conductor capabilities                                    # which models are live, and how hard they can think
conductor task assign T-42 --require-tier T4 --require-effort xhigh
conductor inbox                                           # work offered to this session
conductor budget                                          # the team's token budget this window
conductor budget share rachel 500k                        # give a teammate part of your allowance
conductor pause                                           # freeze every agent terminal on this machine
conductor resume                                          # wake them; closed terminals are reopened
conductor sessions save all                               # keep every session resumable, even after a reboot
conductor usage --by day,harness                          # tokens and cost over time, across claude/codex/opencode
conductor sessions export                                 # the project's session history, as JSON
conductor sessions install-hook                          # capture every session at shutdown (systemd/launchd)
conductor backup push | pull | status                    # copy this machine's resume records to/from S3
conductor checkpoint capture --note "tests pass"         # snapshot a session: transcript + working tree, portable
conductor checkpoint resume 9a474a --account work        # continue it under another login, or --harness codex
conductor security [local|enhanced]                       # sign in without a token on this machine, or tokens only
conductor github setup | link | status                    # a "Conductor" check on every pull request
conductor github issues enable --label conductor          # labelled GitHub issues become tasks
conductor integrate cursor                                # wire a coding tool to this project (MCP + hooks)
conductor route T-42                                      # what would this route to, and why — before spending a token
conductor dispatch T-42                                   # send work to a model by policy, through the queue
conductor models                                          # the model catalog; `models discover` finds local ones
conductor policy lint                                     # validate .conductor/ policy and dispatch rules
conductor swarm                                           # the team's pooled capacity and who has budget to share
conductor queue                                           # the admission line when the team is at capacity
```

Every command takes `--json`.

### The loop, start to finish

1. **Check.** `conductor check` (or the agent's `conductor_check_conflicts`) says whether
   anyone holds what you are about to touch. A blocked check leaves a short note that you are
   waiting; when the holder lets go, a `scope.released` event names you.
2. **Claim.** `conductor task claim T-42 --scope path:…` takes the task and its territory. Run
   inside a wrapped session (or by an agent through `coord_start_work`), the claim is bound to
   that session. From a plain shell it is recorded against the checkout and waits up to ten
   minutes for a session to take it over.
3. **Wrap.** `conductor wrap claude` registers the session and adopts any claim made in the
   same checkout. Its heartbeat (every 20 seconds) keeps the session **and every claim it
   holds** alive for as long as it runs, and reports which paths the working tree has touched
   (paths only), so merge-risk detection sees interactive work too. When the session ends,
   the claim lapses one lease TTL later and the reconciler releases it. An MCP gateway keeps
   the claim it took alive the same way, without the model spending a token on it.
4. **Work.** The pre-edit hook blocks an edit to a file someone else holds. The first edit of
   a file outside your own claim reserves it under the claim (`--auto-reserve`, the default
   `conductor integrate` installs) and tells the agent so; a session with no claim is
   reminded that its edits reserve nothing.
5. **Publish.** `coord_publish_result` records the commit, the changed paths, and each
   validation command with its exit code; `coord_finish_work` moves the task to `verifying`.
6. **Merge.** Finished work keeps its territory while it waits to merge — anyone who checks
   one of its files is told the change is in an unmerged pull request, not that someone is
   editing. When the pull request merges, the task is done and its files are free: the
   GitHub App does this from the merge webhook (or by polling), and `conductor task done`
   does it by hand. A pull request closed without merging sends a waiting task back to
   `ready` and releases its hold; `conductor task reopen` sends work back but keeps it.

### Sharing with someone by text

`conductor invite` makes one link: endpoint, project, and token, with the token in the URL
fragment so it never reaches a server log. Text it; they run `conductor join "<link>"` or just
open it in a browser. `conductor join` then offers to connect every coding tool on their
machine in one step (`--integrate` / `--no-integrate` to decide in advance).

A link to `127.0.0.1` only reaches you. When Tailscale is running, `conductor invite` prints
the two commands that let someone in over your tailnet, and nothing else:

```bash
tailscale serve --bg 8080
conductor invite brother --endpoint https://your-mac.tailnet.ts.net
```

Requests through the tailnet name are not local, so they need the token in the link; local
sign-in stays yours.

### GitHub: a check on every pull request

Conductor knows which files every in-flight task holds. With its GitHub App installed, each
pull request on a linked repository gets a **Conductor** check run. The check says whether the
pull request changes files that other open work has reserved, or has already changed itself.
It names the overlapping file, the task, and its owner. It never names the task's title,
because anyone who can read the repository sees the check.

```bash
conductor github setup          # opens one page with one button: GitHub creates the app
conductor github install        # pick the repositories
conductor github link           # inside a checkout: this project is that repository (bootstrap does it too)
conductor github status         # the app, where it is installed, what is linked
conductor github check acme/widgets#12   # check one pull request now
```

The app is the machine owner's: only they can create it (`--replace` to swap it out), and only
projects in their organization can be linked, so another tenant on a shared control plane can
neither take the app over nor read a repository through it. In a check run, a private task
appears as "a private task", and a public repository gets no task references or owners at
all. A pull request's own task is excluded only for a branch in the repository itself, never
a fork's. The app asks for read access to contents and pull requests, and write access to
checks and issues (issues only for issue sync, below). It cannot push, merge, or change settings. Its credentials are kept in Conductor's database, so
every `conductord` sharing it serves the same app, with the private key and secrets sealed
under a key that is not in the database (`~/.conductor/secret.key`, `--secret-key-file`, or
`CONDUCTOR_SECRET_KEY`; replicas must share it — see docs/OPERATIONS.md). An app saved by an
older version in `~/.conductor/github-app.json` is imported once. `CONDUCTOR_GITHUB_APP_ID` /
`CONDUCTOR_GITHUB_APP_PRIVATE_KEY(_FILE)` / `CONDUCTOR_GITHUB_WEBHOOK_SECRET` override the
stored values. A changed result updates the commit's check run rather than adding another,
and an unchanged one is not posted again after a restart. A conductord that GitHub cannot reach, such as a laptop,
polls open pull requests every two minutes (`--github-poll`). One started with a public
`--public-url` receives signed webhooks at `/github/webhook`. The check is `neutral` when
there is an overlap, so it informs a reviewer without blocking a merge unless branch
protection requires it.

The app also closes the loop. A pull request is linked to its task when its branch is one an
attempt recorded or follows the `agent/<task-ref>/attempt-<n>` convention (branches in the
repository itself only, never a fork's); `conductor task show` and the dashboard show the
link. When a linked pull request **merges**, the task moves to `done` — from wherever its work
stood, ending a still-live claim — and its reserved files are released. When one is **closed
without merging**, a task that was waiting on it goes back to `ready` and drops its hold, and a
task still being worked is left alone. Without webhooks, the poller does the same from the
pull requests closed since its last pass (and looks up any linked pull request that left the
open list), so a pull request opened and merged between two polls still completes its task.
Seeing the same merge twice changes nothing and announces nothing.

#### GitHub Issues as tasks

A team's backlog already lives in its issue tracker. Issue sync keeps it there: opt a linked
project in, and its repository's issues become Conductor tasks without anyone filing them twice.

```bash
conductor github issues enable                 # open issues labelled `conductor` become tasks
conductor github issues enable --all           # ...or every open issue
conductor github issues enable --no-progress-label --public-visibility team_summary
conductor github issues status                 # settings, how many imported, the last problem
conductor github issues sync                   # import now instead of at the next poll
conductor github issues disable
```

- **Import.** Each qualifying open issue becomes a `ready` task with external_ref
  `github:owner/repo#N`: the issue's title, its body as the objective (comments stripped,
  bounded like any objective, the full text a click away), and the list under a `## Acceptance`
  (or `## Acceptance criteria`) heading as acceptance criteria. Importing is idempotent: the
  webhook, the poller, `sync`, and a second replica all find the same task. An open task that
  already names the issue in its external_ref is linked rather than duplicated. Removing the
  label later does not drop the task.
- **Edits, last writer wins.** An edit to the issue's title, or to its body, reaches the task
  unless the task's copy was also edited in Conductor since the last sync; then whichever edit
  is later wins (the issue's `updated_at` against when the task's text was edited). A Conductor
  edit that wins stays until the issue's text changes again. Content only ever flows from
  GitHub to Conductor.
- **Close and reopen.** Closing an issue cancels its task, unless the task is done or its work
  is landing (verifying, in review, or an open pull request): a pull request's `Closes #N`
  closes the issue moments before the merge completes the task, and the merge decides.
  Reopening an issue brings back a task the sync cancelled; one a person cancelled stays.
- **Write-back.** A claim adds one comment, "Claimed by `<handle>` via Conductor", and the
  `in-progress` label (configurable, or off); a release removes the label. When the task is done
  (by `conductor task done` or by its pull request merging) the issue gets one comment linking
  the pull request and is closed, unless it is closed already. Comments are written once per
  claimant and once per completion, in fixed words.
- **Privacy.** Nothing written in Conductor reaches GitHub: no title, objective, criteria, or
  progress. A task imported from a public repository is `team_artifacts` (configurable), since
  every word of it is public already; one from a private repository gets the project's default
  visibility. A private task is never written back to a public repository's issue, and its
  claimant is never named; on a public repository only `team_artifacts` and `shared_debug`
  tasks name their claimant. A pull request is linked only when it is in the issue's own
  repository.
- **Delivery and rate limits.** `issues` webhooks when GitHub can reach Conductor; otherwise the
  poller lists only issues changed since its last pass, as a conditional request, so an idle
  repository costs a 304 that GitHub does not count. The write-back runs every 15 seconds on the
  poller's lock, bounded per pass, and stops for as long as GitHub asks when it hits a rate
  limit.
- **Permissions.** Issue sync needs the app's `issues: write` permission, which apps created
  before it existed lack: GitHub applies a manifest's permissions only when it creates the app.
  `conductor github status` says so, with the app-settings page to grant it and each
  installation's page where GitHub asks its owner to accept the new permission.

Task lists, `conductor task show`, and the dashboard's task detail link to the issue. GitHub
is the only tracker for now; the sync's rules live in `internal/tracker`, behind a small
adapter interface, and Linear is the next adapter planned.

### Connecting your coding tool

One command wires Conductor's MCP tools — and, where the tool supports them, pre-edit hooks —
into whatever you drive:

```bash
conductor integrate claude        # Claude Code: .mcp.json + PreToolUse/SessionStart hooks
conductor integrate cursor        # Cursor: .cursor/mcp.json + a rules file
conductor integrate codex         # Codex: ~/.codex/config.toml
conductor integrate opencode      # OpenCode: opencode.json + a pre-tool plugin
conductor integrate all           # every tool this machine has
```

Also supported: `windsurf`, `vscode`, `zed`, `gemini`. Each merges into the tool's own config
without disturbing anything else already there, and `--print` shows exactly what it would write
before it writes it. `conductor doctor` reports which tools are connected.

Every write is idempotent and never puts a bearer token into a project file that could be
committed: stdio configs need no token (the `conductor-mcp` binary reads your saved login), and
HTTP configs reference the token through the tool's own `${env:CONDUCTOR_TOKEN}` syntax.

**Two transports.** The `conductor-mcp` binary speaks MCP over stdio, for any tool that launches
a local process:

```json
{ "mcpServers": { "conductor": {
  "command": "conductor-mcp", "args": ["--project", "myrepo"] } } }
```

Or point an HTTP-capable client straight at the control plane — no local binary, which is what a
teammate on a shared server wants:

```json
{ "mcpServers": { "conductor": { "type": "http",
  "url": "https://conductor.team/mcp",
  "headers": { "Authorization": "Bearer ${CONDUCTOR_TOKEN}", "X-Conductor-Project": "myrepo" }
} } }
```

`conductord` serves the Streamable HTTP transport at `/mcp` (and `/mcp/{project}`), negotiating
protocol revisions `2024-11-05` through `2025-06-18`, with per-session ids and the same bearer
auth every other client uses — the gateway holds no private path into the store, over either
transport.

Thirteen tools: `conductor_check_conflicts`, `coord_start_work`, `coord_get_work`,
`coord_expand_scope`, `coord_report_progress`, `coord_publish_result`, `coord_finish_work`,
`coord_handoff`, `coord_delegate`, `coord_capabilities`, `coord_checkpoint`, `coord_quota`,
`coord_project_status`. Heartbeats are
deliberately *not* an MCP tool — a model should never spend tokens telling the server it is
still alive. And where a harness supports pre-edit hooks, `conductor integrate` installs
`conductor hook pre-tool`, which calls the same conflict check before every edit and blocks the
tool call (exit 2, with the holder named) when someone else holds the file — enforcement, not
just advice.

### Token usage across harnesses

Every harness meters itself and none of them compare notes. Claude Code writes a usage
block on each message of its transcript, Codex logs a running total after every response,
OpenCode stores tokens and cost on every message — each on one machine, each for itself.
Conductor reads those logs and keeps one ledger.

```bash
conductor usage                          # last 7 days, by harness
conductor usage --by day,harness         # a daily series per harness
conductor usage --by model --since 30d   # which models carry the load
conductor usage --by principal           # who used what
conductor usage sync                     # report this directory's unwrapped sessions
```

A session launched through `conductor wrap` reports as it runs: the sidecar re-reads the
harness's own log once a minute, folds it into hourly buckets — one per harness session and
model — and sends only what changed, with a final flush at exit (`CONDUCTOR_USAGE=off`
disables it). Sessions that were not wrapped are reported after the fact with `conductor
usage sync`, which reads the same logs for the current directory. Runner attempts land in
the same ledger from their progress reports. Buckets carry absolute counts, so re-reporting
replaces rather than adds, and a restarted collector cannot double-count.

Cost is what the harness reported where it reports one (OpenCode); otherwise Conductor
estimates it from the organization's model catalog at list price, cache reads at a tenth,
and marks the row `catalog`. A model the catalog does not know stays unpriced rather than
guessed.

What crosses the wire is numbers, a model name, and an hour. The readers decode only the
usage fields of each record; there is no struct field the transcript text could land in,
and OpenCode's export is asked to redact before Conductor even sees it. Team totals by day,
harness, and model are visible to every member. Per-session detail is your own unless you
maintain the project, and other people's model names follow `publishModelIdentity`, exactly
as they do in presence. The dashboard shows usage over time by harness and model, alongside the task board, fleet, swarm, and admission queue.

### Pausing the wall of terminals

A person running three agents has three terminals. Standing up from that desk — a meeting, a
laptop lid, an office move — is one command, and sitting back down is one command, even if
some of those terminals no longer exist by then.

```bash
conductor pause     # freeze every interactive agent session on this machine
conductor resume    # wake them all; --list shows what is saved
```

`conductor pause` finds every interactive Claude Code, Codex, and OpenCode session — launched
through `conductor wrap` or bare — saves a record of how to revive each one under
`~/.conductor/sessions/`, and freezes it with `SIGSTOP`. The terminals stay open, stopped
mid-thought. Before signaling anything, each pid is re-identified against the process table,
because a `SIGSTOP` delivered to a recycled pid would freeze a stranger.

`conductor resume` wakes each session where it can and reopens it where it must:

- **Its terminal survived.** `SIGCONT`, in place. Wrapped sessions come back seamlessly —
  the wrap sidecar stopped only the harness, so the shell never reclaimed the terminal.
  Bare sessions were their shell's foreground job; the shell took the terminal back when they
  stopped, so if the keyboard is dead, `fg` in that terminal hands it over — resume says so.
- **Its terminal was closed.** A new terminal is opened — a window in your current tmux, the
  platform's terminal app, an installed emulator, or a detached tmux session named
  `conductor` as a last resort (`CONDUCTOR_TERMINAL="kitty --directory {cwd} sh -c {cmd}"`
  overrides the choice) — running the harness's own conversation-resume invocation:
  `claude --continue`, `codex resume --last`, `opencode --continue`. Each harness keeps its
  transcript in its own local state, so the conversation survives the terminal; Conductor
  never sees it. One caveat: `codex resume --last` is Codex's most recent conversation
  globally, not per-directory, so two revived Codex sessions can land on the same one —
  `codex resume` opens the picker for the other.

Pausing is for stepping away; saving is for the terminals themselves going away.

```bash
conductor sessions save all     # keep every live session resumable — nothing is stopped
conductor sessions list         # what this machine knows: saved, paused, running
```

A running session's record normally vanishes with its process — right after a crash, wrong
after a reboot with three conversations open. `conductor sessions save all` marks every
session on the machine as deliberately kept. The sessions keep running; if a terminal is
closed or the machine restarts, the record stays, listed as `saved`, and `conductor resume`
reopens the conversation exactly as it reopens a paused session whose terminal was closed. A
saved session you quit yourself is forgotten, as it should be, and a reopened one starts a
fresh record — save again if it should survive the next reboot too. Saving is per-machine and
touches nothing on the server; `conductor sessions export` is the other direction — the
project's whole session history, everyone's, as a JSON file.

### Surviving a shutdown, and the machine itself

You should not have to remember to run `save` before a reboot. Three layers make a shutdown
non-destructive:

1. **Wrapped sessions save themselves.** `conductor wrap` catches the `SIGTERM` a shutdown
   sends (and the `SIGHUP` a closed terminal or dropped SSH connection sends) and marks its
   record kept-for-resume *before* the harness is killed. The harness has already written its
   own transcript, so all that must be preserved is how to reopen it. Nothing to run first.

2. **A machine-wide capture hook** covers bare sessions (no sidecar to catch the signal) and
   unclean shutdowns. `conductor sessions install-hook` generates a systemd user
   service+timer (Linux) or a launchd agent (macOS) that runs `conductor sessions save all` at
   logout/shutdown and periodically — so even a machine that dies without a clean shutdown
   loses at most one interval of state. It writes the unit files and prints the one command to
   enable them; it never starts a system service for you.

3. **Off-host backup**, for when the machine itself does not come back — a terminated cloud
   instance takes its disk, and the local `~/.conductor/sessions` records with it. Point
   Conductor at an S3 bucket and the resume records travel too:

   ```bash
   export CONDUCTOR_BACKUP_S3_BUCKET=my-team-conductor
   export CONDUCTOR_BACKUP_S3_REGION=us-east-1
   export AWS_ACCESS_KEY_ID=…  AWS_SECRET_ACCESS_KEY=…   # or an instance role's env

   conductor backup push        # bundle this machine's records to S3 (a manifest + a snapshot)
   conductor backup status      # where they go, and the latest snapshot
   conductor backup pull        # on a fresh instance: restore them, then `conductor resume`
   ```

   `conductor sessions save all` pushes automatically once a bucket is configured, the
   shutdown hook and the `wrap` SIGTERM handler push on the way down, and `conductor resume`
   pulls first when a machine has no local records — so a replaced instance resumes where the
   old one left off. Objects are keyed by machine under a prefix; set `CONDUCTOR_MACHINE_ID`
   to a stable id if hostnames are not (autoscaled hosts), or to another machine's id to
   adopt its sessions. The S3 client is dependency-free (SigV4 signed over the standard
   library) and works against any S3-compatible store — real S3, MinIO, R2 — via
   `CONDUCTOR_BACKUP_S3_ENDPOINT`. As everywhere else, only coordination metadata travels:
   how to reopen a session, never a transcript. `CONDUCTOR_BACKUP=off` disables it.

Wrapped sessions stay honest with the team while paused: the sidecar keeps heartbeating as
`waiting_for_input`, so presence shows a parked session that is not offered work, rather than
a mystery that stopped moving. A relaunched wrap registers a fresh session with the same
capability flags it was started with.

### Moving a session: another login, another machine, another harness

Everything above reopens a conversation where its harness keeps it — this machine, this
login, this directory. That is the wrong place the day the account hits its usage limit
with the task half done, the cloud instance is reclaimed with the transcript on its disk,
or Claude planned something Codex should finish. A **checkpoint** is the session in one
file: the harness's own transcript, the uncommitted working tree (plus any commits not yet
on a remote), and a harness-neutral `CONTINUATION.md` distilled from the conversation.

```bash
conductor wrap claude                        # checkpoints itself every 2 minutes, and on the way down
conductor checkpoint list                    # what this machine holds
conductor checkpoint resume latest --account work      # usage limit hit: carry on under another login
conductor checkpoint resume latest --harness codex     # carry on in a different agent
conductor checkpoint export latest --seal              # one file, encrypted, for another machine
conductor checkpoint resume session.ckpt --dir ~/src/repo --clone   # …and on that machine
```

A `conductor wrap` session checkpoints itself: every `CONDUCTOR_CHECKPOINT_INTERVAL`
(default two minutes), on the SIGTERM/SIGHUP a shutdown sends, and when the harness exits.
Bare sessions are covered by hooks — `conductor integrate claude` adds `Stop` and `PreCompact`
hooks, `conductor integrate opencode` captures on `session.idle` — and by `conductor sessions
save all`, so the shutdown hook captures conversations as well as resume records. Inside a
session, an agent that reaches a milestone or suspects it is near a limit can call the
`coord_checkpoint` MCP tool with a note on where the work stands. Nothing is written when
nothing changed, and the newest five per session are kept (`CONDUCTOR_CHECKPOINT_KEEP`).

`conductor checkpoint resume` restores the working tree into a checkout (the current one,
or `--dir`, cloned on demand with `--clone`), then:

- **Same harness.** The native transcript is installed where that harness looks, with the
  session's working directory rewritten to the new checkout, and the harness reopens the
  very same conversation: `claude --resume <path>`, `codex resume <id>`, or `opencode import`
  followed by `opencode --session <id>`. `--account NAME` (or `--state-dir DIR`) points the
  harness at a different state directory — `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, or
  `XDG_DATA_HOME` — which is where each keeps both its login and its sessions, so the
  conversation is reopened by that directory's account.
- **Different harness.** Codex cannot read a Claude Code transcript, so it starts from the
  checkpoint's `CONTINUATION.md` — the original request, the conversation so far with one
  line per tool call, the files touched, and the last exchange — with a first prompt that
  tells it to read that file and continue, not start over.

Where this sits in the privacy model matters: a checkpoint contains the conversation, which
the control plane never holds. Checkpoints are written only by the CLI, on your machine,
under `~/.conductor/checkpoints/` (0600 in 0700, beside your credentials), and leave it only
as a file you move yourself or **sealed** — AES-256-GCM under a passphrase — in your own
bucket: `conductor checkpoint push` refuses to upload plaintext and shares the
`CONDUCTOR_BACKUP_S3_*` configuration with `conductor backup`. The `coord_checkpoint` tool
works only in the stdio gateway, on the session's own machine; the HTTP gateway, which runs
in the control plane, refuses it. [docs/PORTABILITY.md](docs/PORTABILITY.md) has the bundle
format and the per-harness mechanics.

### Usage limits: warned before a login runs out

```
conductor quota                          # every login × tool × window: used, resets in, source
conductor quota statusline install       # record Claude Code's documented 5h / weekly limits
conductor quota suggest                  # the resume command for the login with the most room
```

Subscription logins — Claude Pro/Max, a ChatGPT plan in Codex, a Cursor plan — stop the session
when a rolling window runs out. Conductor reads what each tool exposes about those windows on
your machine: Claude Code's status line payload (a documented `rate_limits` object, recorded by
a shim that chains to your own status line so nothing visible changes) and its "limit reached ·
resets …" transcript records; the `rate_limits` Codex writes into every rollout; and, only if
you hand it your session cookie, Cursor's undocumented usage-summary endpoint. Logins are named
by their state directory (`~/.claude-work` is `work`), never by email or token.

At 80% (warn) and 95% (critical) — `CONDUCTOR_QUOTA_WARN` / `_CRITICAL`, or `quota:` in
`.conductor/project.yaml` — the `conductor wrap` sidecar raises the level once per window: a
desktop notification, a `quota.warning` / `quota.exhausted` event, and at critical an immediate
checkpoint plus the exact `conductor checkpoint resume … --account / --harness …` command for
the login or tool with the most headroom. Agents can check their own headroom with
`coord_quota`. Readings are visible only to their owner; teammates see how many of the team's
logins are near their limit, nothing more. `CONDUCTOR_QUOTA=off` disables it all.
[docs/USAGE_LIMITS.md](docs/USAGE_LIMITS.md) has every source, its classification, and the
research behind it.

**VS Code:** integrated terminals are ordinary ptys, so pausing and in-place resume already
work there. Reopening a *closed* session into VS Code needs the companion extension in
[`integrations/vscode`](integrations/vscode) — VS Code offers no command-line way to open an
integrated terminal running a command, so `conductor resume` hands the session to the
extension via a `vscode://` URI (carrying only a record id, never a command) and the
extension opens the terminal in the session's working directory. Which sessions lived in
VS Code is recorded from `TERM_PROGRAM` at save time; without the extension installed,
resume simply falls back to the terminal chain above. The extension also adds
`Conductor: Pause All Agent Sessions` and `Conductor: Resume All Agent Sessions` to the
command palette.

### Notifications: Slack, Discord, and webhooks

```
conductor notify add slack https://hooks.slack.com/services/T…/B…/… --name "#eng-agents"
conductor notify add webhook https://ci.example.com/conductor      # prints its signing secret once
conductor notify test <id>          # send a test message now
conductor notify                    # channels and their delivery health
conductor notify events             # what can be sent, and the defaults
```

A channel sends the project's events to a Slack incoming webhook, a Discord webhook, or any
HTTPS endpoint as signed JSON. By default it gets the moments a team acts on: someone refused
territory another task holds (`conflict.blocked`) or starting work that looks like a task
already in flight (`conflict.suggest_join`) — each at most once per person, task and outcome
every 15 minutes, however often an agent retries — a new medium-or-worse conflict in the
merge-risk graph (`conflict.detected`, once while it stays open), work paused on a conflict
(`task.status_changed:blocked_conflict`), territory someone was waiting for is free
(`scope.released`, naming who was waiting), an agent stalled or lost its lease
(`attempt.stalled`, `lease.expired`), a pull request merged, a task done or failed, the
project budget crossing its downshift or pause threshold, and a teammate's login near or at
its usage limit (`quota.warning`, `quota.exhausted`). `--events` picks others from
`conductor notify events`; `"*"` sends all of them. A channel hears about what happens after
it is added, not the backlog. Channels are managed by maintainers, from the CLI, the API
(`/v1/projects/{p}/notifications`), or the Notifications card in the dashboard's Settings.

**What leaves.** A channel is project-wide, and Slack is not Conductor: every event goes
through the same visibility projection the API applies for an ordinary project member, then
narrower still for private work — an event about a private task says "a private task" and
nothing else: no title, no ref, no paths or territory. Prompts and transcripts were never in
events to begin with.

**Credentials.** A Slack or Discord URL is the credential, and a webhook's signing secret is
what its receiver trusts, so both are sealed in the database under conductord's secret key
([docs/OPERATIONS.md](docs/OPERATIONS.md#the-secret-key)) and never returned after creation —
the API shows the host and last four characters. URLs must be `https`, and conductord refuses
to connect to loopback, private, link-local and other non-public addresses (checked on the
address actually dialed, after DNS), so a channel cannot be pointed at the control plane's own
network. A self-hosted chat server on your LAN needs `conductord
--notify-allow-private-networks`; `--notify-allow-http` is for local testing only. Where
conductord reaches the internet only through a proxy, pass `--notify-proxy URL` (or
`CONDUCTOR_NOTIFY_PROXY`); destinations are still resolved and checked before the proxy is
asked for them.

**Delivery** is at least once — deduplicate on the `id` field (also `X-Conductor-Delivery`).
A failing endpoint is retried with exponential backoff (15s doubling, at most an hour) and an
event is given up after 8 failed attempts or 24 hours; a 4xx answer other than 408 or 429 is
not retried. `conductor notify` shows each channel's last error.

**Verifying a webhook.** Each request carries `X-Conductor-Timestamp` (Unix seconds) and
`X-Conductor-Signature: sha256=<hex>`, an HMAC-SHA256 keyed with the whole secret (`whsec_…`)
over the timestamp, a `.`, and the raw body. Check both, against the raw bytes before any JSON
parsing, and refuse a timestamp more than five minutes off — that is what stops a captured
request from being replayed.

```python
import hashlib, hmac, time

def verify(secret: str, headers, body: bytes, tolerance: int = 300) -> bool:
    ts = headers.get("X-Conductor-Timestamp", "")
    sig = headers.get("X-Conductor-Signature", "")
    if not ts.isdigit() or abs(time.time() - int(ts)) > tolerance:
        return False
    mac = hmac.new(secret.encode(), ts.encode() + b"." + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(sig, "sha256=" + mac)
```

```go
func verify(secret string, h http.Header, body []byte) bool {
	ts := h.Get("X-Conductor-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(h.Get("X-Conductor-Signature")), []byte(want))
}
```

The body is `{"id", "type", "project", "occurred_at", "subject", "private", "text", "url",
"data"}`: `text` is a one-line summary, `data` the event's payload as a project member sees
it, and `url` a dashboard link when conductord has a public `--public-url`.

---

## How it holds together

```
 Claude Code · Codex · OpenCode · human sessions
        │                    │
   MCP tools           conductor CLI / wrap
        └────────┬───────────┘
                 ▼
      control plane (conductord)
   ledger · leases · reservations · conflict graph · presence
                 │
          PostgreSQL (source of truth)
                 │
      scheduler ─┴─ adaptive router
                 │
   harness drivers → isolated git worktrees
```

Four mechanisms do the real work:

**Transactional claims.** `SELECT … FOR UPDATE SKIP LOCKED` makes duplicate dispatch
structurally impossible rather than unlikely. Two schedulers racing the same ready queue
cannot select the same row, so replicas need no leader election.

**Fencing epochs.** Every claim gets a strictly higher epoch. A worker that was paused, lost
its lease, and woke up later presents a stale epoch and is rejected — it can keep writing in
its own worktree, but it can never publish. Expiry alone does not close that window; the epoch
does.

**Reservations under an advisory lock.** Territory is per-resource (file, directory, glob,
migration lane, table, API route, symbol), and acquisition takes a per-project advisory lock so
check-then-insert cannot interleave. Without it, two agents each see a clear field and both
plant a flag.

**Merge risk from observed diffs.** Runners report the paths git says changed, and so does the
`conductor wrap` heartbeat for interactive sessions, so the conflict graph is built from what
agents and people are *doing*, not only what they declared. That is what turns a merge-time
disaster into a minute-five warning.

---

## What is built, and what is not

Implemented and exercised by tests:

- Task ledger with the full DESIGN.md §9 state machines, enforced in Go and in Postgres.
- Atomic claims, expiring leases, fencing epochs, reclamation, retry budgets.
- Scope reservations across all nine resource types, with the complete §11.3 conflict matrix.
- Privacy-preserving duplicate detection (HMAC + MinHash), field-level visibility projections.
- Conflict graph: scope overlap, duplicate intent, merge risk, with join/wait/split advice.
- Presence, event log with gapless per-aggregate sequencing, SSE stream, live dashboard.
- REST API, MCP gateway, CLI, session wrapper with heartbeat sidecar.
- Scheduler: reconcile (with outage recovery, so a control-plane outage does not reclaim
  live work), session reaping, stall detection, dependency gating, budget events announced
  once per threshold crossing, and retention.
- Operations: ordered graceful shutdown, request ids and access logs, Prometheus `/metrics`,
  `/v1/ready`, bounded database calls, request-body deadlines, capped event streams over a
  shared per-project feed, a schema-version guard, multi-replica-safe GitHub state, and a
  tested Postgres backup/restore script (docs/OPERATIONS.md).
- Adaptive router: hard floors, tiers, escalation, de-escalation, budget guard.
- Session capability advertisement and capability-aware assignment: sessions declare the model
  and reasoning effort they are running, the catalog decides what that is worth, and work with
  a capability floor is offered to a session that clears it.
- Shareable per-member token budgets: a rolling-window allowance each member can transfer to
  a teammate, enforced at claim time and settled entirely by ledger arithmetic.
- Harness drivers for Claude Code, Codex, OpenCode, a generic templated `exec` driver, and a
  deterministic in-process fake.
- Shutdown-durable sessions: `conductor wrap` saves its own session on the SIGTERM/SIGHUP a
  shutdown or closed terminal sends; `conductor sessions install-hook` adds a systemd/launchd
  hook that captures bare sessions at shutdown and periodically; and a dependency-free,
  SigV4-signed S3 backend (`conductor backup push|pull`) carries the resume records off-host,
  so a terminated cloud instance resumes on its replacement.
- Machine-local pause/resume: `conductor pause` freezes every interactive agent session on
  the machine and `conductor resume` revives them — in place, or in freshly opened terminals
  on each harness's own conversation-resume invocation.
- Token-free local sign-in for the machine's owner (dashboard, CLI, macOS app), guarded
  against drive-by pages, DNS rebinding, and proxies, with an enhanced security mode that
  requires tokens everywhere and revokes what local sign-in issued.
- A GitHub App created in one click through GitHub's manifest flow. It posts a "Conductor"
  check run on each pull request that overlaps reserved or in-flight work, links the pull
  request to its task, and completes the task when it merges — by webhook, or by polling when
  GitHub cannot reach the daemon. Opt-in issue sync turns a repository's labelled issues into
  tasks and writes each task's claim and completion back to its issue.
- Session portability: `conductor checkpoint` bundles a session's native transcript, working
  tree, and a harness-neutral continuation into one file — taken periodically by `conductor
  wrap`, by Claude Code and OpenCode hooks, at shutdown, and on an agent's own `coord_checkpoint`
  call — and `conductor checkpoint resume` continues it on another machine, under another
  login, or in a different harness, sealed with a passphrase whenever it leaves the machine.
- Isolated git worktrees, scope-drift detection, runner-attested validation, evidence manifests,
  handoff bundles, portable Markdown task cards.
- Member and token administration, TLS, a loopback-by-default bind, and auth throttling.
- Single sign-on through any OpenID Connect provider or GitHub, for the dashboard and the CLI,
  issuing ordinary tokens; external identities link only to accounts an administrator
  registered (§25.7).
- Daemon-to-daemon peering over mutual TLS: a private CA names every control plane in a
  mesh, each daemon dials its configured or DNS-discovered peers and keeps a live link
  table, and `conductor peers` reports it. Connectivity and identity only — no data is
  replicated across the link. `--peer-discover-dns` finds peers via a DNS SRV record so a
  mesh can grow without an operator hand-listing every daemon's address.
- A runner that reaches the control plane over HTTP and holds no database credential
  (§28.2), alongside the in-process backend for single-host use (§28.1).
- One-link onboarding: `conductor invite <handle>` mints a teammate their own token and prints
  a single join link (token in the URL fragment, off the wire); `conductor join <link>` redeems
  it, and the same link self-connects the web dashboard.
- One-command integration into eight coding tools (Claude Code, Cursor, Codex, OpenCode,
  Windsurf, VS Code, Zed, Gemini CLI): MCP config plus, where supported, pre-edit hooks that
  run the conflict check before every edit and block on a hard conflict.
- MCP over Streamable HTTP served by `conductord` itself, so an HTTP-capable client connects
  with a bearer token and no local binary — the same thirteen tools as the stdio gateway
  (all but `coord_checkpoint`, which only the stdio gateway can honour, since it runs on
  the harness's own machine).
- Repository dispatch policy: named lanes, ordered model ladders, `when`-gated candidates, and
  hard-floor-respecting escalation, evaluated by a small deterministic expression language,
  with `conductor route` to preview and `conductor policy lint` to validate.
- A pooled-capacity swarm view and per-member budget sharing across a team, and an admission
  queue that makes sessions and attempts wait for a slot instead of failing when the team is at
  capacity, granted in arrival order with heartbeat-expiry hand-off.
- A single-page dashboard (no build step, no external requests): Home (check before an edit,
  who is on what, what is waiting on you), Tasks, People and Settings, with the fleet, swarm,
  usage, events, queue and integration views under More, and an Admin area for organization
  administrators.
- Enterprise administration: a server config file that can lock organization settings,
  require-SSO, Entra ID tenant trust, SCIM 2.0 provisioning, group → role mapping, token
  lifetime caps, feature flags, branding, and an exportable audit log.
- Notifications to Slack, Discord, and signed webhooks, relayed from the transactional outbox
  with retries, through the same privacy projection as the API.

Not built, and where the design says it goes:

- **The macOS app.** Planned in [docs/MACOS_APP.md](docs/MACOS_APP.md): a menu bar app that
  runs the daemon and a private Postgres for you, signs you in automatically, and shares
  invites through Messages.

- **Planner and reviewer services** (§14, §15.3). The contracts, validation rules, and
  `reviewer.*` routing are in place; nothing yet invokes a model to decompose an objective or
  review a diff.
- **Codex App Server driver** (§16.3). The Codex driver shells out to `codex exec --json`
  rather than binding the bidirectional JSON-RPC App Server.
- **Merge queue, symbol/tree-sitter indexing** (§29, §30 phase 5). Pull requests are
  integrated as far as the check run and merge-to-done above; nothing queues or performs
  merges.
- **Tracker sync beyond GitHub Issues** (§17.5). GitHub Issues sync is built; Linear is the
  next adapter, then Jira.
- **Codex** is profiled as `gpt-5.3-codex` in `.conductor/models.yaml` but left disabled until
  someone verifies it against their account; its `exec --json` stream adapter is tested against
  fixture transcripts built from Codex's documented event schema, not a live run. **OpenCode**
  is wired to local vLLM: Qwen 3.8 27B, GLM-5.3-Flash, and GLM-5.3 (`conductor serve
  qwen|flash|glm53`, then `conductor wrap opencode --model vllm/…`).

One deliberate deviation from the design document: it recommends TypeScript (§28.1). This is
Go, at the repository owner's direction. The tradeoff is real — the Claude Agent SDK and
OpenCode SDK are TypeScript, so their drivers here are CLI-based rather than SDK-based.

---

## Testing

```bash
make unit     # pure logic, no database
make test     # everything; integration tests skip without DATABASE_URL
make db-up && make test
make e2e      # scripted two-person scenario end to end
```

CI runs all of it against a real Postgres (16 and 17) on every push, with and without the
race detector, and fails if the integration tests skip — a misconfigured database service
would otherwise produce a silently green run. It also runs `staticcheck` and `govulncheck`,
and builds and unit-tests on macOS, which the release ships binaries for.

`scripts/e2e.sh` asserts the MVP acceptance criteria of DESIGN.md §31 rather than printing
output for a human to eyeball: that a completed task carries a commit and runner-observed
validation, that the attempt ran in a per-task worktree, that the workflow and config hashes
and the model routing were recorded, and that presence exposes a branch and a heartbeat and
nothing resembling a conversation.

Two acceptance criteria are still unverified, both for the same reason: nothing here has ever
launched a real Claude Code, Codex, or OpenCode process. §31.5 (each harness registers and
publishes progress) and the live half of §31.6 are exercised only through the built-in fake.

The suite proves the invariants rather than asserting them in prose. Notably:

| Test | Proves |
|---|---|
| `TestConcurrentClaimsYieldExactlyOneLease` | 24 concurrent claims → exactly one winner |
| `TestStaleFenceIsRejected` | a reclaimed worker cannot heartbeat, release, or publish |
| `TestConcurrentOverlappingReservationsSerialize` | 12 racing migration reservations → one winner |
| `TestClaimNextDoesNotDoubleDispatch` | 6 scheduler replicas, 8 tasks, no double dispatch |
| `TestReclamationReleasesReservations` | a dead session frees its territory |
| `TestPrivateTaskHidesIntentButKeepsTerritory` | private work still prevents collisions |
| `TestNoTranscriptColumnsInSchema` | the database has nowhere to put a prompt |
| `TestMatrixMatchesDesign` | all 25 cells of the §11.3 conflict matrix |
| `TestSecurityFloorIsAbsolute` | budget pressure cannot downgrade a security-sensitive task |
| `TestPrivateTaskIsRedactedOverHTTP` | the projection survives serialization, not just unit tests |
| `TestNonMemberSeesNotFoundNotForbidden` | a 403 would confirm the project exists |
| `TestStaleFenceIsA409` | a stale worker gets "stop", not "retry" |
| `TestParseFlagsAcceptsFlagsAfterPositionals` | CLI flags after a positional are not silently dropped |
| `TestSimilarityIsStableAcrossKeys` | duplicate detection does not miss real collisions across tenant keys |
| `TestMCPWorkLifecycleAgainstLiveServer` | the MCP gateway works against the real API, not a stub |
| `TestQueuedAttemptCannotSucceed` | an attempt cannot report success without having run |
| `TestProbeUntrustedPeer` | a daemon certified by another CA can never pass as a peer |
| `TestDiscoverAddsPeersFromSRV` | a daemon joins the mesh via DNS alone, no `--peer` list required |
| `TestPeerInfoRequiresMeshCertificate` | a bearer token is not a peer credential; only a mesh certificate is |

---

## Peering daemons

A mesh is a set of `conductord` instances that know each other by certificate. One CA is
generated per mesh; every daemon holds a certificate signed by it and uses that single
certificate in two roles — served as its TLS certificate, and presented as its client
certificate when dialing peers. A peer link is therefore mutual TLS: each side proves it
holds a mesh-issued key, and the mesh CA is the only root either side trusts for it.

Peering carries connectivity and identity, nothing else. Each daemon probes its
configured peers (`GET /v1/peer/info`) and records state, round-trip time, and the
identity that answered; project members read that link table with `conductor peers`. No
tasks, scopes, or events cross the link — the database remains each daemon's own source
of truth.

```bash
# once per mesh: a CA, then one certificate per daemon
scripts/gen-peer-certs.sh laptop desktop

# per daemon (each points at the other)
conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/laptop/cert.pem --peer-key .conductor/certs/laptop/key.pem \
  --peer desktop=https://desktop.example.com:8443

conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/desktop/cert.pem --peer-key .conductor/certs/desktop/key.pem \
  --peer laptop=https://laptop.example.com:8443

conductor peers   # PEER ADDRESS STATE RTT LAST CHECK
```

Env equivalents: `CONDUCTOR_PEERS=name=url,…` (comma-separated) plus
`CONDUCTOR_PEER_CA`, `CONDUCTOR_PEER_CERT`, `CONDUCTOR_PEER_KEY`. Clients verify the
daemon with `CONDUCTOR_CA_CERT=.conductor/certs/ca.pem`. Peer URLs must be `https` —
a plaintext peer would put the mesh identity on an unauthenticated wire, and the daemon
refuses it.

### Joining a mesh without seed nodes

`--peer name=url` does not scale past a couple of daemons — every one needs every other
one's address by hand. `--peer-discover-dns` replaces that with one shared DNS SRV record
(RFC 2782): every daemon resolves it on each probe tick and treats each target as a peer
to dial, the same join-by-DNS pattern a Kubernetes headless service or Consul uses.

```bash
# publish an SRV record once, e.g. _conductor-mesh._tcp.mesh.internal pointing at every
# daemon's host:port — DNS round-robin, a headless Service, or a Consul/etc service catalog

conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/laptop/cert.pem --peer-key .conductor/certs/laptop/key.pem \
  --peer-discover-dns _conductor-mesh._tcp.mesh.internal

conductord --addr 0.0.0.0:8443 \
  --peer-ca .conductor/certs/ca.pem \
  --peer-cert .conductor/certs/desktop/cert.pem --peer-key .conductor/certs/desktop/key.pem \
  --peer-discover-dns _conductor-mesh._tcp.mesh.internal
```

No daemon names another by hand: a discovered address is dialed, and once it answers with
a certificate the mesh CA vouches for, it's identified by whatever name its own
`/v1/peer/info` reports — DNS only proposes where to look, never who to trust. `--peer` and
`--peer-discover-dns` compose freely (`conductor peers` marks a discovered link `(dns)`),
and a resolver hiccup only pauses discovery of *new* peers — it never drops one already
linked. Env equivalent: `CONDUCTOR_PEER_DISCOVER_DNS`.

The lookup uses the system resolver, which on some machines never sees a laptop-local
directory (Go ignores `/etc/resolver`). `--peer-dns-server host:port` points the lookup
straight at a directory server instead — `CONDUCTOR_PEER_DNS_SERVER` is the env form.

---

## Configuration

Policy lives in the repository, versioned with the code it governs, and every attempt records
the hash of the files in force when it ran — so a result can always be explained by the rules
that produced it.

| File | What it controls |
|---|---|
| `.conductor/project.yaml` | lease TTLs, heartbeat cadence, visibility defaults, isolation |
| `.conductor/policies.yaml` | conflict matrix, duplicate thresholds, hard routing rules, budgets |
| `.conductor/models.yaml` | model aliases (roles), capability floors, concrete profiles |
| `.conductor/WORKFLOW.md` | the prose contract every agent reads; required checks; protected scopes |

**These files are code, not just settings, wherever a `conductor worker` runs.** The harness
`command`, `arg_template`, and `mcp_servers` in `project.yaml` are executed by the worker, and
the required checks run as `sh -c` inside the worktree the agent just edited — an edited
`Makefile` included. The worker runs them as its own user with no sandbox; it strips credentials
from their environment (and hands the agent a short-lived, project-scoped token instead of
yours), but it cannot stop code from reading that user's files. Run a worker only for
repositories and teammates you would let run code on that machine (DESIGN.md §25.3).

The control plane's own configuration — flags, environment, and the server config file
(`conductord --config`) — is described in [docs/OPERATIONS.md](docs/OPERATIONS.md#the-config-file)
and, for organization policy, in [For enterprises](#for-enterprises).

Running the control plane itself — probes and `/metrics`, shutdown and outage behaviour,
database timeouts, retention windows, running several replicas, and backing up and restoring
Postgres (`scripts/pg-backup.sh`) — is covered in [docs/OPERATIONS.md](docs/OPERATIONS.md).

---

## Removing Conductor

`make uninstall` (or deleting the three binaries) removes the program, not what it created.
All of it, from least to most destructive:

```bash
conductor down --db                          # stop the control plane and the conductor-db container
conductor integrate <tool> --remove          # for each tool you connected (claude, codex, cursor, …)
conductor sessions install-hook --uninstall  # if you installed the shutdown hook
make uninstall                               # the binaries in ~/.local/bin and the PATH entry
                                             # (~/.zshrc, ~/.bashrc or ~/.bash_profile, or fish conf.d)
rm -rf ~/.conductor                          # credentials, pidfile and log, saved sessions,
                                             # checkpoints, extra harness accounts, GitHub App key
```

`~/.conductor/checkpoints` may hold the only copy of a session you captured; export anything
you want to keep (`conductor checkpoint export`) first.

In each repository you ran `conductor init` in, `.conductor/` holds the policy files (versioned
with your code; keep them if you might come back) and `.conductor/runtime/` the task worktrees.
`init` also added a block between `<!-- conductor:begin -->` and `<!-- conductor:end -->` to
`CLAUDE.md` and `AGENTS.md`; delete it by hand.

The database is last because it is every task, reservation, and member of every project on
this control plane. If `conductor up` started it in Docker:

```bash
docker rm -f conductor-db && docker volume rm conductor-pgdata
```

If you pointed Conductor at your own Postgres, drop its database there instead.

---

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
