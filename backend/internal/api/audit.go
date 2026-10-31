package api

import (
	"net/http"
	"strconv"

	"github.com/aws-abaschen/strata/backend/internal/authz"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

const maxAuditPage = 500

// auditLog pages through a repository's ledger: GET /repos/{repo}/audit?after=0&limit=200.
// The server checks the page links to the record before it; clients should
// still verify independently (strata audit --verify).
func (a *API) auditLog(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repoFor(r, authz.Read)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if after < 0 {
		after = 0
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > maxAuditPage {
		n = maxAuditPage
	}
	var prev core.Hash
	if after > 0 {
		before, err := a.Meta.ListAudit(ctx, rp.Name, after-1, 1)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if len(before) == 1 {
			prev = before[0].Hash
		}
	}
	recs, err := a.Meta.ListAudit(ctx, rp.Name, after, n)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := map[string]any{"repo": rp.Name, "records": recs, "verified": true}
	if _, err := core.VerifyAudit(recs, prev); err != nil {
		out["verified"], out["error"] = false, err.Error()
	}
	if len(recs) == n {
		out["next"] = recs[len(recs)-1].Seq
	}
	writeJSON(w, http.StatusOK, out)
}

// maxAnchorPage bounds a page: each proof carries a timestamp token of a few KB.
const maxAnchorPage = 100

// auditAnchors returns the signed anchors that cover a repository's ledger:
// GET /repos/{repo}/audit/anchors?after=0. Each proof shows only this
// chain's head and sibling hashes, never other repositories' names.
func (a *API) auditAnchors(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repoFor(r, authz.Read)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.anchorPage(w, r, rp.Name)
}

func (a *API) anchorPage(w http.ResponseWriter, r *http.Request, chain string) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	ps, err := a.Meta.ListAnchorProofs(r.Context(), chain, max(after, 0), maxAnchorPage)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := map[string]any{"chain": chain, "anchors": ps}
	if len(ps) == maxAnchorPage {
		out["next"] = ps[len(ps)-1].Statement.Seq
	}
	writeJSON(w, http.StatusOK, out)
}
