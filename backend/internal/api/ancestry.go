package api

import (
	"container/heap"
	"context"
	"fmt"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// maxRangeWalk caps how many changes are read to work out a range (a ref
// move or a merge). A larger range fails closed: not a fast-forward, no
// approvals, no merge base.
const maxRangeWalk = 200

const (
	fromHead = 1 << iota
	fromBase
	stale // an ancestor of a common change: not a merge base itself
)

// rangeWalk paints the history reachable from two tips, newest first, like
// Git's merge-base walk. A change reachable from head but not from base is
// "in the range". Parents are written before their children, so CreatedAt
// orders the walk; if clocks disagree the walk reads more than it needs and
// treats some shared changes as in the range, which only removes approvers.
type rangeWalk struct {
	// InRange lists the changes reachable from head only.
	InRange []core.Change
	// BaseInHead is true when base is an ancestor of head (or equal).
	BaseInHead bool
	// Common lists the newest changes reachable from both tips (merge bases).
	Common []core.Hash
	// Truncated is true when the walk stopped at maxRangeWalk.
	Truncated bool
}

type walkItem struct {
	c     core.Change
	flags int
}

type walkQueue []*walkItem

func (q walkQueue) Len() int { return len(q) }
func (q walkQueue) Less(i, j int) bool {
	if !q[i].c.CreatedAt.Equal(q[j].c.CreatedAt) {
		return q[i].c.CreatedAt.After(q[j].c.CreatedAt)
	}
	return q[i].c.Revision > q[j].c.Revision
}
func (q walkQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *walkQueue) Push(x any)   { *q = append(*q, x.(*walkItem)) }
func (q *walkQueue) Pop() any {
	old := *q
	it := old[len(old)-1]
	*q = old[:len(old)-1]
	return it
}

// walkRange compares head with base ("" = no base: only head is read).
// With bases set it keeps walking until the merge bases are known; without,
// it stops as soon as the range is.
func (a *API) walkRange(ctx context.Context, repo string, head core.Change, base core.Hash, bases bool) (rangeWalk, error) {
	var out rangeWalk
	if base == head.Revision {
		out.BaseInHead = true
		out.Common = []core.Hash{base}
		return out, nil
	}
	flags := map[core.Hash]int{}
	queued := map[core.Hash]*walkItem{}
	q := &walkQueue{}
	push := func(c core.Change, f int) {
		flags[c.Revision] |= f
		if it, ok := queued[c.Revision]; ok {
			it.flags |= f
			return
		}
		it := &walkItem{c: c, flags: flags[c.Revision]}
		queued[c.Revision] = it
		heap.Push(q, it)
	}
	push(head, fromHead)
	if base != "" {
		b, err := a.Meta.GetChange(ctx, repo, base)
		if err != nil {
			return out, fmt.Errorf("walking history at %s: %w", base.Short(), err)
		}
		push(b, fromBase)
	}
	// Stop once nothing queued is reachable from head only (or, for bases,
	// once everything queued is older than a merge base).
	more := func() bool {
		for _, it := range *q {
			if bases && it.flags&stale == 0 || it.flags&(fromBase|stale) == 0 {
				return true
			}
		}
		return false
	}
	reads := 0
	for q.Len() > 0 && more() {
		it := heap.Pop(q).(*walkItem)
		delete(queued, it.c.Revision)
		f := flags[it.c.Revision]
		switch {
		case f&stale != 0:
		case f == fromHead:
			out.InRange = append(out.InRange, it.c)
		case f == fromHead|fromBase:
			if it.c.Revision == base {
				out.BaseInHead = true
			}
			out.Common = append(out.Common, it.c.Revision)
			f |= stale // its ancestors are shared, and older than this base
		}
		for _, p := range it.c.Parents {
			if flags[p]&f == f {
				continue // already painted with these flags
			}
			if f == fromHead && p == base {
				out.BaseInHead = true
			}
			if it, ok := queued[p]; ok {
				flags[p] |= f
				it.flags |= f
				continue
			}
			if reads >= maxRangeWalk {
				out.Truncated = true
				return out, nil
			}
			reads++
			pc, err := a.Meta.GetChange(ctx, repo, p)
			if err != nil {
				return out, fmt.Errorf("walking history at %s: %w", p.Short(), err)
			}
			push(pc, f)
		}
	}
	return out, nil
}
