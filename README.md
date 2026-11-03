# Strata

Serverless version control on Amazon S3, built for large, regulated organizations.
Code and docs are versioned bottom-up by teams; every change carries structured
work-tracking metadata (Jira keys, other trackers, labels, attributes) that can be
searched across all repositories and synced to external systems.

Product vision: [PR/FAQ](https://claude.ai/artifact/8UnmdjQ2HzodM9keAQnKoi).
Design notes: [docs/architecture.md](docs/architecture.md).

## What works today (MVP)

| Area | Status |
| --- | --- |
| Content-addressed storage on S3 (SHA-256, verified by S3 on upload, write-once) | done |
| Changes with stable change IDs, amend, first-parent history | done |
| Refs with compare-and-swap (concurrent pushes never corrupt history) | done |
| Work-tracking metadata: work items, labels, attributes; auto-link `PAY-42` from messages | done |
| Annotations: add tickets or labels after the fact, without rewriting history | done |
| Cross-repo search by work item or label | done |
| Jira sync: remote link on the issue for every linked change (EventBridge → Lambda) | done |
| `strata` CLI: init, clone, status, push, log, show, annotate, find | done |
| Web console: repos, history, change detail, file browser, annotate, search | done |
| Layers: org → business unit → team → repo cascade with locked, override and append files | done |
| Audit ledger: hash-chained record of every write, archived under S3 Object Lock, `strata audit --verify`; events published from the ledger (never lost) | done |
| Ledger anchoring: every 15 minutes a KMS-signed, RFC 3161-timestamped Merkle root of all chain heads, chained to the previous anchor; reconciler checks the Object Lock archive record by record; `strata audit --verify --anchor-key` catches a rewritten history | done |
| Policy-as-code permissions: Cedar policies versioned in a Strata repo, groups from Cognito, Layer-aware (`resource in Repo::"payments-rules"`) | done |
| Four-eyes approvals and protected refs: approvals bound to a revision, authors never count, ref-move facts in the Cedar context, approvers recorded in the ledger | done |
| Break-glass: no standing admin access; time-boxed grants (ticket, scope, max 4h, fresh MFA), enforced outside Cedar so no policy can lock everyone out, every use tagged in the ledger, reviewed by another admin, alerted, overdue reviews alarmed | done |
| `strata pull`: three-way merge of your edits with the server's latest (line-level for text, conflict markers, push blocked until resolved) | done |
| Exit: `strata export` writes a repository as plain Git plus an evidence sidecar (sealed changes, trees, ledger, anchors, annotations, approvals); `strata export verify` and an independent Python verifier prove it complete and unaltered, offline ([format](docs/export-format.md)) | done |
| Disaster recovery: warm standby in eu-central-1 (global table, two-way object replication, its own audit archive), one writer fenced in every write transaction, `strata-dr switchover` drilled live (about 20 s of no writes, no data loss) and `promote --force` with chain repair; Cognito user pool replicated with automatic sign-in failover (passkeys as MFA), drilled | done |
| Branch merges in the web console, wave rollouts, Git bridge | next |

## Layout

```
backend/   Go: Lambdas (cmd/api, cmd/tracker), CLI (cmd/strata), internal packages
infra/     AWS CDK (TypeScript): stack "Strata", plus "StrataSiteCert" and "StrataSite" for the public site
web/       React + Vite + TypeScript web console
site/      public showcase site (plain HTML/CSS, no build step), https://strata.baschen.is
scripts/   helper scripts
```

The showcase site describes only what the build status in the PR/FAQ marks as done or
partial; everything else is labelled roadmap. Update it when a feature lands.
`just site-deploy` (Windows: `just win::site-deploy`) publishes it; preview with
`python -m http.server 8790 -d site`.

## Develop

Needs Go 1.27, Node 24, pnpm 10 (`corepack enable`), just, and the AWS CLI.

```sh
just install
just test        # Go vet/tests, TS typecheck, web + CDK tests
just deploy      # needs AWS credentials ($AWS_PROFILE, default "admin")
```

If a deploy fails with "Unable to resolve AWS account", the SSO session expired
(the AWS CLI may still work from cached credentials): run `aws sso login --profile admin`.

On Windows use `just win::<task>`.

The Go tests include an end-to-end test that runs the real CLI against the real API
handler with an in-memory store (`backend/internal/cli/e2e_test.go`).

## Use

After `just deploy`:

1. Create a user in the Cognito user pool (self sign-up is off), then sign in to the
   web console (`WebUrl` output). The first sign-in sets up an authenticator app (MFA is
   required); then click **Add passkey**, sign in with the passkey, and have an admin remove
   the TOTP so the user can also sign in from the standby region.
2. Click **CLI token** in the console header and export it:

```sh
export STRATA_API=<ApiUrl output>
export STRATA_TOKEN=<token>
strata repo create payments -d "Payments service"
strata init --repo payments
strata push -m "PAY-42 round half-even" --label risk:low --attr approver=ana
strata annotate --work-item SEC-9 --note "covered by SEC-9 review"
strata pull                                       # someone pushed first? merge their work into yours
strata find --work-item PAY-42
```

### Layers

Push rules, CI templates, prompts and agent skills from the top down:

```sh
strata repo create org-rules --layer               # in the platform team's folder
strata init --repo org-rules
echo '{"rules":[{"path":"ci/**","mode":"locked"},{"path":"AGENTS.md","mode":"append"}]}' > strata.layer.json
strata push -m "OPS-1 org baseline"

strata repo create cards-api --extends org-rules    # any team
strata clone cards-api && cd cards-api              # inherited files are written here
echo "- cards-api: PAN tokens only" >> AGENTS.md    # your lines are kept on every pull
strata push -m "CARD-1 skeleton"                    # stores only your contribution
strata layers show                                  # chain, modes and sources
strata layers fleet org-rules                       # who inherits from org-rules
```

See [docs/architecture.md](docs/architecture.md#layers-top-down-configuration).

### Break-glass

Once policies exist, `strata-admins` have no standing access. In an emergency:

```sh
strata breakglass start --reason "payments down" --work-item INC-42 --repo payments --for 1h
strata push -m "INC-42 hotfix"
strata breakglass end
# later, another admin:
strata breakglass list --open
strata breakglass review bg-... --outcome accepted --note "diff matches INC-42"
```

See [docs/architecture.md](docs/architecture.md#break-glass-time-boxed-and-reviewed).

### Anchors

`strata audit --verify` also checks the signed anchors that covered the repository.
Pin the anchor public keys yourself, never from the API. Anchors are signed with the
multi-Region key `alias/strata-anchor-mr` (`AnchorKeyArn` output); older ones with the first
key (`PreviousAnchorKeyArn`). Put both in one file:

```sh
for k in <AnchorKeyArn> <PreviousAnchorKeyArn>; do
  aws kms get-public-key --key-id "$k" --query PublicKey --output text | base64 -d | openssl pkey -pubin -inform DER
done > anchor.pem
strata audit --verify --anchor-key anchor.pem --tsa-ca tsa-root.pem
```

Each workspace remembers the anchors it verified (`.strata/anchors.json`), so proofs
deleted or replaced on the server later fail verification. `strata breakglass audit
--verify` does the same for the break-glass ledger.

See [docs/architecture.md](docs/architecture.md#anchoring-making-the-chain-external-evidence).

### Export (exit)

```sh
strata export --repo payments --out payments-export     # add --break-glass for that ledger too
strata export verify payments-export --anchor-key anchor.pem
python3 tools/verify_export.py payments-export --anchor-key anchor.pem   # no Strata code needed
git clone payments-export/git payments
```

The export is plain Git (every revision, amended-away ones included, under
`refs/strata/revisions/`) plus the evidence in `strata/`. Both verifiers use the
ledger as the list of what must be there, so a missing revision, approval or ref is
caught, not only a changed one. Exports are reproducible: the same repository gives
the same Git object names. See [docs/export-format.md](docs/export-format.md).

### Disaster recovery

```sh
just dr-status                         # writer, chains and objects in both regions
cd backend && go run ./cmd/strata-dr switchover --to eu-central-1 --reason "quarterly drill" --work-item OPS-7
cd backend && go run ./cmd/strata-dr switchover --to eu-west-1 --reason "failback" --work-item OPS-7
just dr-drill                          # the whole drill as a test, against the deployed stacks
```

Point clients at `https://api.strata.baschen.is`: it follows the writer region (Route 53 failover on
each region's `GET /health`) within a few minutes of a switchover. Meanwhile the old writer answers
421 with the writer's regional URL, which the CLI follows only if it is in `STRATA_PEER_APIS`. `strata-dr` uses your AWS credentials, not the Strata API. If eu-west-1 is down,
`strata-dr promote --to eu-central-1 --force --reason ... --work-item ...` takes the writer
and repairs partly replicated chains; what was lost is recorded in the ledger. Deploying a
standby from scratch: `StrataStandbyBase`, then its outputs into `dr.standby` in
`infra/cdk.json`, `Strata` (once with `-c drDataReplication=false` to add the table replica
alone), `StrataStandby`, `just dr-backfill`, `strata-dr init`. See
[docs/architecture.md](docs/architecture.md#disaster-recovery-a-warm-standby-with-one-writer).

Sign-in fails over on its own: the Cognito user pool has a replica in eu-central-1, and a
Route 53 health check switches the login domain to it. Replicas do not support TOTP, so every
user needs a passkey (console header: **Add passkey**) and, once it works, an admin removes
their TOTP. See [docs/architecture.md](docs/architecture.md#disaster-recovery-a-warm-standby-with-one-writer).

### Jira

Store a secret `{"baseUrl":"https://your-org.atlassian.net","email":"bot@your-org.com","apiToken":"..."}`
in Secrets Manager and deploy with `-c jiraSecretArn=<arn>`. Each change linked to a
Jira key then shows up as a remote link on that issue. Links are idempotent, so
retries never duplicate them; failures retry for 24 hours, then land in a DLQ.

Any other system can subscribe to the event bus (`ChangeCreated`, `ChangeAnnotated`,
`RefUpdated`, `RepoCreated`) with an EventBridge rule.

## Local development without AWS

```sh
just dev-api   # API on http://localhost:8788, in-memory store, token = user email
echo '{"apiUrl":"http://localhost:8788","authority":"","clientId":"","authDomain":"","devUser":"you@example.com"}' > web/public/config.json
just dev       # web console on http://localhost:5173
STRATA_API=http://localhost:8788 STRATA_TOKEN=you@example.com strata repo list
```
