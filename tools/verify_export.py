#!/usr/bin/env python3
"""Independent verifier for a Strata export (format strata-export/1).

Written from docs/export-format.md alone, with the Python standard library
only, so an auditor does not have to trust Strata's own code. It checks the
ledger hash chain, that every change, tree and file matches the hashes the
ledger implies, that the Git repository holds exactly that content, and the
Merkle inclusion of each anchor. Anchor signatures are checked with openssl
when --anchor-key is given; RFC 3161 timestamps are left to `openssl ts`.

usage: verify_export.py DIR [--anchor-key anchor.pem]
exit 0 = verified, 1 = verification failed, 2 = usage error.
"""
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import zlib


class Failed(Exception):
    pass


def go_str(s):
    # Go's encoding/json: UTF-8 kept as is, HTML characters escaped.
    out = json.dumps(s, ensure_ascii=False)
    for a, b in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        out = out.replace(a, b)
    return out


def go_json(v):
    if isinstance(v, dict):
        return "{" + ",".join(go_str(k) + ":" + go_json(x) for k, x in v.items()) + "}"
    if isinstance(v, list):
        return "[" + ",".join(go_json(x) for x in v) + "]"
    if isinstance(v, str):
        return go_str(v)
    return json.dumps(v)  # numbers, true, false, null


def sha256(b):
    return hashlib.sha256(b).hexdigest()


def read_json(path):
    try:
        with open(path, "rb") as f:
            return json.loads(f.read())
    except FileNotFoundError:
        raise Failed(os.path.basename(path) + " is missing")


def read_jsonl(path):
    try:
        with open(path, "rb") as f:
            return [json.loads(line) for line in f if line.strip()]
    except FileNotFoundError:
        raise Failed(os.path.basename(path) + " is missing")


def verify_chain(recs, repo=None):
    prev = ""
    for i, r in enumerate(recs):
        if r["seq"] != i + 1:
            raise Failed("ledger: expected seq %d, found %d" % (i + 1, r["seq"]))
        if repo is not None and r["repo"] != repo:
            raise Failed("ledger seq %d belongs to %r" % (r["seq"], r["repo"]))
        if r["prev"] != prev:
            raise Failed("ledger: seq %d does not link to the previous record" % r["seq"])
        body = {"repo": r["repo"], "seq": r["seq"], "prev": r["prev"], "hash": "", "at": r["at"],
                "actor": r["actor"], "action": r["action"], "detail": r["detail"]}
        if sha256(go_json(body).encode()) != r["hash"]:
            raise Failed("ledger: seq %d was altered (hash mismatch)" % r["seq"])
        prev = r["hash"]


def leaf_hash(leaf):
    b = go_json({"repo": leaf["repo"], "seq": leaf["seq"], "hash": leaf["hash"]}).encode()
    return hashlib.sha256(b"\x00" + b).digest()


def root_from_proof(h, index, size, proof):
    # RFC 9162 inclusion proof.
    if index >= size:
        raise Failed("anchor proof index out of range")
    fn, sn = index, size - 1
    for p in proof or []:  # a one-leaf tree has no siblings: null or []
        p = bytes.fromhex(p)
        if sn == 0:
            raise Failed("anchor proof too long")
        if fn % 2 == 1 or fn == sn:
            h = hashlib.sha256(b"\x01" + p + h).digest()
            while fn % 2 == 0 and fn != 0:
                fn >>= 1
                sn >>= 1
        else:
            h = hashlib.sha256(b"\x01" + h + p).digest()
        fn >>= 1
        sn >>= 1
    if sn != 0:
        raise Failed("anchor proof too short")
    return h


def statement_bytes(s):
    return go_json({"v": s["v"], "seq": s["seq"], "prev": s["prev"], "root": s["root"],
                    "leaves": s["leaves"], "createdAt": s["createdAt"]}).encode()


