# Claude API workload federation in Conductor

Conductor can launch Claude Code with an OIDC identity token from **Google
Cloud**, **AWS STS**, or a rotating token file provided by another issuer.
Anthropic exchanges that token for a short-lived, workspace-scoped Claude API
credential. Conductor does not hold an Anthropic API key or an OIDC signing key.
This outbound workload path is separate from OIDC SSO for people signing in to
Conductor.

The machine running `conductor wif exec` must have the platform identity:
a Google service account attached to a Cloud Run/GCE/GKE workload, an AWS IAM
role with outbound federation enabled, or an OIDC token file that its issuer
keeps fresh. A local machine without one of these identities cannot mint a
workload assertion simply by running Conductor.

## Register the workload in Claude Console

Open **Settings → Workload identity → Connect workload**. The wizard creates
the Anthropic service account, issuer, and federation rule. Use a distinct rule
and an exact workload identity for each trust boundary.

| Source | Issuer | Rule match | Token source |
| --- | --- | --- | --- |
| Google Cloud | `https://accounts.google.com`, discovery | `aud=https://api.anthropic.com`, exact Google service-account numeric `sub` and `email` | Metadata server with `format=full` |
| AWS STS | Your AWS account's `https://<uuid>.tokens.sts.global.api.aws`, discovery | `aud=https://api.anthropic.com`, exact IAM role ARN as `sub` | Regional STS `GetWebIdentityToken` |
| File | The token's exact `iss` and its reachable discovery/JWKS, or inline JWKS | Exact `aud` and `sub` from the issuer's token | A file updated by that issuer |

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

For GCP and AWS, Conductor obtains a fresh provider token at startup and once
per minute, placing it in a private `0600` temporary file. Claude Code reads
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

References: [Anthropic WIF setup](https://platform.claude.com/docs/en/manage-claude/workload-identity-federation),
[GCP guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/gcp),
[AWS guide](https://platform.claude.com/docs/en/manage-claude/wif-providers/aws),
[WIF reference](https://platform.claude.com/docs/en/manage-claude/wif-reference).
