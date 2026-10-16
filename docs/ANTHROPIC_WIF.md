# Claude API workload federation in Conductor

Conductor can launch Claude Code with an OIDC identity token from **Google
Cloud**, **AWS STS**, a rotating token file, or a **local laptop issuer**.
Anthropic verifies that assertion and issues a short-lived, workspace-scoped
Claude API credential. The locally minted assertion is signed by the laptop
issuer, not by Anthropic. This outbound workload path is separate from OIDC
SSO for people signing in to Conductor.

The machine running `conductor wif exec` must have the platform identity:
a Google service account attached to a Cloud Run/GCE/GKE workload, an AWS IAM
role with outbound federation enabled, or an OIDC token file that its issuer
keeps fresh. With `--provider local`, Conductor instead keeps a persistent
P-256 signing key on the laptop and mints an ES256 assertion itself. This is
an explicit local trust root, not a way to eliminate all persistent credentials.

## Register the workload in Claude Console

Open **Settings → Workload identity → Connect workload**. The wizard creates
the Anthropic service account, issuer, and federation rule. Use a distinct rule
and an exact workload identity for each trust boundary.

| Source | Issuer | Rule match | Token source |
| --- | --- | --- | --- |
| Google Cloud | `https://accounts.google.com`, discovery | `aud=https://api.anthropic.com`, exact Google service-account numeric `sub` and `email` | Metadata server with `format=full` |
| AWS STS | Your AWS account's `https://<uuid>.tokens.sts.global.api.aws`, discovery | `aud=https://api.anthropic.com`, exact IAM role ARN as `sub` | Regional STS `GetWebIdentityToken` |
| File | The token's exact `iss` and its reachable discovery/JWKS, or inline JWKS | Exact `aud` and `sub` from the issuer's token | A file updated by that issuer |
| Local laptop | A distinct HTTPS issuer identifier with **inline JWKS** | `aud=https://api.anthropic.com` and one exact laptop `sub` | Conductor signs a fresh five-minute assertion with its local P-256 key |

Choose `workspace:developer` for Claude Code. Save the rule ID
(`fdrl_...`), organization UUID, Anthropic service-account ID (`svac_...`),
and workspace ID (`wrkspc_...`). The service account must belong to that
workspace. The workspace ID is required when the rule spans multiple
workspaces.

On AWS, enable outbound federation once with
`aws iam enable-outbound-web-identity-federation`, grant the workload role
`sts:GetWebIdentityToken`, and read its issuer URL with
`aws iam get-outbound-web-identity-federation-info`. Conductor uses the
installed AWS CLI and a regional STS endpoint. On GCP, attach a dedicated
user-managed service account; find its immutable numeric ID with
`gcloud iam service-accounts describe SA_EMAIL --format='value(uniqueId)'`.

### Local laptop issuer

Initialize a signing key on the laptop. This command refuses to replace an
existing key and prints a **public** JWKS document; the private key is saved
at `~/.conductor/wif/local-issuer.pem` with mode `0600`:

```sh
conductor wif local init
conductor wif local jwks
conductor wif local jwks | jq '.keys'
```

In Claude Console, register a **separate** issuer for the laptop. Choose an
HTTPS issuer identifier, for example `https://oidc.YOUR-DOMAIN.example/laptop`,
and set **JWKS source → Inline**. Paste the public `keys` array from the command
output (the third command prints just that array, which the Console field
expects). Anthropic does not contact that URL in inline mode; it compares it
byte-for-byte with the assertion's `iss`. Create a dedicated service account
and federation rule with exact subject such as `conductor:laptop:YOUR-MACHINE`,
exact audience `https://api.anthropic.com`, one workspace, and
`workspace:developer` scope for Claude Code. Keep single-use `jti` checking
enabled. The issuer URL and subject in the launch command must exactly match
this rule. Do not reuse the GitHub Actions issuer for laptop tokens.

```sh
conductor wif exec \
  --provider local \
  --issuer https://oidc.YOUR-DOMAIN.example/laptop \
  --subject conductor:laptop:YOUR-MACHINE \
  --rule-id fdrl_YOUR_RULE \
  --organization-id YOUR_ORGANIZATION_UUID \
  --service-account-id svac_YOUR_SERVICE_ACCOUNT \
  --workspace-id wrkspc_YOUR_WORKSPACE \
  -- conductor wrap claude
```

