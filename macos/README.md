# Conductor for macOS

`Conductor.app` runs the control plane, its database, and the dashboard as one Mac app:
install, open, done. The plan and the reasons behind it are in
[docs/MACOS_APP.md](../docs/MACOS_APP.md); the storage settings it edits are
[docs/STORAGE.md](../docs/STORAGE.md).

## What is here

```
macos/
├── Package.swift              SwiftPM, no Xcode project
├── Sources/
│   ├── ConductorKit/          all the logic; builds and is tested on Linux too
│   ├── ConductorKeychainACL/  the deprecated Keychain access-list calls, isolated
│   └── Conductor/             the SwiftUI app (macOS only)
├── Tests/ConductorKitTests/
├── Info.plist, Conductor.entitlements
├── build.sh                   the universal .app
├── fetch-postgres.sh          the pinned PostgreSQL 17 bundle
└── packaging/                 signing, notarization, .pkg and .dmg
```

| Piece | Where |
|---|---|
| launchd plists for Postgres, conductord, the base backup | `ConductorKit/LaunchAgent.swift` |
| The supervisor (initdb or restore, archiving, start, wait, createdb) | `ConductorKit/Supervisor.swift` |
| Postgres configuration, socket folder, DSN | `ConductorKit/Postgres.swift`, `AppPaths.swift` |
| Local sign-in and the API | `ConductorKit/APIClient.swift`, `APIModels.swift` |
| SSE parsing and the event stream | `ConductorKit/SSE.swift`, `EventStream.swift` |
| Join links, Tailscale | `ConductorKit/Invite.swift`, `Reachability.swift` |
| Checkpoints, accounts, the Terminal opener | `ConductorKit/Checkpoints.swift`, `Terminal.swift` |
| `storage.json`, `storage set` arguments, AWS profiles, `db status` | `ConductorKit/Storage/` |
| Every `conductor` command line the app runs | `ConductorKit/ConductorCommands.swift` |

The app owns three launchd user agents, each a plain plist in `~/Library/LaunchAgents`:

| Label | Runs |
|---|---|
| `dev.conductor.postgres` | the bundled `postgres -D ~/Library/Application Support/Conductor/pg`, on a Unix socket only, in a folder only you can open |
| `dev.conductor.daemon` | `conductord --addr 127.0.0.1:<port> --dsn <socket DSN>` |
| `dev.conductor.db-backup` | `conductor db base-backup` every `base_backup_every_hours`, only while Settings → Storage sends the database to a bucket |

`launchctl print gui/$(id -u)/dev.conductor.daemon` shows one; their logs are in
`~/Library/Logs/Conductor`.

## Build

You need the Xcode Command Line Tools (Swift 5.9 or later) and Go (the version in
`go.mod`). Xcode itself is not needed.

```sh
macos/build.sh                   # macos/build/Conductor.app: arm64 + x86_64, signed ad hoc
macos/build.sh --arch arm64      # this Mac's architecture only: quicker
macos/build.sh --open            # and launch it
macos/build.sh --no-postgres     # without the PostgreSQL bundle (attach to a database instead)
macos/build.sh --go-only         # only the Go commands, cross-compiled; works on Linux
```

`build.sh` cross-compiles `conductor`, `conductord` and `conductor-mcp` for both
architectures and joins them with `lipo` into `Contents/Resources/bin`; runs one
`swift build --triple <arch>-apple-macosx13.0` per architecture and joins those; copies
Sparkle's framework with `ditto`; puts PostgreSQL in `Contents/Resources/postgres`; and
signs (see below).

PostgreSQL comes from `fetch-postgres.sh`, which downloads EnterpriseDB's universal
`postgresql-17.11-1-osx-binaries.zip` (437 MB), checks its pinned sha256, keeps only the
server, the tools the app and `conductor db` run, `lib/` and `share/` (pgAdmin, StackBuilder
and headers go), and leaves the result in `macos/build/postgres` for the next build. To
move to a newer 17.x, change the URL, version and hash in that script together.

## Test

```sh
cd macos && swift test
```

runs on macOS and on Linux (Swift 6.1). The app target is declared only on a Mac, so on
Linux this builds and tests ConductorKit alone. Two kinds of test are off by default:

- `CONDUCTOR_TEST_KEYCHAIN=1` writes a shared item to your login keychain and reads it
  back through `/usr/bin/security`, which checks the access list really spares the CLI a
  prompt. Off by default because a CI keychain may be locked.
