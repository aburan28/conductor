# Usage limits: knowing a login is about to run out

People drive coding agents through subscription logins — Claude Pro/Max in Claude Code, a
ChatGPT plan in Codex, a Cursor plan in Cursor and `cursor-agent` — and every one of those
logins has a rolling usage limit. When it lands, the session stops mid-task. Conductor
already makes a session portable (`conductor checkpoint resume --account / --harness`, see
[PORTABILITY.md](PORTABILITY.md)); this document is about seeing the limit coming early
enough to use that.

It has two halves: what each tool exposes (research, with sources, as of October 2026), and
what Conductor does with it.

## 1. What each tool exposes

Every source is classified as one of:

- **documented** — the vendor documents the field or file and its meaning;
- **observable-local-file** — a file the tool writes on the user's machine whose shape is
  visible in the open-source code or stable in practice, but which is not a documented
  interface;
- **undocumented-endpoint** — a vendor web endpoint used by the vendor's own dashboard and
  by community tools, reachable only with the user's own session credential;
- **none** — nothing local says how much is left.

### Claude Code (Claude Pro / Max)

**Limit model.** Every plan has a usage limit that resets on a rolling **five-hour session
window**; paid plans add **weekly limits** on top, and both apply at once. Claude Code and
claude.ai draw on the same allowance. Max 5x and Max 20x scale the per-session allowance.
([Max plan](https://support.claude.com/en/articles/11049741-what-is-the-max-plan),
[pricing](https://claude.com/pricing))

**Signals.**

| Signal | Class | What it carries |
|---|---|---|
| Status line command stdin: `rate_limits.five_hour` / `rate_limits.seven_day` (`used_percentage` 0–100, `resets_at` Unix seconds); `rate_limits.spend_limit` behind a Claude apps gateway (`used_percentage`, `resets_at`, optionally `used_usd`, `limit_usd`, `period`) | **documented** ([status line](https://code.claude.com/docs/en/statusline#available-data)) | exactly the two windows the plan enforces. Present only for Pro/Max subscribers (or behind a gateway with a spend limit), only after the first API response, each window independently absent, and dropped once `resets_at` passes. The status line re-runs on every assistant message and when a window's `resets_at` arrives. |
| Transcript record for a hit limit: an `assistant` line with `isApiErrorMessage: true`, `error: "rate_limit"`, `apiErrorStatus: 429`, model `<synthetic>`, and text such as `You've hit your weekly limit · resets Jun 3 at 4pm (Europe/Berlin)`; older versions wrote `Claude AI usage limit reached\|<unix seconds>` | **observable-local-file** ([claude-code#68816](https://github.com/anthropics/claude-code/issues/68816), [deja-vu#4758](https://github.com/vshulcz/deja-vu/issues/4758)) | only the fact that a limit *was* hit, the window, and the reset time — no percentage. |
| `/usage` in the TUI | documented, interactive only | not machine-readable. |
| `anthropic-ratelimit-unified-*` response headers | response headers | visible only to something sitting in the HTTP path (a proxy); not observable locally without intercepting traffic. |
| OAuth usage endpoint used by community menu-bar tools | **undocumented-endpoint** | the same percentages as the status line, but needs the OAuth access token from Claude Code's credential store. |

### Codex CLI (ChatGPT plans)

**Limit model.** Codex usage on a ChatGPT plan is metered against a **primary** and a
**secondary** window. On Plus (and Standard Business) the primary is a **five-hour** window
and the secondary is **weekly**; OpenAI removed and then restored the five-hour window for
Plus in August 2026, and Pro plans currently have only the weekly window — which Codex then
reports as `primary`. Model-specific buckets can appear alongside the default one.
([Codex pricing](https://developers.openai.com/codex/pricing),
[9to5Mac, Aug 2026](https://9to5mac.com/2026/08/24/openai-restores-5-hour-codex-and-work-limits-for-chatgpt-plus-users/),
[openai/codex#43136](https://github.com/openai/codex/issues/43136))

**Signals.**

| Signal | Class | What it carries |
|---|---|---|
| `event_msg` / `token_count` records in `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl` with a `rate_limits` object: `limit_id`, `limit_name`, `primary` / `secondary` windows (`used_percent` 0–100, `window_minutes`, `resets_at` Unix seconds), `credits`, `plan_type`, `rate_limit_reached_type` | **observable-local-file**; the shape is the `RateLimitSnapshot` / `RateLimitWindow` types in the open-source protocol crate ([protocol.rs](https://raw.githubusercontent.com/openai/codex/main/codex-rs/protocol/src/protocol.rs)) | both windows, after every response. Older builds wrote `resets_in_seconds` (relative to the event) instead of `resets_at` ([hatada.jp](https://tech.hatada.jp/en/posts/reading-codex-rate-limits-without-burning-them)). Pitfalls seen in the wild: the slot (`primary`/`secondary`) does not say which window it is — `window_minutes` does (300 = 5h, 10080 = weekly); `secondary` may be `null`; `resets_at` is an integer; the newest file is not necessarily the newest reading ([loom#8963](https://github.com/rjwalters/loom/issues/8963), [openusage#371](https://github.com/janekbaraniewski/openusage/issues/371)). `used_percent` has 1% resolution. |
| `codex app-server` JSON-RPC `account/rateLimits/read` | experimental, documented via `codex app-server generate-json-schema` | the same snapshot on demand, but means spawning an app-server; not needed while the rollouts carry it. |
| `/status` in the TUI | interactive only | not machine-readable. |

### Cursor (editor and `cursor-agent` CLI)

**Limit model.** Since mid-2025 a Cursor plan includes a **monthly dollar amount** of
frontier-model usage at API prices (Pro $20, Pro+ about $70, Ultra about $400), resetting
with the billing cycle; Auto mode is not drawn from that pool; on-demand usage beyond it is
off by default. ([Cursor pricing clarification](https://cursor.com/blog/june-2025-pricing),
[Vantage](https://www.vantage.sh/blog/cursor-pricing-explained))

**Signals.**

| Signal | Class | What it carries |
|---|---|---|
| Dashboard usage page; `/usage` in `cursor-agent` | interactive only | included-usage meters, on-demand spend, reset date. |
| `GET https://cursor.com/api/usage-summary` with the `WorkosCursorSessionToken` cookie | **undocumented-endpoint** ([reverse-engineered notes](https://gist.github.com/dmwyatt/1e9359b1862e7cbfe1e754fe4c8db764), [caam#109](https://github.com/Dicklesworthstone/coding_agent_account_manager/issues/109)) | `billingCycleStart/End`, `membershipType`, `individualUsage.plan.{used,limit,remaining,totalPercentUsed,apiPercentUsed,autoPercentUsed}`, `individualUsage.onDemand`. Community tools obtain the cookie by reading Cursor's `state.vscdb` or `~/.config/cursor/auth.json`, i.e. a live credential. |

### OpenCode

**Limit model.** Depends on the provider the login is for. OpenCode's own **Go** plan meters
a monthly dollar amount with a **five-hour** cap (20% of the month), a **weekly** cap (50%)
and the **monthly** cap; other providers (GitHub Copilot, an API key) carry their own
limits. ([OpenCode Go](https://opencode.ai/docs/go/))

**Signals.** `opencode stats [--json]` reports local token and cost history (which
`internal/usage` already reads through `opencode export`), but nothing local states the
remaining allowance. Class: **none**.

### Gemini CLI

**Limit model.** Per-user **daily request** quotas plus a per-minute rate: 1,000/day for a
personal Google login (Code Assist individual), 1,500 for Google AI Pro, 2,000 for Ultra, 250
for an unpaid API key. ([quotas and pricing](https://geminicli.com/docs/resources/quota-and-pricing/))
Note the same page says the unpaid tier was replaced by Antigravity CLI in June 2026.

**Signals.** `/stats model` in the TUI shows session usage and the applicable limits;
nothing machine-readable reports what is left of the day. Class: **none** (interactive only).

### GitHub Copilot (including Copilot CLI)

**Limit model.** A **monthly premium-request allowance** per plan, resetting on the 1st of
the month at 00:00 UTC, with paid overage behind a spending limit (default $0).
([premium requests](https://docs.github.com/en/billing/concepts/product-billing/github-copilot-premium-requests),
[individual billing](https://docs.github.com/en/copilot/concepts/billing-and-usage/individuals/billing))

**Signals.** The IDE status icon (interactive), downloadable usage reports, and the REST
endpoint `GET /users/{username}/settings/billing/premium_request/usage`, which needs a
token with the "Plan" read permission and reports consumption rather than the remaining
allowance ([billing usage API](https://docs.github.com/en/rest/billing/usage)). Class:
**documented**, but remote and credentialed; nothing local.

## 2. What Conductor does with each

| Tool | Source used | Class | Default |
|---|---|---|---|
| Claude Code | status line shim (`conductor quota statusline`) | documented | on once installed (`conductor quota statusline install`) |
| Claude Code | "limit reached · resets …" transcript records | observable-local-file | on — fallback when no status line reading exists; marks the window exhausted until its reset |
| Codex | rollout `token_count.rate_limits` | observable-local-file | on |
| Cursor | `cursor.com/api/usage-summary` | undocumented-endpoint | **off**; opt-in with a cookie you supply (below) |
| OpenCode, Gemini CLI, Copilot | none locally | — | `conductor quota report` lets a script of yours feed numbers in (`manual`) |

Not used, deliberately:

- **Claude's OAuth usage endpoint.** It returns nothing the documented status line does not,
  and calling it means reading Claude Code's OAuth token out of its credential store.
  Conductor never reads another tool's credentials.
- **`anthropic-ratelimit-unified-*` headers.** Only a proxy sees them.
- **Harvesting Cursor's session cookie** from `state.vscdb` or `auth.json`. The Cursor
  collector runs only when you hand it the cookie yourself.

Every collector degrades to "unknown": a missing directory, a file mid-write, a format
change, a network error or a timeout yields no reading, never an error that reaches the
session. Collectors in the wrap sidecar run on their own goroutine with a recover.

### The Claude Code status line shim

```
conductor quota statusline install [--config-dir ~/.claude-work] [--dry-run]
conductor quota statusline uninstall
```

`install` points `statusLine.command` in that config directory's `settings.json` at
`conductor quota statusline`, carrying your existing command along (base64, in the command
line itself) so the shim can run it with the same stdin and print exactly what it printed.
Nothing visible changes. Padding and `refreshInterval` are left as they were. `uninstall`
restores the original command. If you had no status line, the shim prints nothing unless
you pass `--show`, which prints a short `5h 23% · wk 41%` segment.

The shim reads only `rate_limits` from the payload — not the transcript path, not the
working directory, not the session name — and writes one small file per window under
`~/.conductor/quota/`.

### Cursor (opt-in, undocumented)

```
export CONDUCTOR_QUOTA_CURSOR_COOKIE='<WorkosCursorSessionToken value from cursor.com>'
# or put it in ~/.conductor/quota/cursor-cookie (mode 0600)
```

With either present, `conductor quota` and the wrap sidecar ask
`cursor.com/api/usage-summary` at most once every five minutes, with a five-second timeout,
and record the plan's `totalPercentUsed` against the billing cycle, under the login label
`CONDUCTOR_QUOTA_CURSOR_ACCOUNT` (default `default`). Every Cursor reading is
labelled `undocumented` in the CLI, the API and the dashboard. If the endpoint moves or
changes shape the collector reports nothing and says why in `conductor doctor`. Tests use
a local fixture server; nothing in the test suite calls Cursor.

### Manual readings

```
conductor quota report --harness gemini --window daily --used 640 --limit 1000 --resets-at 2026-10-06T07:00:00Z
conductor quota report --harness copilot --window monthly --used-percent 72
```

## 3. The model

One **snapshot** is one window of one login of one tool on one machine:

| Field | Meaning |
|---|---|
| `harness` | `claude`, `codex`, `cursor`, `opencode`, `gemini`, `copilot`, … |
| `account` | a label for the login, never the login itself (below) |
| `machine` | the host name the wrap sessions already report |
| `window` | `5h`, `weekly`, `monthly`, `daily`, `spend`, or `<limit id>:<window>` for a model-specific bucket |
| `window_minutes` | the window's length when known |
| `used_percent` | 0–100 (may exceed 100 for a spend limit) |
| `used`, `limit`, `unit` | when the source gives absolutes (Cursor cents, a manual count) |
| `resets_at` | when the window resets, when known |
| `limit_reached` | the source said the limit is hit, whatever the percentage |
| `plan` | the plan name when the source reports one (`pro`, `plus`, …) |
| `source`, `source_kind` | which collector, and documented / local_file / undocumented / manual |
| `observed_at` | when the reading was taken |

**Account labels.** The login is named by its state directory, the same convention
`checkpoint resume --account` uses: `~/.claude` is `default`, `~/.claude-work` is `work`,
`~/.conductor/accounts/claude/work` is `work`. Any other directory becomes `dir-` plus the
first ten hex digits of a SHA-256 of its path, so a path with a user name in it never leaves
the machine. No email, token, organization id or prompt text is read for this, and none is
sent.

### Levels and thresholds

| Level | When |
|---|---|
| `ok` | below the warning threshold |
| `warning` | `used_percent` ≥ warn (default 80) |
| `critical` | `used_percent` ≥ critical (default 95) |
| `exhausted` | `used_percent` ≥ 100, or the source reports the limit hit |

A reading whose `resets_at` has passed is treated as reset (0%), whatever it said.

Thresholds, highest precedence first: `CONDUCTOR_QUOTA_WARN` / `CONDUCTOR_QUOTA_CRITICAL`;
`~/.conductor/quota.yaml`; the repository's `.conductor/project.yaml`; the defaults.

```yaml
# .conductor/project.yaml or ~/.conductor/quota.yaml
quota:
  warnPercent: 80
  criticalPercent: 95
```

### Warnings, once per window per level

When a login crosses a level, Conductor raises it **once for that window**: a later reading
in the same window does not repeat it; a new window (its `resets_at` more than ten minutes
after the one alerted, or the alerted window's reset has passed, or — with no reset time —
one window length since the alert) re-arms it. Crossing straight to a higher level raises
only the highest and marks the lower ones done. The marks are persisted twice, because two
things act on them:

- **The control plane** (`quota_alerts`, migration 0009) appends `quota.warning`
  (severity `warning` or `critical`) or `quota.exhausted` to the project's event stream.
- **The machine** (`~/.conductor/quota/alerts.json`, under a file lock) shows a desktop
  notification (`osascript` on macOS, `notify-send` on Linux with a display; silent
  otherwise; `CONDUCTOR_QUOTA_NOTIFY=off` to disable) and, at `critical` or `exhausted`,
  has the wrap sidecar capture a checkpoint immediately and print the command that would
  continue the session on the login or tool with the most headroom:

  ```
  Conductor: claude login "default" is at 96% of its 5h window (resets 15:40).
    Checkpoint 9a474a saved. Continue on claude "work" (12% used):
      conductor checkpoint resume 9a474a --account work
  ```

  Headroom is 100 minus the highest window of a login. The same tool on another account is
  preferred (the conversation resumes natively) when it is below the warning threshold;
  otherwise the tool and account with the most headroom wins, resumed from the checkpoint's
  continuation. The default account is addressed with `--state-dir`, since `--account
  default` would name a different directory.

### Who sees what

| Viewer | Sees |
|---|---|
| the login's owner | every snapshot of every login and machine they reported: `GET /v1/quota`, `conductor quota`, `coord_quota`, the dashboard card |
| other project members | `GET /v1/projects/{p}/quota`: counts only — logins reported in the last 24 hours, how many are near the limit (≥ warning), how many are exhausted. No person, account, machine or window. |
| the event stream | `quota.warning` / `quota.exhausted` carry `harness`, window (`kind`), `severity`, a rounded `percent_hint`, and the reset time (`expires_at`). No actor, no account, no machine. The aggregate id is the alert's own id. |

This is the same line the usage ledger draws: numbers and labels travel, the login does not,
and anything that would let a teammate read one person's state stays with that person. A
project of one does reveal its member's state through the counts; that is unavoidable for
any count and is the reason the counts are all there is.

## 4. Where it shows up

- `conductor quota` — a table of tools × accounts × windows: used %, window, resets in,
  source, freshness, level. Reads this machine's collectors, reports them, and merges the
  owner's other machines from the control plane. `--json`, `--local` (no network),
  `--watch`.
- `conductor quota suggest` — the resume command with the most headroom, for the latest
  checkpoint.
- `conductor status` — a "Usage limits" line for your own logins at or above warning, and the
  team count.
- `conductor doctor` — which collectors found data here, how fresh it is, and whether the
  status line shim is installed.
- `conductor wrap` — the sidecar collects every minute (`CONDUCTOR_QUOTA_INTERVAL`), reports,
  notifies, and checkpoints as above. `CONDUCTOR_QUOTA=off` disables all of it.
- `conductor usage sync` — also reports this machine's quota snapshots.
- MCP `coord_quota` — read-only: the agent's own logins, levels, headroom, and the resume
  command, so it can checkpoint (`coord_checkpoint`) before the limit lands.
- Dashboard — a small "Usage limits" card on the Usage view.