def verify_anchors(proofs, recs, key, name):
    # Returns (anchors checked, records after the last anchored one).
    if not proofs:
        if key and recs:
            raise Failed("%s carries no anchors: nothing ties this ledger to the pinned key" % name)
        return 0, len(recs)
    # A chain only gets a proof in anchors where its head moved, so seqs can
    # skip. Any anchor covers every record up to its leaf through the chain.
    proofs = sorted(proofs, key=lambda p: p["statement"]["seq"])
    by_seq = {r["seq"]: r["hash"] for r in recs}
    prev, prev_seq, last = None, 0, 0
    for p in proofs:
        s, leaf = p["statement"], p["leaf"]
        if s["v"] != 1:
            raise Failed("anchor %d: unknown version" % s["seq"])
        if root_from_proof(leaf_hash(leaf), p["index"], s["leaves"], p["proof"]).hex() != s["root"]:
            raise Failed("anchor %d: leaf is not under the anchored root" % s["seq"])
        if recs and leaf["repo"] != recs[0]["repo"]:
            raise Failed("anchor %d is for %s, not %s" % (s["seq"], leaf["repo"], recs[0]["repo"]))
        if by_seq.get(leaf["seq"]) != leaf["hash"]:
            raise Failed("anchor %d covers seq %d, which the ledger no longer has" % (s["seq"], leaf["seq"]))
        digest = hashlib.sha256(statement_bytes(s)).digest()
        if prev is not None and s["seq"] == prev_seq + 1 and s["prev"] != prev:
            raise Failed("anchor %d does not link to anchor %d" % (s["seq"], prev_seq))
        prev, prev_seq = digest.hex(), s["seq"]
        last = max(last, leaf["seq"])
        if key:
            check_signature(key, digest, p["seal"]["signature"], s["seq"])
    return len(proofs), len(recs) - last


def check_signature(key, digest, sig_b64, seq):
    import base64
    openssl = shutil.which("openssl")
    if not openssl:
        raise Failed("--anchor-key needs openssl on PATH")
    # The key file may hold several PEM keys: every key the chain was signed
    # with. The signature must verify under one of them.
    text = open(key).read()
    blocks = ["-----BEGIN PUBLIC KEY-----" + b.split("-----END PUBLIC KEY-----")[0] + "-----END PUBLIC KEY-----\n"
              for b in text.split("-----BEGIN PUBLIC KEY-----")[1:]]
    with tempfile.TemporaryDirectory() as d:
        dg, sg = os.path.join(d, "digest"), os.path.join(d, "sig")
        open(dg, "wb").write(digest)
        open(sg, "wb").write(base64.b64decode(sig_b64))
        for i, block in enumerate(blocks or [text]):
            kf = os.path.join(d, "key%d.pem" % i)
            open(kf, "w").write(block)
            r = subprocess.run([openssl, "pkeyutl", "-verify", "-pubin", "-inkey", kf, "-in", dg, "-sigfile", sg],
                               capture_output=True)
            if r.returncode == 0:
                return
        raise Failed("anchor %d: signature does not verify with any pinned key" % seq)


class Git:
    def __init__(self, d):
        self.d = d

    def get(self, sha, kind):
        p = os.path.join(self.d, "objects", sha[:2], sha[2:])
        try:
            data = zlib.decompress(open(p, "rb").read())
        except FileNotFoundError:
            raise Failed("git object %s is missing" % sha)
        if hashlib.sha1(data).hexdigest() != sha:
            raise Failed("git object %s does not match its name (content changed)" % sha)
        hdr, _, body = data.partition(b"\x00")
        k, _, n = hdr.decode().partition(" ")
        if k != kind or int(n) != len(body):
            raise Failed("git object %s is not a %s" % (sha, kind))
        return body

    def refs(self, prefix):
        out, root = {}, os.path.join(self.d, prefix)
        for dp, _, files in os.walk(root):
            for f in files:
                full = os.path.join(dp, f)
                name = os.path.relpath(full, root).replace(os.sep, "/")
                out[name] = open(full).read().strip()
        return out

    def tree(self, sha):
        body, out = self.get(sha, "tree"), {}
        while body:
            hdr, _, rest = body.partition(b"\x00")
            mode, _, name = hdr.decode().partition(" ")
            out[name] = (mode, rest[:20].hex())
            body = rest[20:]
        return out

    def commit(self, sha):
        hdr, _, msg = self.get(sha, "commit").decode().partition("\n\n")
        c = {"parents": [], "message": msg}
        for line in hdr.split("\n"):
            k, _, v = line.partition(" ")
            if k == "parent":
                c["parents"].append(v)
            else:
                c[k] = v
        return c


