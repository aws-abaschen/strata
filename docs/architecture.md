# Architecture

Strata has no servers. Every component is a managed AWS service, so an idle
repository costs only its storage.

```
 strata CLI / web console / agents
          │  HTTPS + JWT (Cognito)
          ▼
 API Gateway (HTTP API) ──► Lambda "api" (Go)
          │                      │
          ▼                      ├──────────────► S3 "Data": objects, sealed changes
   presigned URLs                │
   (direct upload/download)      └──────────────► DynamoDB "Index": repos, refs, changes,
                                                  work-item index, audit chain
                                                         │ stream (audit records)
                                                         ▼
                                                  Lambda "ledger" ──► S3 "Audit" (Object Lock)
                                                         │
                                                         ▼
                                                  EventBridge bus (ChangeCreated, ...)
                                                         │
                                          ┌──────────────┴──────────────┐
                                          ▼                             ▼
                                  Lambda "tracker"               your rules
                                  (Jira links)                   (SIEM, Slack...)
```

## Data model

| Concept | Where | Notes |
| --- | --- | --- |
| Object (file or tree) | S3 `repos/<repo>/objects/<h[0:2]>/<h>` | `h` = SHA-256 of the bytes. Uploaded by the client with a presigned URL that signs `x-amz-checksum-sha256` and `If-None-Match: *`, so S3 rejects wrong content and overwrites. |
| Tree | S3, as an object | Canonical JSON `{"entries":[{name,kind,hash,size}]}`, sorted by name. Kinds: `file`, `exec`, `dir`. |
| Change | S3 `repos/<repo>/changes/<rev>.json` + DynamoDB `REV#<rev>` | Like a Git commit. `rev` = SHA-256 of the canonical change JSON. Author and time are set by the server from the verified JWT. |
| Change ID | DynamoDB `CHG#<id>` → latest revision | Stable across amends (`c` + 26 base32 chars). Amend = new revision, same ID, `predecessor` = old revision; CAS on the pointer. |
| Ref | DynamoDB `REF#<name>` | Moved with compare-and-swap in the same transaction as the change. Default expectation = first parent (fast-forward only). |
| Work item / label index | DynamoDB `WI#<system>:<key>`, `LBL#<label>` | Sort key starts with the change time, so queries return newest first across every repository. |
| Annotation | DynamoDB `NOTE#<rev>#...` | Append-only metadata added later; indexed like the change's own metadata. |

## Work tracking

A change's `metadata` holds:

- `workItems`: `{system, key, url?}`. `system` defaults to `jira`; Jira keys found in the
  message (`PAY-42`) are linked automatically. Any tracker works: `servicenow:CHG001`,
  `azure-boards:1234`, `github:org/repo#12`.
- `labels`: lowercase tags such as `risk:low` or `release/2026.11`.
- `attributes`: free key/value pairs such as `approver=ana`, `cost-centre=4711`.

Limits per change: 20 work items, 20 labels, 50 attributes (keeps the index write in
one DynamoDB transaction).

Events go to EventBridge. The `tracker` Lambda adds a Jira remote link with
`globalId = strata:<repo>:<rev>`, so a retry updates the link instead of adding a
second one.

## Layers (top-down configuration)

A Layer is a repository of kind `layer`. Any repository can extend one Layer at a
ref (`payments-rules@main`), and Layers extend Layers, so a chain mirrors the
organization: `org-rules → payments-rules → cards-api`. Chains are at most 8 deep and
cycles are refused.

A Layer's optional `strata.layer.json` sets the mode of its files (first matching
rule wins; `*` matches within a path segment, `**` across segments):

```json
{
  "default": "override",
  "rules": [
    { "path": "ci/**", "mode": "locked" },
    { "path": ".claude/skills/**", "mode": "locked" },
    { "path": "AGENTS.md", "mode": "append" }
  ]
}
```

| Mode | Lower levels (Layers below, then the repo) |
| --- | --- |
| `locked` | cannot change it. The API rejects any push whose tree contains the path (`403`); a lower Layer's copy is ignored and reported as a violation. |
| `override` | may replace it. The lowest level that has the file wins, and may set a new mode for the levels below it. |
| `append` | may add to it. Contributions are concatenated top first, separated by a newline. |

