package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/aws-abaschen/strata/backend/internal/authz"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// approve records the caller's sign-off on one exact revision. The rule that
// nobody approves their own work is in code, not in policy, so no policy
// edit (and no admin) can switch it off.
func (a *API) approve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repoFor(r, authz.Approve)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rev, err := hashParam(r, "rev")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if len(in.Note) > 4*1024 {
		a.fail(w, r, badRequest("note longer than 4 KiB"))
		return
	}
	c, err := a.Meta.GetChange(ctx, rp.Name, rev)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	me := identity(r)
	if c.Author == me {
		a.fail(w, r, fmt.Errorf("%w: %s authored this change and cannot approve it", core.ErrPolicy, me))
		return
	}
	ap := core.Approval{Repo: rp.Name, Revision: rev, Approver: me, CreatedAt: a.now().UTC(), Note: in.Note}
	entry, err := a.audit(r, "ApprovalGranted", map[string]any{
		"repo": rp.Name, "revision": rev, "changeId": c.ChangeID, "author": c.Author, "approver": me, "note": in.Note,
		"link": a.changeURL(rp.Name, rev),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.PutApproval(ctx, ap, entry); err != nil {
		if errors.Is(err, store.ErrExists) {
			err = fmt.Errorf("%w: you already approved %s", store.ErrExists, rev.Short())
		}
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, ap)
}

func (a *API) listApprovals(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repoFor(r, authz.Read)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rev, err := hashParam(r, "rev")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list, err := a.Meta.ListApprovals(r.Context(), rp.Name, rev)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
}

// refMove is the outcome of authorizing a ref move.
type refMove struct {
	Approvers []string
}

// authorizeRefMove computes the facts about moving ref from current to head
// (zero Change = delete) and asks the policies. It is called for every write
// that moves a ref, so a policy can protect a branch (`context.ref`), demand
// approvals, a linked work item, or a fast-forward.
func (a *API) authorizeRefMove(ctx context.Context, p authz.Principal, rp core.Repo, ref string, head core.Change, current core.Hash) (refMove, error) {
	f := authz.Facts{Ref: ref}
	var mv refMove
	if head.Revision != "" || head.Tree != "" {
		var err error
		if f, mv.Approvers, err = a.refFacts(ctx, rp.Name, ref, head, current); err != nil {
			return mv, err
		}
	}
	return mv, a.authz.RequireWith(ctx, p, authz.Write, rp, f)
}

// refFacts walks the history from head back to current, through every
// parent, so a merge counts the authors of the branch it brings in. If
// current is reached the move is a fast-forward, and every author in the
// range is excluded from the approvers that count: A writes c1, B writes c2
// on top, then A cannot supply the second pair of eyes for c2. A range too
// long to read counts no approvals at all.
func (a *API) refFacts(ctx context.Context, repo, ref string, head core.Change, current core.Hash) (authz.Facts, []string, error) {
	f := authz.Facts{Ref: ref}
	authors := map[string]bool{head.Author: true}
	truncated := false
	switch {
	case current == "":
		f.FastForward = true
	case head.Revision == current:
		f.FastForward = true
	default:
		rw, err := a.walkRange(ctx, repo, head, current, false)
		if err != nil {
			return f, nil, err
		}
		for _, c := range rw.InRange {
			authors[c.Author] = true
		}
		f.FastForward = rw.BaseInHead && !rw.Truncated
		truncated = rw.Truncated
	}
	var approvers []string
	if head.Revision != "" && !truncated {
		list, err := a.Meta.ListApprovals(ctx, repo, head.Revision)
		if err != nil {
			return f, nil, err
		}
		for _, ap := range list {
			if !authors[ap.Approver] && !slices.Contains(approvers, ap.Approver) {
				approvers = append(approvers, ap.Approver)
			}
		}
		sort.Strings(approvers)
	}
	f.Approvals = len(approvers)

	md := head.Metadata
	if head.Revision != "" {
		notes, err := a.Meta.ListAnnotations(ctx, repo, head.Revision)
		if err != nil {
			return f, nil, err
		}
		for _, n := range notes {
			md.Merge(n.Metadata)
		}
	}
	f.HasWorkItem = len(md.WorkItems) > 0
	return f, approvers, nil
}
