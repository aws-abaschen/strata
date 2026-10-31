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

## Known gaps and next steps

- **Audit of reads**: only writes are in the ledger. Reads can be logged with API
  Gateway access logs and S3 data events in CloudTrail.
- **Closure check** walks the whole tree on every push. Next: skip subtrees already
  verified in the parent change.
- **Large files**: whole-file objects up to 5 GB. Next: content-defined chunking and
  multipart uploads.
- **Merges**: pushes are fast-forward only. Next: three-way merge Lambda and
  first-class conflicts.
- **Layers**: automatic wave rollouts (today: point canary repos at a `canary` ref, then move `main`), fleet drift view (which Layer revision each repo last pulled), Layer-level deny rules.
- **Authorization**: any signed-in user can read and write any repository. Next:
  Amazon Verified Permissions (Cedar) with repo and path-level policies.
- **Git bridge**, **semantic search**, **KMS per-object keys**, **Object Lock retention**.
