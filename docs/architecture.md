# Architecture

Strata has no servers. Every component is a managed AWS service, so an idle
repository costs only its storage.

```
 strata CLI / web console / agents
          │  HTTPS + JWT (Cognito)
          ▼
 API Gateway (HTTP API) ──► Lambda "api" (Go)
          │                      │
          │        ┌─────────────┼───────────────┐
          ▼        ▼             ▼               ▼
   presigned URLs  S3 "Data"     DynamoDB "Index"  EventBridge bus
   (direct upload/ objects +     repos, refs,      ChangeCreated, ...
    download)      sealed changes changes, index        │
                                                  ┌─────┴──────┐
                                                  ▼            ▼
                                        Lambda "tracker"   your rules
                                        (Jira links)       (SIEM, Slack...)
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

## Consistency

1. The client uploads objects (S3 verifies each hash).
2. `POST /changes` checks every object the tree references exists (closure check),
   writes the sealed change to S3, then one DynamoDB transaction writes the revision,
   moves the change-ID pointer and the ref (both CAS), and adds index entries.
3. A lost race returns `409`; nothing partial is visible.

## Known gaps and next steps

- **Events**: published after the write, best effort. Next: DynamoDB Streams →
  EventBridge Pipes for guaranteed delivery and the hash-chained audit ledger.
- **Closure check** walks the whole tree on every push. Next: skip subtrees already
  verified in the parent change.
- **Large files**: whole-file objects up to 5 GB. Next: content-defined chunking and
  multipart uploads.
- **Merges**: pushes are fast-forward only. Next: three-way merge Lambda and
  first-class conflicts.
- **Layers**: the top-down cascade from the PR/FAQ (org → business unit → team → repo).
- **Authorization**: any signed-in user can read and write any repository. Next:
  Amazon Verified Permissions (Cedar) with repo and path-level policies.
- **Git bridge**, **semantic search**, **KMS per-object keys**, **Object Lock retention**.
