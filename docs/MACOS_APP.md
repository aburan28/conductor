# Conductor for macOS: plan

Status: phases 1 to 5 implemented in [`macos/`](../macos/README.md) (see "Phases" for what
has been verified, and what needs a Mac and a Developer ID to verify). Scope: macOS 13
(Ventura) and later, Apple silicon and Intel.

## Why an app

Conductor is a CLI, a daemon, a database, and a dashboard. For the person who just wants
their agents to stop colliding, that is four things to install, start, and keep running, and
a token to find. The app makes it one thing:

- **Install, open, done.** The app runs the control plane and its database for you, starts
  them at login, and signs you in without a token (local security mode, see
  `conductor security`).
- **Always visible.** A menu bar item shows who is live, what is contested, and work offered
  to you, without opening a browser.
- **Share by text.** "Invite someone" makes a link and hands it to Messages through the
  macOS share sheet. Your brother taps it and is in.
- **Move work when a limit hits.** Checkpoints of every session, one click to continue under
  another account or in another tool.

## What already exists to build on

| Need | Already in Conductor |
|---|---|
| Sign in with no token on this machine | `POST /v1/local/session` (client `mac-app`), `GET /v1/local/status`, `conductor security` |
| Every screen of a dashboard | the web dashboard, served by conductord, same-origin, no external requests |
| Live updates | `GET /v1/projects/{p}/events/stream` (SSE) |
| Invite and join | `conductor invite` (link with the token in the fragment), `conductor join` |
| Connect coding tools | `conductor integrate all --global` |
| Pause and wake every agent terminal | `conductor pause`, `conductor resume` |
| Portable sessions | `conductor checkpoint list / resume --account / --harness` |
| GitHub | `conductor github setup / status / link` |

The cairn repository's macOS apps are the template (paths are in `aburan28/cairn`):

- `gui/macos-app`: process supervision (`Node.swift`), the launchd agent
  (`BackgroundService.swift`), the web view and its bridge (`WebView.swift`), Sparkle
  updates gated on signing keys (`Updates.swift`), the build script (`build.sh`), and DMG
  packaging (`packaging/macos/build-dmg.sh`).
- `gui/macos`: the native-view pattern: `WindowGroup` + `MenuBarExtra`, a `NavigationSplitView`
  sidebar, and a polling model.
- `AIProvider.swift`: an SSE line splitter and parser that handles the blank line ending each
  event, which `URLSession.bytes(...).lines` silently drops.

## Architecture

```
Conductor.app (SwiftUI, macOS 13+)
├── MenuBarExtra ─────────── live sessions, conflicts, offers, quick actions
├── Window ───────────────── WKWebView of the dashboard (http://127.0.0.1:<port>/)
├── Sheets ───────────────── Invite, Connect tools, GitHub, Checkpoints, Security
├── Settings ─────────────── port, start at login, security mode, data folder
│
├── Supervisor ───────────── owns two launchd user agents:
│     dev.conductor.postgres   private Postgres on a Unix socket, data in ~/Library/Application Support/Conductor/pg
│     dev.conductor.daemon     conductord --addr 127.0.0.1:<port> --dsn <socket DSN>
├── API client ───────────── URLSession + Codable; token from local sign-in, kept in the Keychain
└── Event stream ─────────── SSE → @MainActor model → menu bar + notifications
```

**Hybrid, web view first.** The dashboard already has every screen, and a second native
copy of each would drift. The app wraps it, the way Cairn.app does, and adds natively only
what a web page cannot do: the menu bar, notifications, the share sheet, launchd, the
Keychain, and the first-run setup. Native screens can replace web ones later, one at a time,
behind the same API.

### The database is the hard part

conductord needs PostgreSQL. It relies on `SKIP LOCKED`, advisory locks, and partial unique
indexes, so SQLite is not a drop-in. Today `conductor up` gets Postgres from Docker, and asking
a Mac user to install Docker defeats the purpose of the app. The plan:

1. **Bundle a private Postgres.** Ship PostgreSQL 17 server binaries inside the app (about
   30 MB, from the PostgreSQL project's macOS builds, universal). `initdb` it on first run into
   the app's support folder, listen on a Unix socket only (no TCP port, nothing to collide
   with an existing Postgres), and run it as its own launchd agent.
2. Keep "attach to an existing database" in Settings, for people who already run Postgres.
3. Out of scope for v1: replacing Postgres. If the bundle proves too heavy, the follow-up is
   an embedded store behind the `db.Store` interface, which is a large change on its own.

### Signing in