Resolution (`internal/layers`) reads each Layer at its ref from S3, top first, then
applies the repository's own files as the last level. The repository stores only its
own contribution (its override, or its appended lines), never a copy of inherited
content, so a Layer change reaches every subscriber on its next pull.

- `GET /repos/{repo}/layers?ref=main&content=true`: the effective files with modes,
  sources and rendered text. This is the runtime endpoint for tools and AI agents.
- `GET /repos/{layer}/subscribers`: the fleet, every repo inheriting directly or
  through other Layers (subscriber index `PK=SUBS#<layer>`).
- `PUT /repos/{repo}/settings {"extends": {"layer","ref"} | null}`: compare-and-swap
  on the previous Layer; returns files the new Layer would lock.
- CLI: `strata layers pull` writes the merged files into the working copy (locked ones
  read-only) and records inherited content in `.strata/layers.json`; `push` uploads
  only the repo's contribution and refuses edits to locked or inherited append lines.

Rollouts in waves work with refs today: point canary repos at `org-rules@canary`,
move `canary` first, then `main`.

## Consistency

1. The client uploads objects (S3 verifies each hash).
2. `POST /changes` checks every object the tree references exists (closure check),
   writes the sealed change to S3, then one DynamoDB transaction writes the revision,
   moves the change-ID pointer and the ref (both CAS), and adds index entries.
3. A lost race returns `409`; nothing partial is visible.

## Pull and merge

Every change is recorded by a push, so a working copy never holds unpushed changes
of its own, only uncommitted edits. `strata pull` therefore works like a rebase of those
edits onto the server:

1. Snapshot the working copy (yours), read HEAD's tree (base) and the ref's current
   tree on the server (theirs).
2. Per file: changed on one side only → that side wins (including adds and deletes);
   changed the same way → kept; changed differently → text files merge line by line
   (diff3), clashing regions get `<<<<<<< yours` / `>>>>>>> server <rev>` markers;
   binary files keep yours and write `<file>.theirs`.
3. HEAD moves to the server's revision; unresolved files go to
   `.strata/conflicts.json`, and `push` refuses until no markers or `.theirs` copies
   remain. The next push is a plain fast-forward, so history stays linear.

For Layer-managed files the merge works on the repo's own contribution, then the
inherited content is re-applied.

## Audit ledger and events

Every write (repo created, settings changed, ref moved, change created, annotation)
stores an audit record **in the same DynamoDB transaction** as the write, so there is
never a write without its record, nor a record without its write.

- Records form one hash chain per repository: `PK=AUDIT#<repo>`, `SK=<seq>`, each with
  `prev` (the previous record's hash) and `hash` (SHA-256 of the canonical record).
  `REPO#<repo>/AUDITHEAD` holds the last seq and hash, moved with compare-and-swap;
  two concurrent writers retry until each gets its own seq.
- Altering, deleting or reordering any record breaks every hash after it.
  `strata audit --verify` checks the whole chain on the client, so the check does not
  rely on the server; `GET /repos/{repo}/audit` returns pages with a server check too.
- The table streams new audit records to the **ledger** Lambda, which writes each one
  to the audit bucket (`audit/<repo>/<seq>.json`, write-once, default Object Lock
  retention: governance mode, 365 days; `-c auditLockMode=compliance` for production)
  and publishes it to EventBridge with the action as `detail-type`. The ledger is the
  event outbox: events cannot be lost or sent for a write that did not commit.
  Delivery is at least once (retries, bisect on error, DLQ after 20 attempts).

Event `detail` = the write's payload (for `ChangeCreated`: repo, revision, change id,
subject, author, ref, previous target, metadata, link) plus
`audit: {repo, seq, hash, actor, at}`.

### Anchoring: making the chain external evidence

A hash chain alone proves nothing against whoever can write the table: they can
rewrite a chain's tail and recompute every hash after it, and `--verify` still passes.
The **anchor** Lambda (`cmd/anchor`, package `internal/anchor`) closes that gap. It runs
every 15 minutes, one run at a time (reserved concurrency 1). Each run does three things.

1. **Rewrite check.** Every head (repo, seq, hash) the previous anchor committed to
   must still be in the table with the same hash. Chains link each record to the one
   before, so this covers every older record too. A difference is `AnchorRewrites`,
   which raises an alarm. The job then **stops anchoring** (an archive mismatch does the
   same): it never signs over tampering, the last good anchor stays the baseline, and
   every run reports the problem again until a person investigates.
