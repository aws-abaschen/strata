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
| Merges, wave rollouts, policy-as-code, Git bridge | next |

## Layout

```
backend/   Go: Lambdas (cmd/api, cmd/tracker), CLI (cmd/strata), internal packages
infra/     AWS CDK (TypeScript): stack "Strata"
web/       React + Vite + TypeScript web console
scripts/   helper scripts
```

## Develop

Needs Go 1.26, Node 24, pnpm 10 (`corepack enable`), just, and the AWS CLI.

```sh
just install
just test        # Go vet/tests, TS typecheck, web + CDK tests
just deploy      # needs AWS credentials ($AWS_PROFILE, default "admin")
```

On Windows use `just win::<task>`.

The Go tests include an end-to-end test that runs the real CLI against the real API
handler with an in-memory store (`backend/internal/cli/e2e_test.go`).

## Use

After `just deploy`:

1. Create a user in the Cognito user pool (self sign-up is off), then sign in to the
   web console (`WebUrl` output).
2. Click **CLI token** in the console header and export it:

```sh
export STRATA_API=<ApiUrl output>
export STRATA_TOKEN=<token>
strata repo create payments -d "Payments service"
strata init --repo payments
strata push -m "PAY-42 round half-even" --label risk:low --attr approver=ana
strata annotate --work-item SEC-9 --note "covered by SEC-9 review"
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
