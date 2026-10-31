package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/authz"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// freshAuth is how recent a sign-in must be to activate break-glass.
const freshAuth = 15 * time.Minute

// Break-glass is enforced in code, not in Cedar: activating needs
// strata-admins membership from the token, and an active grant overrides a
// policy deny (see authz.Authorizer.CheckWith). A broken or hostile policy
// set can therefore never remove the emergency path.

func newGrantID(t time.Time) string {
	b := make([]byte, 3)
	rand.Read(b)
	return "bg-" + t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

func requireAdmin(p authz.Principal, what string) error {
	if !p.IsAdmin() {
		return fmt.Errorf("%w: only members of %s may %s", core.ErrPolicy, authz.AdminGroup, what)
	}
	return nil
}

// canSeeGrants: administrators, or whoever policy allows Action::"audit" on
// Repo::"_breakglass", such as auditors.
func (a *API) canSeeGrants(r *http.Request) error {
	p := principal(r)
	if p.IsAdmin() {
		return nil
	}
	d, err := a.authz.Check(r.Context(), p, authz.Audit, core.Repo{Name: core.BreakGlassChain, Kind: "system"})
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fmt.Errorf("%w: break-glass grants are visible to %s and to whoever policy allows audit on %s",
			core.ErrPolicy, authz.AdminGroup, core.BreakGlassChain)
	}
	return nil
}

