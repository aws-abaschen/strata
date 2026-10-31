// Package dr moves the writer between the primary and standby regions.
//
// The index is a DynamoDB global table: replicated asynchronously, item by
// item, last writer wins. Exactly one region may write (store.Writer); every
// audited write checks that in its own transaction. This package changes the
// writer in two ways:
//
//   - Switchover (planned, drills): demote the writer first, so writes stop
//     there at once; wait until the target holds everything (writer item,
//     every chain head and record, every object); then the target writes.
//     No data is lost.
//   - Promote (the writer region is down): try to demote it, take the writer
//     locally, and repair chains that hold part of a transaction. Records
//     the target never received are lost, and the ledger says which.
//
// Both record what happened on the DR chain (store.DRChain).
package dr

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// Region is one side.
type Region struct {
	Name string
	API  string // the region's Strata API URL, given to clients by the writer item
	Meta store.Meta
	// Keys lists the region's data bucket (content-addressed objects and
	// sealed change documents).
	Keys func(ctx context.Context) (map[string]bool, error)
}

// Op is who does a writer change and why.
type Op struct {
	Actor    string // the IAM principal
	Reason   string
	WorkItem string
	Now      func() time.Time
	// Poll is how often switchover re-checks the target (default 2s).
	Poll time.Duration
	// Log, if set, gets progress lines.
	Log func(format string, a ...any)
}

func (o Op) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Op) logf(format string, a ...any) {
	if o.Log != nil {
		o.Log(format, a...)
	}
}

func (o Op) validate() error {
	if len(o.Reason) < 10 || o.WorkItem == "" || o.Actor == "" {
		return fmt.Errorf("%w: a reason (10+ characters), a work item and an actor are required", core.ErrInvalid)
	}
	return nil
}

// Status is both sides' view.
type Status struct {
	Writer   map[string]store.Writer // as each region sees it
	Chains   []ChainDiff
	Objects  map[string]int // object count per region
	Missing  map[string]int // objects the other region has and this one lacks
	InSync   bool
	Problems []string
}

// ChainDiff is one chain in both regions.
type ChainDiff struct {
	Repo  string
	State map[string]store.ChainState
}

// Same reports whether both regions hold the chain identically and whole.
func (c ChainDiff) Same() bool {
	var first *store.ChainState
	for _, s := range c.State {
		if !s.Consistent() {
			return false
		}
		if first == nil {
			first = &s
			continue
		}
		if s.HeadSeq != first.HeadSeq || s.HeadHash != first.HeadHash {
			return false
		}
	}
	return true
}

