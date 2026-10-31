// Package anchor turns the hash-chained audit ledger into external evidence.
//
// A scheduled job (cmd/anchor) runs Job.Run. Each run:
//
//  1. checks that every head committed to by the previous anchor is still in
//     the table, unchanged (a rewrite of any chain's tail shows up here);
//  2. reconciles the Object Lock archive against the table, record by record
//     and hash by hash, up to the previous anchor's heads (missing or
//     different archive records are gaps and mismatches);
//  3. if any head moved, or a day has passed, signs a statement over a Merkle
//     root of all heads that also commits to the previous anchor, gets an
//     RFC 3161 timestamp for it, and stores it with a compare-and-swap so
//     anchors never fork.
//
// Readers verify their chain against every anchor that covered it (Verify),
// with a public key and a timestamp authority they trust out of band.
package anchor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// Signer signs a SHA-256 digest (ECDSA P-256, ASN.1 DER).
type Signer interface {
	KeyID() string
	Sign(ctx context.Context, digest []byte) ([]byte, error)
}

// TSA returns an RFC 3161 TimeStampToken (DER) over a SHA-256 digest.
type TSA interface {
	Stamp(ctx context.Context, digest []byte) ([]byte, error)
}

// Archive is the Object Lock copy of the ledger.
type Archive interface {
	// Get returns an archived record (store.ErrNotFound if missing).
	Get(ctx context.Context, repo string, seq int64) (core.AuditRecord, error)
	// PutAnchor writes the full anchor once.
	PutAnchor(ctx context.Context, a core.Anchor) error
}

// Heartbeat is how often an anchor is written even when no chain moved,
// so a stalled job is visible and the anchor chain keeps getting timestamps.
const Heartbeat = 24 * time.Hour

// maxChecksPerRun caps archive reads in one run; the rest continues next run.
const maxChecksPerRun = 5000

// Job is one anchoring run's dependencies.
type Job struct {
	Meta    store.Meta
	Archive Archive
	Signer  Signer
	TSA     TSA // optional
	Now     func() time.Time
	// Region is this region. Only the writer region signs anchors (two
	// signers would fork the anchor chain); every region reconciles its own
	// archive. Empty: one region, always sign.
	Region string
}

// Problem is one integrity finding.
type Problem struct {
	Repo   string `json:"repo"`
	Seq    int64  `json:"seq"`
	Detail string `json:"detail"`
}

// Report says what a run found and did.
type Report struct {
	Chains          int       `json:"chains"`
	Anchored        int64     `json:"anchored,omitempty"` // seq of the anchor written, 0 if none
	Checked         int       `json:"checked"`            // archive records compared this run
	Rewrites        []Problem `json:"rewrites,omitempty"`
	Gaps            []Problem `json:"gaps,omitempty"`
	Mismatches      []Problem `json:"mismatches,omitempty"`
	TimestampFailed bool      `json:"timestampFailed,omitempty"`
	// Standby is set when this region is not the writer: it checked but did
	// not sign.
	Standby bool `json:"standby,omitempty"`
}

func (j *Job) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// Run does one pass. Integrity findings go in the report; the error is for
// failures to run at all.
func (j *Job) Run(ctx context.Context) (Report, error) {
	var rep Report
	if j.Region != "" {
		w, err := j.Meta.Writer(ctx)
		if err != nil {
			return rep, err
		}
		rep.Standby = w.Region != j.Region
	}
	prev, indexed, err := j.Meta.LatestAnchor(ctx)
	havePrev := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return rep, err
	}
	if havePrev && !indexed && !rep.Standby {
		// The last run stored the anchor but not its proofs: finish that first.
		if err := j.Meta.PutAnchorProofs(ctx, prev.Statement.Seq, proofs(prev, prev.Heads)); err != nil {
			return rep, err
		}
	}

	heads, err := j.heads(ctx)
	if err != nil {
		return rep, err
	}
	rep.Chains = len(heads)

	if havePrev {
		if rep.Rewrites, err = j.checkAnchored(ctx, prev.Heads, rep.Standby); err != nil {
			return rep, err
		}
		if err := j.reconcile(ctx, prev.Heads, &rep); err != nil {
			return rep, err
		}
	}
	if rep.Standby {
		return rep, nil
	}

	// Never sign over tampering: the previous anchor stays the baseline, so
	// every run reports it again and the alarm stays raised until a person
	// investigates. (Gaps only mean the ledger Lambda missed a record.)
	if len(rep.Rewrites) > 0 || len(rep.Mismatches) > 0 {
		return rep, nil
	}
	if havePrev && core.SameLeaves(prev.Heads, heads) && j.now().Sub(prev.Statement.CreatedAt) < Heartbeat {
		return rep, nil
	}
	a, err := j.seal(ctx, prev, havePrev, heads)
	if err != nil {
		return rep, err
	}
	rep.TimestampFailed = a.Seal.TimestampError != ""
	if err := j.Meta.PutAnchor(ctx, a); err != nil {
		return rep, fmt.Errorf("store anchor %d: %w", a.Statement.Seq, err)
	}
	if err := j.Archive.PutAnchor(ctx, a); err != nil {
		return rep, fmt.Errorf("archive anchor %d: %w", a.Statement.Seq, err)
	}
	changed := heads
	if havePrev {
		changed = changedLeaves(prev.Heads, heads)
	}
	if err := j.Meta.PutAnchorProofs(ctx, a.Statement.Seq, proofs(a, changed)); err != nil {
		return rep, err
	}
	rep.Anchored = a.Statement.Seq
	return rep, nil
}