- `IntegrationTests` runs the real Postgres and conductord without launchd: initdb with
  the supervisor's arguments, the socket-only configuration, conductord on the socket DSN,
  bootstrap, local sign-in, a `quota.exhausted` event through the SSE client, and an
  invite link `conductor join` accepts.

  ```sh
  make build                                   # bin/conductor, bin/conductord
  CONDUCTOR_IT_PG_BIN=/opt/homebrew/opt/postgresql@17/bin CONDUCTOR_IT_BIN=$PWD/bin \
    swift test --package-path macos --filter IntegrationTests
  ```

  Add `CONDUCTOR_IT_RUN_AS=postgres` when running as root, which Postgres refuses. CI runs
  it against the bundle `build.sh` made.

`macos/packaging/test-signing.sh` tests the signing rules below without a Mac.

CI: `.github/workflows/macos-app.yml` runs the Linux job (`swift test` in the `swift:6.1`
container, and the packaging tests) and the macOS job (`swift build`, `swift test`,
`build.sh`, checks of the bundle, the integration test, and `Conductor.app.zip` as an
artifact).

## Package

```sh
macos/build.sh --version 1.2.3
macos/packaging/build-dmg.sh --app macos/build/Conductor.app --version 1.2.3 --out dist
```

gives `dist/Conductor-1.2.3-macos-universal.dmg`, holding `Install Conductor.pkg`. The
package installs `/Applications/Conductor.app` and links `conductor`, `conductord` and
`conductor-mcp` from inside it into `/usr/local/bin`, so a terminal and the coding tools'
MCP configurations run the binaries the app runs. `build-pkg.sh` makes the package alone.

## Sign and notarize

Nothing is signed with a Developer ID unless you say so, and every step that would be
says when it is skipped. A release takes all of these or none:

| Variable | What |
|---|---|
| `DEVELOPER_ID_APPLICATION` | `Developer ID Application: NAME (TEAMID)`: the app, everything in it, the image |
| `DEVELOPER_ID_INSTALLER` | `Developer ID Installer: NAME (TEAMID)`: the package |
| `NOTARY_PROFILE` | a profile saved with `xcrun notarytool store-credentials` |
| or `NOTARY_KEY`, `NOTARY_KEY_ID`, `NOTARY_ISSUER` | an App Store Connect API key (.p8), its id, its issuer |
| `SPARKLE_PUBLIC_ED_KEY` | optional: turns on updates (below) |

`build-dmg.sh` refuses a partial set before building anything: a signed package that was
never notarized is blocked by Gatekeeper like an unsigned one, and looks done. With the
full set it signs every library and executable inside the app innermost first (Postgres,
the Go commands, Sparkle's helpers) and then the app, all with the hardened runtime and a
timestamp; signs the package; notarizes and staples the package; builds the image; signs,
notarizes and staples it. `notarize.sh` reads the status out of `notarytool`'s JSON,
because `notarytool submit --wait` exits 0 when Apple rejects a file, and prints Apple's log
when the answer is not `Accepted`.

The app runs under the hardened runtime without the App Sandbox: it spawns processes and
writes `~/Library/LaunchAgents`. Its one entitlement is Apple Events, for opening Terminal
to continue a checkpoint. Postgres runs with `jit = off`, which the hardened runtime needs.

## Updates

Sparkle 2.10.0, pinned exactly in `Package.swift`. The app starts it only when its
Info.plist carries `SUPublicEDKey`, which `build-dmg.sh` writes from
`SPARKLE_PUBLIC_ED_KEY`; a build without it never checks and says why. To publish an
update, sign the image with Sparkle's `sign_update` (in
`.build/artifacts/sparkle/Sparkle/bin` after a build) using the private half of that key,
and publish `appcast.xml` at the `SUFeedURL` in Info.plist. Losing the private key strands
every installed copy: keep it somewhere that outlives any one machine.

## Settings → Storage

The pane edits `storage.json` through the CLI (`conductor storage show --json`, then
`conductor storage set …` with every setting on screen as `--name=value`). An access key's
secret, and the seal passphrase, go into the login Keychain as generic passwords
(`dev.conductor.s3` / the access key ID, and `dev.conductor.seal` / `default`) whose access
list trusts this app and `/usr/bin/security`, through which the CLI reads them; Save then
passes `--secret-from=keychain`, so the CLI proves it can read the item. They are never
written to disk, to UserDefaults, or onto a command line. AWS profiles are read from
`~/.aws/config` and `~/.aws/credentials` (or `AWS_CONFIG_FILE` /
`AWS_SHARED_CREDENTIALS_FILE`), and an SSO profile gets a button that runs
`aws sso login --profile NAME`.

With the database going to the bucket, the supervisor runs
`conductor db archiving --data-dir <pg> --write` before Postgres starts and installs the
base-backup agent; on a new Mac whose bucket holds base backups, the first run offers
Restore from bucket (`conductor db restore --data-dir <pg> --backup latest`) and waits for
the restored cluster to finish replaying before conductord starts.