Use `--key-file PATH` on `init`, `jwks`, and `exec` if you need a separate key
per laptop. Register each public key under the issuer that will validate it.
The `init` output contains public material only; never paste the PEM file.
Use a distinct key, issuer and rule per device so one device can be revoked
without disabling the others.

## Launch Claude Code

For a Google Cloud runner:

```sh
conductor wif exec \
  --provider gcp \
  --rule-id fdrl_YOUR_RULE \
  --organization-id YOUR_ORGANIZATION_UUID \
  --service-account-id svac_YOUR_SERVICE_ACCOUNT \
  --workspace-id wrkspc_YOUR_WORKSPACE \
  -- conductor wrap claude
```

For AWS, replace `--provider gcp` with
`--provider aws --aws-region us-east-1`. For a projected or otherwise
rotating token file, use `--provider file --identity-token-file /absolute/path/to/token`.
The file must contain an OIDC JWT for `https://api.anthropic.com`; Conductor
checks its basic claims, and Anthropic verifies its signature and federation
rule. Replace `conductor wrap claude` with `claude` when Conductor session
coordination is not needed.

For GCP, AWS and local signing, Conductor obtains a fresh assertion at startup
and once per minute, placing it in a private `0600` temporary file. Claude Code reads
that file for token exchange. A failed refresh removes the old file so the
next exchange fails closed; recovery occurs on the next refresh. The file is
removed when the child exits. For `--provider file`, Conductor passes the
upstream file path directly so the issuer's rotation remains authoritative.

The wrapper removes `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, an explicit
`ANTHROPIC_PROFILE`, and cloud-provider routing variables from the child
environment because they could override federation. It does not store a
provider token in task cards, Git, or the Conductor MCP configuration.

For dispatched runs, add this to `.conductor/project.yaml` (using your IDs):

```yaml
harnesses:
  claude:
    enabled: true
    command: conductor
    arg_template:
      - wif
      - exec
      - --provider
      - gcp
      - --rule-id
      - fdrl_YOUR_RULE
      - --organization-id
      - YOUR_ORGANIZATION_UUID
      - --service-account-id
      - svac_YOUR_SERVICE_ACCOUNT
      - --workspace-id
      - wrkspc_YOUR_WORKSPACE
      - --
      - claude
      - -p
      - --output-format
      - stream-json
      - --verbose
      - --mcp-config
      - "{mcp_config}"
```

Use `aws` and insert `--aws-region, us-east-1`, or use `file` and insert
`--identity-token-file, /absolute/path/to/token`, for those providers. The
runner must have the provider identity and the new Conductor binary installed.

## Verify and operate

Trigger an actual Claude request while the Console wizard's connection test
is listening. Confirm the request is attributed to the intended Anthropic
service account and workspace. The local preflight only checks JWT structure,
audience, and lifetime; it does not prove Anthropic accepts the issuer, key, or
rule. Anthropic's authentication history shows the reason for a rejected
exchange.

AWS STS tokens contain a `jti` and Anthropic accepts each such assertion only
once. Conductor refreshes its AWS token file every minute, so normal SDK
refreshes receive new assertions. A rapid retry that rereads the same file can
still receive a replay rejection. Use Anthropic's callable token-provider API
for a workload that needs guaranteed fresh AWS assertions on every exchange.

The local issuer also generates a cryptographically random `jti` for each new
assertion, so the same rapid-retry limit applies to its token file. Keep replay
protection enabled and inspect Console authentication history for `jti_reused`.
The local PEM file is a long-lived signing credential. File mode `0600`
prevents other OS users from reading it, but a process running as the laptop
owner can still read or use it. Never put it in Git, task metadata, prompts,
backups shared with other users, or an exposed Conductor HTTP endpoint. If it
is lost or compromised, archive the laptop rule or issuer, create a new key,
and register its new JWKS before resuming. A future Secure Enclave signer can
replace the PEM backend without changing the Console rule contract; it is not
part of this CLI implementation. This local issuer does not migrate hosted
`conductord` Delphi or inference paths that run on another machine.

References: [Anthropic WIF setup](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation),
[GCP guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/gcp),
[AWS guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/aws),
[WIF reference](https://platform.claude.com/docs/en/manage-claude/wif-reference).