func (a *API) activateGrant(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := requireAdmin(p, "activate break-glass"); err != nil {
		a.fail(w, r, err)
		return
	}
	var in struct {
		Reason   string   `json:"reason"`
		WorkItem string   `json:"workItem"`
		Repos    []string `json:"repos"`
		Minutes  int      `json:"minutes"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	now := a.now().UTC()
	if a.RequireFreshAuth && (p.AuthTime.IsZero() || now.Sub(p.AuthTime) > freshAuth) {
		a.fail(w, r, fmt.Errorf("%w: break-glass needs a sign-in with MFA in the last 15 minutes; sign in again and use a new token", core.ErrPolicy))
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len(in.Reason) < 10 || len(in.Reason) > 2048 {
		a.fail(w, r, badRequest("reason: say what is broken and why (10 to 2048 characters)"))
		return
	}
	wi, err := core.ParseWorkItem(strings.TrimSpace(in.WorkItem))
	if strings.TrimSpace(in.WorkItem) == "" || err != nil {
		a.fail(w, r, badRequest("workItem: the incident or emergency-change ticket is required, e.g. INC-123 or servicenow:INC0012345"))
		return
	}
	if len(in.Repos) == 0 || len(in.Repos) > 50 {
		a.fail(w, r, badRequest(`repos: list 1 to 50 repositories, or "*" for all`))
		return
	}
	slices.Sort(in.Repos)
	in.Repos = slices.Compact(in.Repos)
	for _, n := range in.Repos {
		if n == "*" {
			continue
		}
		if err := core.ValidateRepoName(n); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	d := core.BreakGlassDefault
	if in.Minutes != 0 {
		d = time.Duration(in.Minutes) * time.Minute
	}
	if d < 5*time.Minute || d > core.BreakGlassMax {
		a.fail(w, r, badRequest("minutes: between 5 and %d", int(core.BreakGlassMax.Minutes())))
		return
	}
	g := core.Grant{ID: newGrantID(now), User: p.ID, Reason: in.Reason, WorkItem: wi, Repos: in.Repos, CreatedAt: now, ExpiresAt: now.Add(d)}
	detail := map[string]any{
		"grantId": g.ID, "user": g.User, "reason": g.Reason, "workItem": g.WorkItem, "repos": g.Repos,
		"allRepos": slices.Contains(g.Repos, "*"), "expiresAt": g.ExpiresAt,
	}
	if !p.AuthTime.IsZero() {
		detail["authTime"] = p.AuthTime
	}
	entry, err := a.audit(r, "BreakGlassActivated", detail)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.PutGrant(r.Context(), g, entry); err != nil {
		if errors.Is(err, store.ErrExists) {
			err = fmt.Errorf("%w: you already hold an active grant; end it first", store.ErrExists)
		}
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.view(g))
}

type grantView struct {
	core.Grant
	Active   bool               `json:"active"`
	Overdue  bool               `json:"overdue"`
	Activity []core.AuditRecord `json:"activity,omitempty"`
}

func (a *API) view(g core.Grant) grantView {
	now := a.now()
	return grantView{Grant: g, Active: g.ActiveAt(now), Overdue: g.OverdueAt(now)}
}

func (a *API) listGrants(w http.ResponseWriter, r *http.Request) {
	if err := a.canSeeGrants(r); err != nil {
		a.fail(w, r, err)
		return
	}
	gs, err := a.Meta.ListGrants(r.Context(), limit(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	open := r.URL.Query().Get("open") != ""
	out := []grantView{}
	for _, g := range gs {
		if open && g.Review != nil {
			continue
		}
		out = append(out, a.view(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) getGrant(w http.ResponseWriter, r *http.Request) {
	if err := a.canSeeGrants(r); err != nil {
		a.fail(w, r, err)
		return
	}
	g, err := a.Meta.GetGrant(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v := a.view(g)
	if v.Activity, err = a.Meta.ListGrantActivity(r.Context(), g.ID, 1000); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// endGrant ends a grant early. The holder or any other administrator may do
// it: revoking access is always safe.
func (a *API) endGrant(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := requireAdmin(p, "end a break-glass grant"); err != nil {
		a.fail(w, r, err)
		return
	}
	g, err := a.Meta.GetGrant(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	now := a.now().UTC()
	if !g.ActiveAt(now) {
		a.fail(w, r, fmt.Errorf("%w: grant %s is no longer active", store.ErrConflict, g.ID))
		return
	}
	g.EndedAt = &now
	entry, err := a.audit(r, "BreakGlassEnded", map[string]any{"grantId": g.ID, "user": g.User, "endedBy": p.ID, "endedAt": now})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.EndGrant(r.Context(), g, entry); err != nil {
		if errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrConflict) {
			err = fmt.Errorf("%w: grant %s already ended", store.ErrConflict, g.ID)
		}
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.view(g))
}

// reviewGrant records the after-the-fact review. The reviewer is another
// administrator, never the holder, and reviews only a grant that is over.
// A rejection is recorded and alerted; it does not roll anything back.
func (a *API) reviewGrant(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := requireAdmin(p, "review a break-glass grant"); err != nil {
		a.fail(w, r, err)
		return
	}
	var in struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if in.Outcome != core.ReviewAccepted && in.Outcome != core.ReviewRejected {
		a.fail(w, r, badRequest("outcome: %s or %s", core.ReviewAccepted, core.ReviewRejected))
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if len(in.Note) < 10 || len(in.Note) > 4096 {
		a.fail(w, r, badRequest("note: what you checked and found (10 to 4096 characters)"))
		return
	}
	ctx := r.Context()
	g, err := a.Meta.GetGrant(ctx, r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if g.User == p.ID {
		a.fail(w, r, fmt.Errorf("%w: %s held this grant and cannot review it", core.ErrPolicy, p.ID))
		return
	}
	now := a.now().UTC()
	if g.ActiveAt(now) {
		a.fail(w, r, fmt.Errorf("%w: grant %s is still active; review it once it ends", store.ErrConflict, g.ID))
		return
	}
	if g.Review != nil {
		a.fail(w, r, fmt.Errorf("%w: grant %s was already reviewed by %s", store.ErrConflict, g.ID, g.Review.Reviewer))
		return
	}
	act, err := a.Meta.ListGrantActivity(ctx, g.ID, 1000)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	overdue := g.OverdueAt(now)
	g.Review = &core.GrantReview{Reviewer: p.ID, Outcome: in.Outcome, Note: in.Note, At: now}
	entry, err := a.audit(r, "BreakGlassReviewed", map[string]any{
		"grantId": g.ID, "user": g.User, "reviewer": p.ID, "outcome": in.Outcome, "note": in.Note,
		"workItem": g.WorkItem, "actions": len(act), "overdue": overdue,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.ReviewGrant(ctx, g, entry); err != nil {
		if errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrConflict) {
			err = fmt.Errorf("%w: grant %s was already reviewed", store.ErrConflict, g.ID)
		}
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.view(g))
}

// grantLedger returns the break-glass audit chain, for
// `strata breakglass audit --verify`.
func (a *API) grantLedger(w http.ResponseWriter, r *http.Request) {
	if err := a.canSeeGrants(r); err != nil {
		a.fail(w, r, err)
		return
	}
	recs, err := a.Meta.ListAudit(r.Context(), core.BreakGlassChain, 0, 100_000)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, recs)
}

// grantLedgerAnchors returns the anchors covering the break-glass ledger
// (GET /break-glass/ledger/anchors), with the same visibility as the ledger.
func (a *API) grantLedgerAnchors(w http.ResponseWriter, r *http.Request) {
	if err := a.canSeeGrants(r); err != nil {
		a.fail(w, r, err)
		return
	}
	a.anchorPage(w, r, core.BreakGlassChain)
}