// heads lists every non-empty chain's head, sorted by name.
func (j *Job) heads(ctx context.Context) ([]core.AnchorLeaf, error) {
	repos, err := j.Meta.ListRepos(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{core.BreakGlassChain}
	for _, r := range repos {
		names = append(names, r.Name)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	var out []core.AnchorLeaf
	for _, n := range names {
		seq, h, err := j.Meta.AuditHead(ctx, n)
		if err != nil {
			return nil, err
		}
		if seq > 0 {
			out = append(out, core.AnchorLeaf{Repo: n, Seq: seq, Hash: h})
		}
	}
	return out, nil
}

// checkAnchored confirms each anchored head is still in its chain, unchanged.
// Chains link every record to the one before, so this covers every older
// record too.
//
// In a standby, replication is item by item: an anchor can arrive before the
// records it covers, so a missing record is not yet a finding there. A
// different hash is.
func (j *Job) checkAnchored(ctx context.Context, anchored []core.AnchorLeaf, standby bool) ([]Problem, error) {
	var out []Problem
	for _, l := range anchored {
		recs, err := j.Meta.ListAudit(ctx, l.Repo, l.Seq-1, 1)
		if err != nil {
			return nil, err
		}
		switch {
		case (len(recs) == 0 || recs[0].Seq != l.Seq) && standby:
		case len(recs) == 0 || recs[0].Seq != l.Seq:
			out = append(out, Problem{l.Repo, l.Seq, "anchored record is gone from the table"})
		case recs[0].Hash != l.Hash:
			out = append(out, Problem{l.Repo, l.Seq, fmt.Sprintf("anchored hash %s, table now has %s", l.Hash.Short(), recs[0].Hash.Short())})
		}
	}
	return out, nil
}

// reconcile compares the archive with the table up to the anchored heads.
// Those records are at least one run old, so the ledger Lambda has had time
// to archive them. Progress stops at the first problem, so it is reported
// again every run until someone fixes it.
func (j *Job) reconcile(ctx context.Context, anchored []core.AnchorLeaf, rep *Report) error {
	progress, err := j.Meta.AnchorProgress(ctx)
	if err != nil {
		return err
	}
	budget := maxChecksPerRun
chains:
	for _, l := range anchored {
		for progress[l.Repo] < l.Seq && budget > 0 {
			recs, err := j.Meta.ListAudit(ctx, l.Repo, progress[l.Repo], min(200, budget))
			if err != nil {
				return err
			}
			if len(recs) == 0 {
				break
			}
			for _, r := range recs {
				if r.Seq > l.Seq {
					break
				}
				budget--
				rep.Checked++
				got, err := j.Archive.Get(ctx, l.Repo, r.Seq)
				if errors.Is(err, store.ErrNotFound) {
					rep.Gaps = append(rep.Gaps, Problem{l.Repo, r.Seq, "record missing from the Object Lock archive"})
					continue chains
				}
				if err != nil {
					return err
				}
				if got.Hash != r.Hash {
					rep.Mismatches = append(rep.Mismatches, Problem{l.Repo, r.Seq, fmt.Sprintf("archive has %s, table has %s", got.Hash.Short(), r.Hash.Short())})
					continue chains
				}
				progress[l.Repo] = r.Seq
			}
		}
	}
	return j.Meta.SetAnchorProgress(ctx, progress)
}

func (j *Job) seal(ctx context.Context, prev core.Anchor, havePrev bool, heads []core.AnchorLeaf) (core.Anchor, error) {
	leaves := make([][]byte, len(heads))
	for i, h := range heads {
		leaves[i] = core.LeafHash(h)
	}
	st := core.AnchorStatement{Version: core.AnchorVersion, Seq: 1, Root: hex.EncodeToString(core.MerkleRoot(leaves)),
		Leaves: len(heads), CreatedAt: j.now().UTC().Truncate(time.Millisecond)}
	if havePrev {
		st.Seq, st.Prev = prev.Statement.Seq+1, prev.Statement.DigestHex()
	}
	digest := st.Digest()
	sig, err := j.Signer.Sign(ctx, digest)
	if err != nil {
		return core.Anchor{}, fmt.Errorf("sign anchor: %w", err)
	}
	a := core.Anchor{Statement: st, Seal: core.AnchorSeal{KeyID: j.Signer.KeyID(), Signature: sig}, Heads: heads}
	if j.TSA == nil {
		a.Seal.TimestampError = "no timestamp authority configured"
		return a, nil
	}
	// A timestamp authority outage must not stop anchoring: record it, alarm on it.
	if tok, err := j.TSA.Stamp(ctx, digest); err != nil {
		a.Seal.TimestampError = err.Error()
	} else {
		a.Seal.Timestamp = tok
	}
	return a, nil
}

func changedLeaves(before, after []core.AnchorLeaf) []core.AnchorLeaf {
	old := map[string]core.AnchorLeaf{}
	for _, l := range before {
		old[l.Repo] = l
	}
	var out []core.AnchorLeaf
	for _, l := range after {
		if old[l.Repo] != l {
			out = append(out, l)
		}
	}
	return out
}

// proofs builds per-chain proofs for some of an anchor's heads.
func proofs(a core.Anchor, which []core.AnchorLeaf) []core.AnchorProof {
	leaves := make([][]byte, len(a.Heads))
	pos := map[string]int{}
	for i, h := range a.Heads {
		leaves[i] = core.LeafHash(h)
		pos[h.Repo] = i
	}
	var out []core.AnchorProof
	for _, l := range which {
		i, ok := pos[l.Repo]
		if !ok {
			continue
		}
		var path []string
		for _, p := range core.MerkleProof(leaves, i) {
			path = append(path, hex.EncodeToString(p))
		}
		out = append(out, core.AnchorProof{Statement: a.Statement, Seal: a.Seal, Leaf: a.Heads[i], Index: i, Proof: path})
	}
	return out
}