// chains lists every chain either region knows.
func chains(ctx context.Context, rs ...Region) ([]string, error) {
	names := []string{core.BreakGlassChain, store.DRChain}
	for _, r := range rs {
		repos, err := r.Meta.ListRepos(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Name, err)
		}
		for _, rp := range repos {
			names = append(names, rp.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// Compare reads both regions.
func Compare(ctx context.Context, a, b Region) (Status, error) {
	st := Status{Writer: map[string]store.Writer{}, Objects: map[string]int{}, Missing: map[string]int{}}
	for _, r := range []Region{a, b} {
		w, err := r.Meta.Writer(ctx)
		if err != nil {
			return st, fmt.Errorf("%s writer: %w", r.Name, err)
		}
		st.Writer[r.Name] = w
	}
	names, err := chains(ctx, a, b)
	if err != nil {
		return st, err
	}
	for _, n := range names {
		d := ChainDiff{Repo: n, State: map[string]store.ChainState{}}
		empty := true
		for _, r := range []Region{a, b} {
			cs, err := r.Meta.InspectChain(ctx, n)
			if err != nil {
				return st, fmt.Errorf("%s chain %s: %w", r.Name, n, err)
			}
			d.State[r.Name] = cs
			empty = empty && cs.HeadSeq == 0 && cs.LastSeq == 0
		}
		if !empty {
			st.Chains = append(st.Chains, d)
		}
	}
	ka, err := a.Keys(ctx)
	if err != nil {
		return st, fmt.Errorf("%s objects: %w", a.Name, err)
	}
	kb, err := b.Keys(ctx)
	if err != nil {
		return st, fmt.Errorf("%s objects: %w", b.Name, err)
	}
	st.Objects[a.Name], st.Objects[b.Name] = len(ka), len(kb)
	for k := range ka {
		if !kb[k] {
			st.Missing[b.Name]++
		}
	}
	for k := range kb {
		if !ka[k] {
			st.Missing[a.Name]++
		}
	}
	wa, wb := st.Writer[a.Name], st.Writer[b.Name]
	if wa.Region != wb.Region || wa.Epoch != wb.Epoch {
		st.Problems = append(st.Problems, fmt.Sprintf("writer differs: %s sees %s (epoch %d), %s sees %s (epoch %d)", a.Name, wa.Region, wa.Epoch, b.Name, wb.Region, wb.Epoch))
	}
	for _, c := range st.Chains {
		if !c.Same() {
			st.Problems = append(st.Problems, fmt.Sprintf("chain %s differs: %s", c.Repo, describe(c)))
		}
	}
	for r, n := range st.Missing {
		if n > 0 {
			st.Problems = append(st.Problems, fmt.Sprintf("%s lacks %d objects", r, n))
		}
	}
	st.InSync = len(st.Problems) == 0
	return st, nil
}

func describe(c ChainDiff) string {
	var out string
	for _, r := range slices.Sorted(mapsKeys(c.State)) {
		s := c.State[r]
		out += fmt.Sprintf("%s head %d last %d orphans %d; ", r, s.HeadSeq, s.LastSeq, len(s.Orphans))
	}
	return out
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// Result is what a writer change did, also recorded on the DR chain.
type Result struct {
	Mode       string    `json:"mode"` // switchover or promote
	From       string    `json:"from"`
	To         string    `json:"to"`
	Epoch      int64     `json:"epoch"`
	Reason     string    `json:"reason"`
	WorkItem   string    `json:"workItem"`
	DemotedAt  time.Time `json:"demotedAt,omitzero"` // writes stopped in From
	WritableAt time.Time `json:"writableAt"`         // To accepts writes
	// WritesUnavailable is DemotedAt to WritableAt: the recovery time a
	// client saw (switchover).
	WritesUnavailable string `json:"writesUnavailable,omitempty"`
	// OldFenced says whether From was demoted (promote may not reach it).
	OldFenced bool          `json:"oldFenced"`
	Chains    int           `json:"chains"`
	Objects   int           `json:"objects"`
	Repairs   []ChainRepair `json:"repairs,omitempty"`
	// MissingObjects are objects From had and To lacked (promote).
	MissingObjects int `json:"missingObjects,omitempty"`
}

// ChainRepair is a chain promotion had to fix.
type ChainRepair struct {
	Repo     string    `json:"repo"`
	HeadSeen int64     `json:"headSeen"`
	HashSeen core.Hash `json:"hashSeen,omitempty"`
	// ContinuedFrom is the record new writes follow.
	ContinuedFrom int64 `json:"continuedFrom"`
	// Lost lists seqs the head said existed but this region never received.
	Lost []int64 `json:"lost,omitempty"`
	// Orphans are records present but off the chain, moved to ORPHAN#<repo>.
	Orphans []int64 `json:"orphans,omitempty"`
}

// Switchover moves the writer from one region to the other with no data loss.
func Switchover(ctx context.Context, from, to Region, op Op, timeout time.Duration) (Result, error) {
	res := Result{Mode: "switchover", From: from.Name, To: to.Name, Reason: op.Reason, WorkItem: op.WorkItem}
	if err := op.validate(); err != nil {
		return res, err
	}
	st, err := Compare(ctx, from, to)
	if err != nil {
		return res, err
	}
	cur := st.Writer[from.Name]
	if cur.Region != from.Name {
		return res, fmt.Errorf("%s is not the writer (it sees %s, epoch %d)", from.Name, cur.Region, cur.Epoch)
	}
	if t := st.Writer[to.Name]; t.Epoch != cur.Epoch {
		return res, fmt.Errorf("the regions disagree on the writer (%s epoch %d, %s epoch %d): run status", from.Name, cur.Epoch, to.Name, t.Epoch)
	}

	// 1. Demote: writes in From stop now (their fence reads From's item).
	next := store.Writer{Region: to.Name, Epoch: cur.Epoch + 1, API: to.API, Since: op.now().UTC(), By: op.Actor, Reason: op.Reason + " (" + op.WorkItem + ")"}
	if err := from.Meta.SetWriter(ctx, next, cur.Epoch); err != nil {
		return res, fmt.Errorf("demote %s: %w", from.Name, err)
	}
	res.DemotedAt, res.Epoch = next.Since, next.Epoch
	op.logf("%s demoted at %s (epoch %d): writes there now fail", from.Name, res.DemotedAt.Format(time.RFC3339), next.Epoch)

	// 2. Wait until To holds everything. The writer item arriving proves
	// nothing about other items: replication has no order across items.
	poll := op.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := op.now().Add(timeout)
	for {
		st, err = Compare(ctx, from, to)
		if err != nil {
			return res, err
		}
		if st.InSync && st.Writer[to.Name].Epoch == next.Epoch {
			break
		}
		if op.now().After(deadline) {
			return res, fmt.Errorf("%s not in sync after %s, writes are stopped in both regions; fix and rerun switchover from %s, or promote: %v", to.Name, timeout, to.Name, st.Problems)
		}
		op.logf("waiting for %s: %v", to.Name, st.Problems)
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(poll):
		}
	}

	// 3. To writes from here. Record it there, as the first write.
	res.WritableAt = op.now().UTC()
	res.WritesUnavailable = res.WritableAt.Sub(res.DemotedAt).Round(time.Millisecond).String()
	res.OldFenced, res.Chains, res.Objects = true, len(st.Chains), st.Objects[to.Name]
	return res, record(ctx, to, op, res)
}

// Promote makes to the writer when the writer region is unreachable. It
// tries to demote from first (so an old writer that is still up stops), but
// goes on without it.
func Promote(ctx context.Context, from, to Region, op Op, fenceTimeout time.Duration) (Result, error) {
	res := Result{Mode: "promote", From: from.Name, To: to.Name, Reason: op.Reason, WorkItem: op.WorkItem}
	if err := op.validate(); err != nil {
		return res, err
	}
	cur, err := to.Meta.Writer(ctx)
	if err != nil {
		return res, err
	}
	if cur.Region == to.Name {
		return res, fmt.Errorf("%s is already the writer (epoch %d)", to.Name, cur.Epoch)
	}
	next := store.Writer{Region: to.Name, Epoch: cur.Epoch + 1, API: to.API, Since: op.now().UTC(), By: op.Actor, Reason: op.Reason + " (" + op.WorkItem + ", forced)"}

	// 1. Best effort: fence the old writer so it stops if it is still up.
	fctx, cancel := context.WithTimeout(ctx, fenceTimeout)
	ferr := from.Meta.SetWriter(fctx, next, cur.Epoch)
	cancel()
	res.OldFenced = ferr == nil
	if ferr != nil {
		op.logf("could not demote %s (%v): if it is still serving clients it keeps writing until replication brings it the new writer; its extra records will show as forks", from.Name, ferr)
	}

	// 2. Take the writer here.
	if err := to.Meta.SetWriter(ctx, next, cur.Epoch); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			return res, err
		}
		// The demote above may already have replicated here.
		if w, _ := to.Meta.Writer(ctx); w.Region != to.Name || w.Epoch != next.Epoch {
			return res, fmt.Errorf("take writer in %s: %w (someone else changed it: run status)", to.Name, err)
		}
	}
	res.Epoch = next.Epoch

	// 3. Repair chains holding part of a transaction, then say so in each.
	names, err := chains(ctx, to)
	if err != nil {
		return res, err
	}
	for _, n := range names {
		cs, err := to.Meta.InspectChain(ctx, n)
		if err != nil {
			return res, err
		}
		if cs.HeadSeq == 0 && cs.LastSeq == 0 && len(cs.Orphans) == 0 {
			continue
		}
		res.Chains++
		if cs.Consistent() {
			continue
		}
		rp := ChainRepair{Repo: n, HeadSeen: cs.HeadSeq, HashSeen: cs.HeadHash, ContinuedFrom: cs.LastSeq, Orphans: cs.Orphans, Lost: cs.Lost}
		// Orphans would block the seqs new writes need: move them aside.
		if err := to.Meta.QuarantineAudit(ctx, n, cs.Orphans, next.Epoch); err != nil {
			return res, fmt.Errorf("quarantine %s: %w", n, err)
		}
		if cs.LastSeq != cs.HeadSeq || cs.LastHash != cs.HeadHash {
			if err := to.Meta.SetAuditHead(ctx, n, cs.LastSeq, cs.LastHash, cs.HeadSeq); err != nil {
				return res, fmt.Errorf("repair %s: %w", n, err)
			}
		}
		e, err := core.NewAuditEntry("ChainRepairedAfterPromotion", op.Actor, op.now(), rp)
		if err != nil {
			return res, err
		}
		if err := to.Meta.AppendAudit(ctx, n, e); err != nil {
			return res, fmt.Errorf("record repair of %s: %w", n, err)
		}
		res.Repairs = append(res.Repairs, rp)
		op.logf("repaired %s: head was %d, continues from %d, lost %v, orphans %v", n, cs.HeadSeq, cs.LastSeq, rp.Lost, rp.Orphans)
	}

	// 4. Objects the old writer had that never arrived (if it can be listed).
	if kf, err := from.Keys(ctx); err == nil {
		if kt, err := to.Keys(ctx); err == nil {
			for k := range kf {
				if !kt[k] {
					res.MissingObjects++
				}
			}
			res.Objects = len(kt)
		}
	}
	res.WritableAt = op.now().UTC()
	return res, record(ctx, to, op, res)
}

func record(ctx context.Context, r Region, op Op, res Result) error {
	e, err := core.NewAuditEntry("WriterChanged", op.Actor, op.now(), res)
	if err != nil {
		return err
	}
	if err := r.Meta.AppendAudit(ctx, store.DRChain, e); err != nil {
		return fmt.Errorf("record on %s: %w", store.DRChain, err)
	}
	return nil
}

// Init creates the writer item in the primary (epoch 1).
func Init(ctx context.Context, primary Region, op Op) (store.Writer, error) {
	w := store.Writer{Region: primary.Name, Epoch: 1, API: primary.API, Since: op.now().UTC(), By: op.Actor, Reason: "initial writer"}
	if err := primary.Meta.SetWriter(ctx, w, 0); err != nil {
		return w, err
	}
	e, err := core.NewAuditEntry("WriterInitialized", op.Actor, op.now(), w)
	if err != nil {
		return w, err
	}
	return w, primary.Meta.AppendAudit(ctx, store.DRChain, e)
}