2. **Completeness reconciler.** For each chain, up to the previous anchor's head (at
   least one run old, so the ledger Lambda has archived it), compare every Object Lock
   archive record with the table record by hash. A missing record is
   `LedgerArchiveGaps` and a different one is `LedgerArchiveMismatches`; both raise
   alarms. Progress is kept per chain and stops at the first problem, so the problem is
   reported again every run until someone fixes it.
3. **Anchor.** If any head moved, or 24 hours have passed (heartbeat), the run builds a
   Merkle tree (RFC 9162 hashing, domain-separated leaves and nodes) over every chain's
   head, `_breakglass` included. It then signs a statement `{v, seq, prev, root, leaves,
   createdAt}`, where `prev` is the previous statement's digest:
   - the signature is ECDSA P-256 by the KMS key `alias/strata-anchor`; the private key
     never leaves KMS;
   - the run asks an RFC 3161 timestamp authority (`TSA_URL`) for a token over the same
     digest;
   - the anchor goes to `ANCHOR/<seq>` with a compare-and-swap on `ANCHOR/LATEST`, so
     anchors never fork, and to the audit bucket (`anchors/<seq>.json`, Object Lock);
   - each chain whose head moved gets a proof (`ANCHORED#<repo>/<seq>`): its leaf, the
     sibling hashes and the signed statement. No other repository's name is in it.

   A timestamp authority outage does not stop anchoring: the anchor is stored without a
   token and `AnchorTimestampFailures` raises an alarm. A missing successful run for an
   hour raises `AnchorStalledAlarm`.

`strata audit --verify` (and `strata breakglass audit --verify` for the `_breakglass`
chain) then checks every anchor that ever covered the chain (`GET
/repos/{repo}/audit/anchors`, `GET /break-glass/ledger/anchors`, paged):
- the Merkle proof;
- that the anchored record is the one the reader holds;
- the signature, against a public key the reader pinned out of band (`--anchor-key` or
  `STRATA_ANCHOR_KEY`; get it with `aws kms get-public-key`, never from the API);
- the timestamp token, chained to pinned roots (`--tsa-ca` or `STRATA_TSA_CA`);
- the **witness file**: every anchor this machine verified before is remembered in
  `.strata/anchors.json` (or `--witness FILE`). The proofs live in the same table as
  the ledger, so someone who can rewrite the ledger can also delete or replace the
  proofs that cover it. A remembered anchor that is no longer served, or has changed,
  fails verification.

It says plainly what it could not check when nothing is pinned or remembered. A reader
with no history is protected only by the anchors as they are served now. The originals
stay in the Object Lock bucket (`anchors/<seq>.json`), so an auditor can compare them,
until anchors are copied to a separate account.

**Limits of this prototype.**
- The key, the table and the bucket are in one account, so its administrators can still
  sign and can lift a governance-mode lock. Production moves the anchor key and a copy of
  the anchors to a separate security (log-archive) account, uses compliance mode, and
  publishes the key fingerprint.
- FreeTSA, the default, is not a qualified eIDAS authority; set `-c tsaUrl=` to a
  qualified one in production.
- One anchor item holds every head, which limits it to a few thousand repositories.

### Export: leaving with the evidence

DORA Art. 28(8) asks for an exit strategy that works without the provider. `strata
export` writes a repository as a plain Git repository plus a sidecar of evidence, and
`strata export verify` checks it offline. [docs/export-format.md](export-format.md)
is the specification.

- **Client-side, read-only.** The CLI builds the export from the public API, so it
  needs only read access and the same code works against any deployment. Nothing new
  runs on the server.
- **The ledger is the list.** Every write is in the repository's ledger, superseded
  revisions included. The verifier replays it (`ChangeCreated`, `RefUpdated`,
  `ChangeAnnotated`, `ApprovalGranted`, Layer settings) and requires the export to
  hold exactly that, both ways. A dropped revision is caught as surely as an edited
  one. The anchors, checked with a pinned key, make the ledger itself trustworthy up
  to the last anchor.