1. On launch, the app calls `GET /v1/local/status`. When `local_login_available` is true it
   calls `POST /v1/local/session {"client":"mac-app"}`. A native app sends no `Origin` header,
   and the request comes from loopback with a loopback Host, so the server accepts it.
2. The token goes into the Keychain (`kSecClassGenericPassword`, service `dev.conductor`,
   account `<endpoint>`). The app never writes it to `UserDefaults` or to disk.
3. The web view needs no token injection. It is the same origin, so the dashboard signs
   itself in through the same endpoint.
4. In enhanced security mode the app shows a token field, or accepts a join link.

### Sharing with someone (the "text my brother" journey)

1. **Invite someone** asks for a handle and a role, calls the members API, and builds the
   link.
2. The link is handed to `NSSharingServicePicker` (Messages, Mail, AirDrop) with one line of
   text: *"Join my Conductor swarm: <link>"*.
3. Reachability: a link to `127.0.0.1` reaches only you. The sheet checks, in order:
   - a `--public-url` the daemon was started with;
   - **Tailscale**: if it is running, offer "Share over Tailscale", which runs
     `tailscale serve --bg <port>` and uses the machine's MagicDNS name (this is what
     `conductor invite` already suggests);
   - otherwise it explains that the other person must be able to reach this Mac.
4. The person receiving the text needs nothing installed: the link opens the dashboard in
   their browser. With the app installed, a `conductor://join#…` link opens the app instead,
   which runs the join and connects their tools.

Local sign-in never leaks through a shared link. Requests arriving through the tailnet name
carry that name as their Host, so the server treats them as remote: they need the token in
the link.

### Usage limit hit: continue elsewhere

The Checkpoints window lists `conductor checkpoint list --json`, newest first, per session.
Each row offers three actions:

- **Continue here** runs `checkpoint resume <id>`.
- **Continue under account…** lists `~/.claude-*` and the other configured logins, then runs
  `resume --account`.
- **Continue in…** offers Codex or OpenCode and runs `resume --harness`.