def valid_git_ref(n):
    # Same rule as the Go exporter: no component starting with "." or ending
    # in ".lock", no trailing ".", and already in clean form.
    parts = n.split("/")
    if any(p.startswith(".") or p.endswith(".lock") for p in parts) or n.endswith("."):
        return False
    return "" not in parts


MODES = {"file": "100644", "exec": "100755", "dir": "40000"}


def verify(d, key=None):
    side = os.path.join(d, "strata")
    git = Git(os.path.join(d, "git"))
    m = read_json(os.path.join(side, "manifest.json"))
    if m.get("format") != "strata-export/1":
        raise Failed("unknown format %r" % m.get("format"))
    repo = m["repo"]
    recs = read_jsonl(os.path.join(side, "audit.jsonl"))
    if not recs:
        raise Failed("the ledger is empty")
    verify_chain(recs, repo)
    anchors, unanchored = verify_anchors(read_json(os.path.join(side, "anchors.json")), recs, key, "anchors.json")

    revs, refs, anns, apps, extends = {}, {}, [], [], None
    for r in recs:
        dt, a = r["detail"], r["action"]
        if a == "ChangeCreated":
            revs.setdefault(dt["revision"], r["seq"])
            if isinstance(dt.get("ref"), str) and dt["ref"]:
                refs[dt["ref"]] = dt["revision"]
        elif a == "RefUpdated":
            if dt["ref"].get("target"):
                refs[dt["ref"]["name"]] = dt["ref"]["target"]
            else:
                refs.pop(dt["ref"]["name"], None)
        elif a == "ChangeAnnotated":
            anns.append((dt["revision"], r["actor"], dt.get("note", ""), go_json(dt.get("metadata", {}))))
        elif a == "ApprovalGranted":
            apps.append((dt["revision"], dt["approver"], dt.get("note", "")))
        elif a in ("RepoCreated", "RepoSettingsChanged") and "settings" in dt:
            extends = dt["settings"].get("extends")

    files = {f[:-5] for f in os.listdir(os.path.join(side, "changes")) if f.endswith(".json")}
    rev_refs = git.refs("refs/strata/revisions")
    for name, have in (("strata/changes", files), ("refs/strata/revisions", set(rev_refs))):
        if set(revs) - have:
            raise Failed("revision %s recorded in the ledger is missing from %s" % (sorted(set(revs) - have)[0][:12], name))
        if have - set(revs):
            raise Failed("revision %s in %s is not in the ledger" % (sorted(have - set(revs))[0][:12], name))

    checked = set()

    def check_tree(h, sha, at):
        if (h, sha) in checked:
            return
        try:
            raw = open(os.path.join(side, "trees", h + ".json"), "rb").read()
        except FileNotFoundError:
            raise Failed("tree %s (%s) is missing" % (h[:12], at))
        if sha256(raw) != h:
            raise Failed("tree %s (%s) does not hash to its name" % (h[:12], at))
        gt, kept = git.tree(sha), 0
        for e in json.loads(raw)["entries"]:
            p = at + e["name"]
            if e["name"].lower() == ".git":
                if e["name"] in gt:
                    raise Failed("%s: Git holds an entry the format omits" % p)
                continue
            kept += 1
            if e["name"] not in gt:
                raise Failed("%s is missing from Git" % p)
            mode, gsha = gt[e["name"]]
            if mode != MODES[e["kind"]]:
                raise Failed("%s: Git mode %s, want %s" % (p, mode, MODES[e["kind"]]))
            if e["kind"] == "dir":
                check_tree(e["hash"], gsha, p + "/")
                continue
            blob = git.get(gsha, "blob")
            if sha256(blob) != e["hash"] or len(blob) != e.get("size", 0):
                raise Failed("%s: content does not match its SHA-256 in the change" % p)
        if kept != len(gt):
            raise Failed("Git tree at %s has entries the change does not" % at)
        checked.add((h, sha))

    def ident(who, created):
        import calendar
        import time
        clean = "".join("_" if ch in "<>\n\r\x00" else ch for ch in who)
        # createdAt is UTC RFC 3339 ("...Z"); Git keeps whole seconds.
        secs = calendar.timegm(time.strptime(created[:19], "%Y-%m-%dT%H:%M:%S"))
        return "%s <%s> %d +0000" % (clean, clean, secs)

    for rev in revs:
        raw = open(os.path.join(side, "changes", rev + ".json"), "rb").read()
        if sha256(raw) != rev:
            raise Failed("strata/changes/%s.json does not hash to its revision" % rev[:12])
        c = json.loads(raw)
        if c["repo"] != repo:
            raise Failed("change %s belongs to %r" % (rev[:12], c["repo"]))
        gc = git.commit(rev_refs[rev])
        check_tree(c["tree"], gc["tree"], "/")
        if [rev_refs.get(p) for p in c["parents"]] != gc["parents"]:
            raise Failed("revision %s: Git parents differ from the change" % rev[:12])
        who = ident(c["author"], c["createdAt"])
        if gc.get("author") != who or gc.get("committer") != who:
            raise Failed("revision %s: Git author differs from the change" % rev[:12])
        msg = c["message"].rstrip("\n") + "\n\nStrata-Revision: %s\nStrata-Change-Id: %s\n" % (rev, c["changeId"])
        if gc["message"] != msg:
            raise Failed("revision %s: Git commit message differs from the change" % rev[:12])

    heads = git.refs("refs/heads")
    omitted = set(m.get("omittedRefs") or [])
    for name in omitted:
        if valid_git_ref(name):
            raise Failed("manifest omits ref %r, which Git can hold" % name)
    for name, target in refs.items():
        if name in omitted:
            continue
        if heads.get(name) != rev_refs.get(target):
            raise Failed("ref %s in Git does not point at %s, its target per the ledger" % (name, target[:12]))
    for name in heads:
        if name not in refs:
            raise Failed("Git has ref %s, which the ledger does not leave standing" % name)

    got = [(a["revision"], a["author"], a.get("note", ""), go_json(a.get("metadata", {})))
           for a in read_jsonl(os.path.join(side, "annotations.jsonl"))]
    if sorted(got) != sorted(anns):
        raise Failed("annotations differ from the ledger")
    got = [(a["revision"], a["approver"], a.get("note", "")) for a in read_jsonl(os.path.join(side, "approvals.jsonl"))]
    if sorted(got) != sorted(apps):
        raise Failed("approvals differ from the ledger")
    if read_json(os.path.join(side, "repo.json")).get("extends") != extends:
        raise Failed("repo.json Layer setting differs from the ledger")

    bg = os.path.join(side, "break-glass", "audit.jsonl")
    bg_recs, bg_anchors = None, 0
    if os.path.exists(bg):
        bg_recs = read_jsonl(bg)
        verify_chain(bg_recs)
        bg_anchors, _ = verify_anchors(read_json(os.path.join(side, "break-glass", "anchors.json")), bg_recs, key,
                                       "break-glass/anchors.json")
    elif m.get("breakGlass"):
        raise Failed("manifest says the break-glass ledger is included, but it is missing")
    return {"repo": repo, "records": len(recs), "changes": len(revs), "refs": len(heads), "anchors": anchors,
            "unanchored": unanchored, "annotations": len(anns), "approvals": len(apps),
            "breakGlassRecords": bg_recs, "breakGlassAnchors": bg_anchors}


