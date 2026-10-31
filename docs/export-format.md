# Strata export format, version 1 (`strata-export/1`)

`strata export` writes one repository as a plain Git repository plus a sidecar of
evidence. The aim is exit (DORA Art. 28(8), Art. 30(3)(f)): a firm leaving Strata,
or an auditor working without it, can browse the history with Git alone and prove
it complete and unaltered without trusting Strata's servers or code.

This document is the specification. Two verifiers implement it:
`strata export verify DIR` (Go, shares code with Strata) and
[`tools/verify_export.py`](../tools/verify_export.py) (Python standard library only,
written from this document). The test suite runs both on every build and checks
they reject the same tampering.

```sh
strata export --repo payments --out payments-export [--break-glass]
strata export verify payments-export --anchor-key anchor.pem
python3 tools/verify_export.py payments-export --anchor-key anchor.pem
git clone payments-export/git payments
```

An export needs only read access. Each file is held in memory while it is exported,
and files over 1 GiB are not exported yet. It runs through the public API, so it works
against any deployment.

## Layout

```
DIR/
  git/                       bare Git repository (SHA-1, loose objects)
  strata/
    manifest.json            format, repo, counts, what Git could not hold
    repo.json                the repository record (name, kind, extends)
    audit.jsonl              the repository's ledger, every record from seq 1
    anchors.json             signed anchors covering the ledger (AnchorProof list)
    changes/<rev>.json       each change's sealed bytes; SHA-256 = <rev>
    trees/<hash>.json        each tree's canonical bytes; SHA-256 = <hash>
    refs.jsonl               refs as the server listed them
    annotations.jsonl        every annotation
    approvals.jsonl          every approval
    break-glass/             only with --break-glass
      audit.jsonl            the break-glass ledger
      anchors.json           its anchors
```

The Git repository is a convenience view. **The evidence is the sidecar plus the
ledger.** Git's SHA-1 names are addresses, not integrity: every check below goes back
to Strata's SHA-256 hashes, which the ledger binds and the anchors sign.

`manifest.json` is informational. A verifier uses it only for `format`, `repo`, the
list of refs Git could not hold (`omittedRefs`) and whether the break-glass ledger is
included. Everything else is derived from the ledger and the content.

## Hashes and encodings

All hashes are lowercase hex SHA-256 unless stated otherwise.

**Changes.** `changes/<rev>.json` holds the exact bytes whose SHA-256 is the revision:
the change's JSON with `"revision": ""`. Verifiers hash the file and do not
re-serialize it. Fields: `revision, changeId, repo, tree, parents, predecessor?,
author, createdAt, message, metadata`.

**Trees.** `trees/<hash>.json` holds the tree's canonical bytes,
`{"entries":[{"name","kind","hash","size"?}]}`, sorted by name. `kind` is `file`,
`exec` or `dir`. For a file, `hash` is the SHA-256 of its content and `size` its
length in bytes (omitted when 0). For a directory, `hash` names another tree.

**Ledger records** (`audit.jsonl`, one JSON object per line). A record's `hash` is the
SHA-256 of this encoding of the record with `hash` set to `""`:

- keys in this order: `repo, seq, prev, hash, at, actor, action, detail`;
- compact JSON (no spaces), UTF-8 kept as is, except that `<`, `>`, `&`, U+2028 and
  U+2029 inside strings are written `<`, `>`, `&`, ` `, ` `
  (the escaping of Go's `encoding/json`);
- `detail` encoded the same way, with its keys in the order they appear in the file;
- `at` copied as the string in the file.

`prev` is the previous record's `hash`, `""` for seq 1. Seqs start at 1 and have no
gaps. This encoding is the one thing a verifier must reproduce rather than hash
directly, because the ledger lives in a database, not as files.

**Anchors** (`anchors.json`, a list of AnchorProof). Each proof carries the
`statement` `{v, seq, prev, root, leaves, createdAt}`, its `seal` (`signature`:
base64 ASN.1 DER ECDSA P-256; `timestamp`: base64 RFC 3161 token, optional), this
repository's `leaf` `{repo, seq, hash}`, its `index` and the RFC 9162 inclusion
`proof` (sibling hashes, leaf to root).

- The statement's bytes are its compact JSON in that key order. Its digest is
  SHA-256 of those bytes. The signature is over the digest (sign-digest, not
  sign-message); `openssl pkeyutl -verify -pubin -inkey anchor.pem -in digest
  -sigfile sig` checks it. The timestamp's message imprint is the same digest.
- Leaf hash: SHA-256 of `0x00 || json(leaf)`. Node hash: SHA-256 of
  `0x01 || left || right`. Merkle tree and inclusion proofs as in RFC 9162 §2.1.
- `prev` is the digest of the previous anchor's statement, so anchors form a chain.
- The leaf's `hash` must equal the ledger record at `leaf.seq`.

The anchor key's public half is obtained out of band (a stack output, an email from
the security team), never from the export.