- **Hash the bytes, don't re-serialize.** Changes and trees are stored as the exact
  bytes their SHA-256 names, so another language can verify them without copying Go's
  JSON encoder. Only the ledger digest needs a defined encoding, given in the spec.
- **Git is a view.** Commits, trees and blobs are written directly as loose objects
  (no git binary, no library). That makes exports reproducible and lets the verifier
  read them without Git. SHA-1 names are addresses; integrity comes from SHA-256.
  Every revision keeps a ref under `refs/strata/revisions/` so `git gc` never drops
  amended-away history.
- **Two verifiers.** The Go one shares Strata's code. `tools/verify_export.py` uses
  only Python's standard library (openssl for signatures) and was written from the
  spec. The test suite runs both on every build, against a clean export and each
  kind of tampering it knows of, so the spec cannot drift from the code without a failing test.
- **Limits.** Layer cascades are not flattened: each repository exports its own tree,
  and Layers export separately. `.git` entries and refs Git rejects stay in the
  sidecar only. The Python verifier leaves RFC 3161 timestamps to `openssl ts`.
  Each file is held in memory while it is exported, and files over 1 GiB are not
  exported yet. Anchors cut from the end of an export cannot be detected offline;
  verifiers report how many records the last anchor does not cover.

## Permissions (policy-as-code)

