# Security policy

## Reporting a vulnerability

Please report security problems privately, through GitHub's private vulnerability reporting:
**[Report a vulnerability](https://github.com/aburan28/conductor/security/advisories/new)**
(the repository's Security tab → Advisories → "Report a vulnerability").

Do not open a public issue, pull request, or discussion for a vulnerability, and do not post
proof-of-concept code anywhere public before a fix is released.

Include what you can of:

- the affected component (`conductord`, the `conductor` CLI, `conductor-mcp`, the dashboard,
  the release artifacts) and version (`conductor version`);
- how to reproduce it, and what an attacker gains;
- any configuration it depends on (security mode, TLS, `--behind-proxy`, peering).

You should get an acknowledgement within 7 days. We will keep you informed while we work on a
fix, agree a disclosure date with you, and credit you in the advisory unless you prefer not.

## Supported versions

Conductor is pre-1.0. Security fixes go into the latest release only; there are no backports.
Upgrade to the newest release (`make install`, or the release installer in the README) to get
them. `conductor doctor` warns when your CLI and your control plane are different versions.

| Version          | Supported |
|------------------|-----------|
| latest release   | yes       |
| anything older   | no        |
| `main`           | best effort; fixes land here first |

## Scope

In scope: the code in this repository and the release artifacts built from it, including

- authentication and authorization in `conductord` (tokens, local sign-in, roles, project
  isolation, the GitHub App), and the privacy guarantees the README and `docs/DESIGN.md` make
  (no transcripts or private task details leaving a machine or reaching other members);
- the CLI, `conductor-mcp`, the runner (`conductor worker`), hooks and integrations it writes
  into other tools, checkpoints and backups;
- the dashboard served by `conductord`;
- the build and release pipeline (`.github/workflows`, `scripts/install-release.sh`).

Out of scope: vulnerabilities in the coding tools Conductor launches (Claude Code, Codex,
OpenCode, …), in Postgres, or in your own deployment's proxy or TLS setup; findings that need
an already-compromised machine or account (for example, root on the host running
`conductord`); and denial of service by volume alone against a server you have exposed without
rate limiting in front of it.

Release archives carry GitHub build-provenance attestations. Verify one with
`gh attestation verify <archive> --repo aburan28/conductor`.
