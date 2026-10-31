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
| `Action::"read" \| "write" \| "annotate" \| "admin" \| "create"` | read: clone, log, browse, layers, audit · write: push, move refs · annotate: link work items, labels, notes · admin: settings · create: new repo |

- A repo the caller may not read answers `404`, and is left out of listings and
  work-item or label searches. Other denials answer `403` with the deciding policy ids
  (`<file>#<n>`).
- Bootstrap: until `strata-policies` has a change on `main`, every signed-in user is
  allowed. Create that repo first, with an admin.
- Break-glass: `Group::"strata-admins"` (created in Cognito by the stack) is always
  allowed unless a `forbid` matches.
- A push (or ref move) that would install policies that do not parse is rejected, so a
  typo cannot lock everyone out.
- Reading inherited Layer files needs `read` on the Layer repositories.

Example: [policies/strata.cedar](policies/strata.cedar).

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
- **Git bridge**, **semantic search**, **KMS per-object keys**, **Object Lock retention**.