Every request is authorized with [Cedar](https://www.cedarpolicy.com/), evaluated
in the API Lambda with the official `cedar-go` library (no extra service, no extra
latency). Policies are `.cedar` files in a Strata repository, `strata-policies` by
default (`-c policyRepo=...`), so they are versioned, reviewed, audited and linked to
tickets like any change. They apply as soon as that repo's `main` moves.

| Entity | Notes |
| --- | --- |
| `User::"<email>"` | from the verified JWT; parents: `Group::"<g>"` for each `cognito:groups` value |
| `Repo::"<name>"` | attributes `name`, `kind`; parent: the Layer it extends, so `resource in Repo::"payments-rules"` matches every repo inheriting from it |
| `Action::"read" \| "write" \| "annotate" \| "admin" \| "create" \| "approve" \| "audit"` | read: clone, log, browse, layers, audit · write: push, move refs · annotate: link work items, labels, notes · admin: settings · create: new repo · approve: sign off a change · audit: see break-glass grants (on `Repo::"_breakglass"`) |

- A repo the caller may not read answers `404`, and is left out of listings and
  work-item or label searches. Other denials answer `403` with the deciding policy ids
  (`<file>#<n>`).
- Fail-closed bootstrap: until `strata-policies` has a change on `main`, only members
  of `strata-admins` can do anything; everyone else gets `404`/`403`, and `GET /me`
  reports `"authorization": "not-configured"`. An admin creates `strata-policies` and
  pushes policies first. Open bootstrap exists only as an explicit `OpenBootstrap`
  field set by the dev server and tests, never read from the Lambda environment.
  If loading the policies fails, requests are denied, never allowed.
- `main` of the policy repository cannot be deleted (that would reopen bootstrap).
- No standing admin access: once policies exist, `Group::"strata-admins"` (created in
  Cognito by the stack) gets only what policies grant, like anyone. Emergency access is
  a time-boxed [break-glass grant](#break-glass-time-boxed-and-reviewed). Membership of
  `strata-admins` is managed at AWS level
  (`aws cognito-idp admin-add-user-to-group ... --group-name strata-admins`, IAM-gated
  and recorded in CloudTrail).
- A push (or ref move) that would install policies that do not parse is rejected, so a
  typo cannot lock everyone out.
- Reading inherited Layer files needs `read` on the Layer repositories.

Example: [policies/strata.cedar](policies/strata.cedar).

### Four-eyes approvals and protected refs

The server computes facts about every write that moves a ref and passes them to Cedar
as `context`; policy decides. There is no separate protected-branch setting to secure:
a `forbid` in `strata-policies` guards `main` and, because forbids bind
`strata-admins`, the admins too.

| `context.` | Meaning |
| --- | --- |
| `ref` | the ref being moved (`""` when the request moves none) |
| `fastForward` | the new target descends from the current one (first-parent walk, at most 100 changes; deletes and rollbacks are `false`) |
| `approvals` | distinct approvers of that exact revision who authored **no** change in the range from the current target to the new one |
| `hasWorkItem` | the change, with its annotations, links a work item |

- **Approvals** are `POST /repos/{repo}/changes/{rev}/approvals` (Cedar action
  `approve`), stored as `APPR#<rev>#<approver>` in the same transaction as an
  `ApprovalGranted` audit record. They bind to the revision hash, so amending a change
  starts again at zero.
- **Separation of duties is code, not policy:** the author of a change cannot approve it,
  administrators included, and an author of any change in the range does not count
  (A writes c1, B stacks c2, A's approval of c2 is ignored).
- **Flow:** `strata push --review NAME` (to `review/NAME`) → colleague `strata approve REV`
  → `strata promote REV --ref main`. A new change has no approvals, so a policy that
  demands them cannot be met by a direct push.
- **Evidence:** the `RefUpdated` record in the Object-Lock ledger lists the approvers of
  the promotion.
- **Fails closed:** if a policy errors while evaluating (Cedar silently skips an erroring
  `forbid`), the request is denied and the decision names the policy.

Example: [policies/four-eyes.cedar](policies/four-eyes.cedar). Mind that protecting the
policy repository this way means a second person is needed to change policies.

### Break-glass: time-boxed and reviewed

Administrators have no standing access. In an emergency one activates a grant; another
administrator reviews it afterwards. DORA Art. 9 (privileged access) and the usual
emergency-change control: the path always works, and every use leaves evidence.

```sh
strata breakglass start --reason "payments down, hotfix" --work-item INC-42 --repo payments --for 1h
strata push -m "INC-42 hotfix"        # allowed although policy (e.g. four-eyes) denies it
strata breakglass end                 # or let it expire
strata breakglass show bg-...         # another admin sees everything done under it
strata breakglass review bg-... --outcome accepted --note "diff matches INC-42"
```

- **Outside Cedar, on purpose.** Activation needs `strata-admins` membership from the
  token, checked in code. While a grant is active, a request on a listed repository that
  Cedar denies is allowed in code, forbids included. So no policy, however broken or
  hostile, can remove the emergency path: a policy push that forbids everything is fixed
  under a grant (tested).
- **Bounded:** a reason, an incident or emergency-change ticket and an explicit list of
  repositories (`*` allowed, flagged in the record) are required. Default 1 hour, at
  most 4. One active grant per person. Expiry is checked on the server clock on every
  request; leaving `strata-admins` ends its effect at once. In AWS, activating needs a
  sign-in (with MFA) in the last 15 minutes, read from the token's `auth_time`.
- **Evidence:** `BreakGlassActivated`, `BreakGlassEnded` and `BreakGlassReviewed` are
  records of a dedicated hash chain, `_breakglass`, archived under Object Lock like
  every ledger (`strata breakglass audit --verify`). Each write let through by a grant
  carries `"breakGlass": "<grant id>"` in its own repository's record and is indexed
  under the grant in the same transaction, so a review lists exactly what was done.
- **Review:** by another member of `strata-admins`, never the holder, once the grant is
  over: `accepted` or `rejected` with a note. A rejection is recorded and alerted; it
  does not roll anything back.
- **Alerts:** every activation, early end and review goes to the alarm topic. An hourly
  sweeper Lambda publishes `Strata/BreakGlassOverdueReviews` (grants unreviewed 24 hours
  after they ended) and `Strata/BreakGlassActive`; an alarm fires on any overdue review.
- **Who sees grants:** `strata-admins`, and whoever policy allows `audit` on
  `Repo::"_breakglass"` (for example `Group::"auditors"`).
- **Bootstrap** is unchanged: until `strata-policies` has a `main`, `strata-admins` may
  do everything, so they can install the first policies.

## Disaster recovery: a warm standby with one writer

DORA Art. 11–12 asks for ICT continuity tested with drills, and backups that
can be restored somewhere else. Strata keeps a warm standby in `eu-central-1`.

**What is replicated, and how**

| Store | Primary (`Strata`, eu-west-1) | Standby (`StrataStandby` + `StrataStandbyBase`, eu-central-1) |
| --- | --- | --- |
| Index table | DynamoDB global table, asynchronous, item by item, last writer wins | its replica, with its own stream |
| Data objects and sealed changes | S3 replication to the standby | S3 replication back to the primary |
| Audit archive (Object Lock) | built by this region's ledger from its stream | built by the standby's ledger from its own stream |
| Anchors | signed by the writer region | signed by the writer region, with the primary's KMS key |
| Sign-in (Cognito) | primary user pool, multi-Region KMS key | replica of the same pool (same ID and issuer), activated, served automatically when the primary fails |

Each region archives every audit record but publishes only the records it
wrote (an unhashed `region` attribute on each audit item), so integrations get
each event once. Objects are content-addressed, so replicating them both ways
never conflicts.

**One writer, fenced.** Global tables accept writes in every region, and two
regions writing at once would fork every audit chain: last writer wins would
keep one of two records with the same seq. So exactly one region writes. The
item `SYSTEM/WRITER {region, epoch}` names it, and every audited write checks
it with a `ConditionCheck` in its own transaction, as does every upload URL.
A region that is not the writer answers 421 with the writer's API URL. The CLI
follows it once, but only to a URL listed in `STRATA_PEER_APIS`: the URL comes
from a table item and the request carries the user's token. Reads work in both
regions.

**Changing the writer** (`strata-dr`, with AWS credentials, never the Strata API,
whose sign-in lives in the failed region):

- `switchover` (planned: drills, failback). First set the writer to none
  (draining), so writes stop in every region at once. Then wait until the
  target holds that writer item, every chain's head and records with the same
  hashes, and every object. Only then name the target the writer, in the
  target, with compare-and-swap on the drained epoch. Naming the target at the
  start would let it write as soon as that one item replicated, at heads still
  behind: replication has no order across items. No data is lost. A switchover
  stopped while draining resumes when run again.
- `promote --force` (the writer region is down). Try to demote it, take the
  writer locally, then repair chains that hold part of a transaction:
  - a head whose records never arrived: the chain continues from the last
    record that arrived, and the lost seqs are recorded;
  - records whose head update never arrived: the head moves up to them;
  - records off the chain (a gap below them): moved to `ORPHAN#<repo>`, kept as
    evidence, so they cannot block new writes.

  Each repaired chain gets a `ChainRepairedAfterPromotion` record saying what
  this region saw, so any loss is in the ledger, not hidden. The chains stay
  contiguous and verifiable.

Every writer change is recorded on the `strata-dr` chain with the IAM principal,
reason, work item and how long writes were unavailable. That chain is anchored
like any other, so drills are evidence.

**Sign-in fails over by itself.** The user pool has a replica in
eu-central-1 with the same pool ID, encrypted with a replica of the same
multi-Region key. Tokens carry Cognito's updated issuer
(`https://issuer-cognito-idp.eu-west-1.amazonaws.com/<pool>`), whose signing
keys Cognito serves from several Regions, so both Regions' APIs and the web
console validate the same tokens. A Route 53 health check on the primary's
Cognito endpoint drives the login domain: while it fails, the same domain
signs users in from the replica. Moving the Strata writer stays a separate,
human decision (`strata-dr`): automatic promotion during a partition is how
two writers happen.

Replicas do not support TOTP, so sign-in uses passkeys with user
verification, which count as MFA. Onboarding: an admin creates the user, the
first sign-in with the temporary password sets up TOTP (required MFA), the
user registers a passkey from the console's **Add passkey** link and signs in
with it, then an admin removes TOTP
(`aws cognito-idp admin-set-user-mfa-preference --software-token-mfa-settings Enabled=false,PreferredMfa=false`).
Until then that user can sign in only while the primary is up.

Drilled live on 2026-11-03: with the health check inverted, the login domain
served a passkey sign-in from eu-central-1 (CloudTrail there shows
`login_passkey_POST` then `Token_POST`; nothing in eu-west-1). To repeat:
`aws route53 update-health-check --health-check-id <CognitoHealthCheckId> --inverted`,
sign in, then `--no-inverted`.

**Detection.** An audit item is written once. A MODIFY or REMOVE of one on either
stream means two regions wrote the same seq, or tampering, and raises
`LedgerForkAlarm`. After a forced promotion, the old region's reconciler also
reports the records its archive has and the table lost.

**Drilled live** (`just dr-drill`, 2026-11-01): eu-west-1 to eu-central-1 and back,
with writes in each phase, an anchor signed in eu-central-1, and an export verified
against the pinned key that holds changes written in both regions. With the
draining handover, writes were unavailable for 20.0 s each way; no data was lost.
An earlier run, before draining, also took 20 s (54.8 s once, waiting for fresh
objects to replicate). The first run stopped at a real bug, the standby's IAM
grants missing the table index, which is how drills earn their keep. Forced promotion and chain
repair are tested against simulated partial replication only: they cannot be
drilled without losing a region.

**Limits**

- **Sign-in replicates, account changes do not.** The Cognito replica signs
  users in, but new users, password resets and profile changes need the
  primary. TOTP does not work in the replica, so every user must have a
  passkey and no TOTP (see below). A federated IdP would also need to outlive
  the region.
- **No anchors while eu-west-1 is down.** The anchor key is single-region (a key
  cannot be made multi-region after creation), so the standby signs with it
  cross-region. During an outage `AnchorStalledAlarm` fires and anchoring resumes
  afterwards. Moving the key to a security account is the time to make it
  multi-region.
- **Split brain is detected, not prevented.** If the old writer is still up,
  unreachable by the operator but reachable by clients, it keeps writing until
  replication brings it the new writer item. Preventing that needs a quorum
  outside both regions (a lease in a third region, or Route 53 ARC), which ties
  write availability to that third party. The fork alarm and the reconciler
  show exactly what diverged.
- **Replication starts empty.** Neither a new replica nor a new replication rule
  copies what already exists: `just dr-backfill` copies the objects and the audit
  archive once.
- **Clients pick a region.** There is no shared API domain. The 421 follow covers
  writes; reads from the standby can lag by seconds. The web console runs in the
  primary only.
- **The fence needs its region.** Lambdas get `STRATA_REGION`; a store built
  without one (local runs, `TestDeployedStack`) writes unfenced, so run those
  only while eu-west-1 is the writer.
- **Switchover lists both buckets** on every check, every 2 seconds. That is fine
  at thousands of objects; millions need S3 replication metrics or inventories
  instead.
- **Cost.** DynamoDB writes roughly double (replicated writes), plus S3
  replication transfer and the standby's idle Lambdas and keys.

## Security and operations baseline

- **Encryption:** customer-managed KMS keys with yearly rotation, one each for the data
  bucket, the audit bucket and the index table (S3 bucket keys on, so cost stays low).
  Keys are retained if the stack is deleted. Objects written before the keys were added
  stay under S3-managed keys.
- **Identity:** Cognito, no self sign-up, MFA required (TOTP). The JWT groups claim
  feeds Cedar groups.
- **Alarms** (SNS topic `AlarmTopic`, optional email via `-c alarmEmail=...`):
  ledger more than 5 minutes behind the table stream, ledger errors, any message in the
  ledger or tracker dead-letter queue, CloudTrail events that change or delete the
  ledger function or its stream mapping (an attempt to switch the evidence off), every
  break-glass activation, end and review, break-glass reviews overdue, and the anchor
  job: rewritten anchored records, archive gaps or mismatches, missing timestamps,
  errors, a stalled job, and changes to its function.
- **Verified against real AWS:** `go test -tags aws ./internal/integration/` runs the
  CLI and API against a deployed stack (presigned uploads, transactions, ledger archive
  under Object Lock). The in-memory store accepts transaction shapes DynamoDB rejects, so
  run it after any change to `internal/store`.

## Known gaps and next steps

- **Audit of reads**: only writes are in the ledger. Reads can be logged with API
  Gateway access logs and S3 data events in CloudTrail.
- **Closure check** walks the whole tree on every push. Next: skip subtrees already
  verified in the parent change.
- **Large files**: whole-file objects up to 5 GB. Next: content-defined chunking and
  multipart uploads.
- **Merges**: `strata pull` merges on the client; histories stay linear. Next: branches
  with server-side merge changes (two parents) and conflicts recorded in history, so a
  reviewer can merge from the web console.
- **Layers**: automatic wave rollouts (today: point canary repos at a `canary` ref, then move `main`), fleet drift view (which Layer revision each repo last pulled), Layer-level deny rules.
- **Permissions**: repository-level today. Next: path-level rules (the changed paths
  of a push in the Cedar context) and a Cedar schema to validate policies.
- **Git bridge** (live, two-way; one-way export is done), **semantic search**, **KMS per-object keys**, **Object Lock retention**.