def main(argv):
    args, key = [], None
    it = iter(argv)
    for a in it:
        if a == "--anchor-key":
            key = next(it, None)
        else:
            args.append(a)
    if len(args) != 1 or (key is not None and not os.path.exists(key)):
        print(__doc__.strip().splitlines()[-2], file=sys.stderr)
        return 2
    try:
        r = verify(args[0], key)
    except Failed as e:
        print("export FAILED verification: %s" % e)
        return 1
    print("verified export of %(repo)s: %(records)d ledger records, %(changes)d changes, %(refs)d refs, "
          "%(annotations)d annotations, %(approvals)d approvals, %(anchors)d anchors" % r)
    if r["unanchored"]:
        print("  %d records after the last anchor rest on the hash chain alone" % r["unanchored"])
    if r["breakGlassRecords"] is not None:
        print("verified %d break-glass records, %d anchors" % (len(r["breakGlassRecords"]), r["breakGlassAnchors"]))
    signed = r["anchors"] + r["breakGlassAnchors"]
    if not key:
        print("  anchor signatures: NOT checked (--anchor-key)")
    else:
        print("  anchor signatures: %d verified with the pinned key (openssl)" % signed)
    print("  timestamps: not checked by this tool (see docs/export-format.md, openssl ts -verify)")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