## Git mapping

| Strata | Git |
| --- | --- |
| file / exec / dir entry | mode `100644` / `100755` / `40000` |
| file content | blob, byte for byte |
| tree | tree; empty directories are kept as empty trees |
| change | commit; parents in Strata order |
| author, `createdAt` | `author` and `committer`: `ID <ID> UNIX +0000` (whole seconds; `<`, `>`, CR, LF, NUL in the ID become `_`) |
| message | message with trailing newlines removed, then `\n\nStrata-Revision: <rev>\nStrata-Change-Id: <id>\n` |
| every revision | `refs/strata/revisions/<rev>`, so amended-away revisions survive `git gc` |
| ref | `refs/heads/<name>`; `HEAD` points at `main`, else the first ref by name |

The export is deterministic: exporting the same repository twice gives the same Git
object names, so the `git/` directories of two exports can be compared with `diff -r`
(`manifest.json` differs: it records when and by whom).

**What Git cannot hold.** A tree entry named `.git` (in any case) is left out of the
Git tree and listed in `manifest.omitted`. It stays in the sidecar tree, so the
evidence is complete. A ref whose name Git rejects (a component starting with `.`,
ending in `.lock` or `.`) gets no branch and is listed in `manifest.omittedRefs`.

**What is not exported.** Layer cascades: each repository's own stored tree is
exported, not the files it inherits. A repository extending a Layer records that in
`repo.json` and in its ledger (`RepoCreated`, `RepoSettingsChanged`). Export the
Layer repository separately. Effective metadata (a change's metadata plus its
annotations) is not stored: it is derived from `changes/` and `annotations.jsonl`.

## Verification

A verifier passes an export only if all of these hold. The ledger is the list of what
must be there: it records every write, superseded revisions included.

1. `audit.jsonl` starts at seq 1, every record names `manifest.repo`, and the hash
   chain holds.
2. Every anchor's leaf is under its root, names this repository, matches the ledger
   record at its seq, and consecutive anchors link through `prev`. With a pinned key,
   every signature verifies and **an export without anchors fails**: otherwise a
   consistently rewritten ledger would pass by dropping them. (With a pinned
   timestamp root, the Go verifier also checks timestamps.)

   A chain gets a proof only in anchors where its head moved, so anchor seqs may skip.
   Any anchor covers every record up to its leaf through the hash chain, so dropping
   some anchors gains nothing; dropping them from the end shrinks what is covered.
   Verifiers report how many records come after the last anchored one.
3. Replay the ledger:
   - every `ChangeCreated` adds `detail.revision`; if `detail.ref` is a non-empty
     string, that ref now points at the revision;
   - `RefUpdated` sets `detail.ref.name` to `detail.ref.target`, or deletes the ref
     when `target` is empty;
   - `ChangeAnnotated` adds an annotation `(revision, record actor, note, metadata)`;
   - `ApprovalGranted` adds an approval `(revision, approver, note)`;
   - `RepoCreated` and `RepoSettingsChanged` set the Layer setting to
     `detail.settings.extends`.
4. `strata/changes/` and `refs/strata/revisions/` each hold exactly the replayed
   revisions. Each change file hashes to its name and names `manifest.repo`.
5. For each change, its Git commit (via `refs/strata/revisions/<rev>`):
   - has a tree that holds exactly the change's tree entries, recursively. Modes
     map from kinds, every blob's SHA-256 and length match the entry, and every
     Git object's SHA-1 matches its content. Omitted `.git` entries must be absent.
   - has, as parents, the commits of the change's parents, in order;
   - has the author, committer and message the mapping above gives.
6. `refs/heads/` holds exactly the replayed refs (minus `omittedRefs`), each pointing
   at its target's commit. Every name in `omittedRefs` must really be one Git rejects;
   otherwise the manifest could hide a ref.
7. `annotations.jsonl` and `approvals.jsonl` equal the replayed ones, one for one.
8. `repo.json`'s `extends` equals the replayed Layer setting.
9. If `break-glass/` is present, its ledger and anchors pass checks 1 and 2. If
   `manifest.breakGlass` is true, it must be present.

A verifier must report the first failure in words that name what is wrong (which
revision, ref or file). It exits non-zero on failure.

### What verification does not prove

- **That the ledger is the whole history.** Without anchors, a server operator could
  have rewritten the ledger before the export. Anchors signed with a key the operator
  does not control, checked against a key pinned out of band, close that gap for
  everything up to the last anchor. Writes after the last anchor rest on the hash
  chain alone, and an export could omit the latest anchors without detection;
  compare against anchors you verified earlier (`strata audit --verify` keeps them in
  `.strata/anchors.json`).
- **That Strata authorized each write.** The ledger records who did what. Whether
  policy allowed it is shown by the policy repository's own export, at the revision
  in force.
- **Timestamps** are checked by the Go verifier against a pinned root (`--tsa-ca`).
  The Python verifier leaves them to `openssl ts -verify`.