The resume opens in Terminal (or the user's terminal app) through the same opener
`conductor resume` uses. A notification fires when a session hits its usage limit. The first
version detects this from the harness exiting with a limit message; version 2 adds a server
event.

## Screens

| Surface | Contents | Source |
|---|---|---|
| Menu bar icon | dot: green (all clear), amber (conflict or offer), grey (daemon down) | SSE + `/v1/projects/{p}/status` |
| Menu bar popover | live sessions; open conflicts with "join / wait / split"; offers with accept/decline; Pause all / Resume all; Open dashboard | status, presence, inbox APIs; `conductor pause/resume` |
| Main window | the dashboard in a web view | conductord |
| First run | 1. start database and daemon; 2. sign in (automatic); 3. pick a repository folder (`conductor init` + bootstrap); 4. connect tools; 5. optional GitHub | `up`, `init`, `integrate`, `github setup` |
| Invite sheet | handle, role, expiry; share sheet; reachability hint | members API |
| Connect tools sheet | per-tool status from `conductor doctor --json`, one Connect button each, or all | `integrate` |
| GitHub sheet | Create app, Install, linked repositories, last poll | `github status/setup/link` |
| Checkpoints window | sessions and checkpoints; continue here, under account, in another harness | `checkpoint` |
| Settings | port; start at login; security mode (local/enhanced) with explanation; data folder; attach to an existing daemon or database; updates | `security`, launchd |

## Packaging

The cairn recipe, unchanged where possible:

- **SwiftPM, no Xcode project.** A `ConductorKit` library target holds the API client, models,
  SSE, supervisor, and Keychain code. `Conductor` is a thin executable. Tests depend on the
  library.
- **Build.** `build.sh` runs one `swift build --triple` per architecture, then `lipo`. It
  assembles `Contents/{MacOS,Resources,Frameworks}`, copies the `conductor`, `conductord` and
  `conductor-mcp` binaries (Go cross-compiles both architectures) and the Postgres bundle into
  `Resources`, and copies frameworks with `ditto`, not `cp -R`.
- **Distribution.** A `.dmg` containing a `.pkg` that installs the app and symlinks
  `/usr/local/bin/conductor`, so the CLI and the coding tools' MCP configs find the same
  binaries.
- **Signing.**
  - Developer ID plus notarization, with partial signing configurations refused.
  - Read the `notarytool` status output, because it exits 0 even when Apple rejects the build.
  - The hardened runtime is needed, but not the App Sandbox: the app spawns processes and
    writes `~/Library/LaunchAgents`.
- **Updates.** Sparkle, pinned to an exact version, and disabled unless the public key is in
  `Info.plist`.
- **`Info.plist`.**
  - `NSAllowsLocalNetworking` (plain HTTP to loopback).
  - `NSLocalNetworkUsageDescription`, needed on macOS 15 for Tailscale and LAN addresses.
  - `CFBundleURLTypes` for `conductor://`.

## Testing

- `swift test` on `macos-latest` on every PR. Logic lives in `ConductorKit`, outside the
  views: the launchd plist builders, decoders, the SSE parser, invite-link building, and
  Keychain wrappers (against a test keychain).
- An integration job starts the bundled Postgres and conductord from the built app, runs
  the local sign-in, and drives the dashboard through `WKWebView`-free HTTP checks.
- Warnings are not fatal, so a new SDK's deprecations do not fail unrelated PRs.

## Phases

| Phase | Delivers | Done when | State |
|---|---|---|---|
| 0. Groundwork (this repository) | local sign-in, `conductor security`, the GitHub App, Tailscale-aware invites, `--json` everywhere | merged (this PR) | done |
| 1. Menu bar + supervisor | `ConductorKit`; launchd agents for the bundled Postgres and conductord; automatic sign-in; the menu bar popover from status + SSE; web-view window | a fresh Mac with no Docker opens the app and sees its dashboard with no terminal | implemented |
| 2. Onboarding | first-run flow; Connect tools; Invite with the share sheet and the Tailscale option; `conductor://join` | a second person joins from a texted link, and their Claude Code shows Conductor's MCP tools | implemented |
| 3. Portability | Checkpoints window; usage-limit notification; continue under another account or harness | a session stopped by a limit continues under a second login in two clicks | implemented |
| 4. GitHub | the GitHub sheet; check-run status per pull request in the popover | the app is created and installed from the sheet | implemented |
| 5. Ship | Developer ID, notarization, DMG/pkg, Sparkle | a notarized DMG installs and updates itself | implemented, unsigned |

Beyond the plan, Settings → Storage configures the bucket of [STORAGE.md](STORAGE.md): the
supervisor archives the bundled Postgres to it, installs a base-backup agent, and offers
Restore from bucket on a new Mac.

### What has been verified, and what has not

The implementation was written where no Mac was available. What could be checked there:

- `ConductorKit`, which holds all the logic (plists, supervisor, API and SSE clients,
  decoders for every `--json` the app reads, storage settings, join links, AWS profiles),
  builds and passes its tests on Linux.
- An integration test ran the supervisor's Postgres setup and conductord for real (with
  PostgreSQL 16 and this repository's conductord, without launchd): initdb with the app's
  arguments, the socket-only configuration under a path with spaces, conductord on the
  socket DSN, bootstrap, local sign-in, a `quota.exhausted` event through the SSE client,
  and an invite link that `conductor join` accepted.
- `build.sh --go-only` cross-compiled the Go commands for both architectures;
  `fetch-postgres.sh` was exercised on a synthetic archive (the real one was downloaded
  once to pin its hash); the signing rules and `notarytool` status parsing have tests.
- The SwiftUI sources were syntax-checked, and the app model type-checked against
  ConductorKit, but not compiled.

What needs a Mac, and is left to the `macos-app` workflow and a person with one:

- compiling the `Conductor` target and `ConductorKeychainACL`'s Security calls, and the
  first run of the app: launchd loading the agents, the window, the menu bar, the share
  sheet, notifications, `conductor://` links, SMAppService;
- whether `/usr/bin/security` reads the shared Keychain items without a prompt (the item's
  partition list, which macOS added after access lists, could still make it ask);
- the EnterpriseDB binaries running from inside the bundle, signed by the app, under the
  hardened runtime.

What needs a Developer ID and an App Store Connect key, and has never run: Developer ID
signing, notarization and stapling, a signed `.pkg`, and Sparkle updates (which also need
`SPARKLE_PUBLIC_ED_KEY` and a published `appcast.xml`).

## Risks and open questions

- **The bundled Postgres** is the largest piece and the riskiest: size, upgrades between
  major versions (`pg_upgrade` on app update), and corruption on hard power loss (WAL makes
  this rare). Decide before Phase 1 whether the app may require Docker as a stopgap.
- **Signing identity**: who holds the Developer ID and the Sparkle key. Losing the update key
  strands every installed copy.
- **One owner per machine**: local sign-in acts as whoever bootstrapped first. A Mac with two
  macOS user accounts should run in enhanced mode. The app should detect a second
  console user and say so.
- **Tailscale serve** needs the user's consent and tailnet policy. The app runs it only after
  the user clicks, and shows how to stop sharing (`tailscale serve reset`).
- **Other platforms**: the CLI already covers Linux. A Windows or Linux GUI would reuse the
  web view approach (Tauri or Electron) and is not planned.
