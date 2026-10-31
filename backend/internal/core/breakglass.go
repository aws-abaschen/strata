package core

import (
	"slices"
	"time"
)

// BreakGlassChain is the audit chain that records break-glass grants. It is
// not a valid repository name, so it never collides with a repository.
const BreakGlassChain = "_breakglass"

// Break-glass limits.
const (
	BreakGlassDefault = time.Hour
	BreakGlassMax     = 4 * time.Hour
	// BreakGlassReviewDue is how long after a grant ends its review is overdue.
	BreakGlassReviewDue = 24 * time.Hour
)

// Review outcomes.
const (
	ReviewAccepted = "accepted"
	ReviewRejected = "rejected"
)

// Grant is a time-boxed emergency elevation: while active, its holder may do
// on the listed repositories what policy would otherwise deny. Every grant
// is reviewed afterwards by another administrator.
type Grant struct {
	ID        string       `json:"id"`
	User      string       `json:"user"`
	Reason    string       `json:"reason"`
	WorkItem  WorkItem     `json:"workItem"`
	Repos     []string     `json:"repos"` // "*" means every repository
	CreatedAt time.Time    `json:"createdAt"`
	ExpiresAt time.Time    `json:"expiresAt"`
	EndedAt   *time.Time   `json:"endedAt,omitempty"`
	Review    *GrantReview `json:"review,omitempty"`
}

// GrantReview is the after-the-fact review of a grant.
type GrantReview struct {
	Reviewer string    `json:"reviewer"`
	Outcome  string    `json:"outcome"`
	Note     string    `json:"note"`
	At       time.Time `json:"at"`
}

// ActiveAt reports whether the grant is in force at t.
func (g Grant) ActiveAt(t time.Time) bool { return g.EndedAt == nil && t.Before(g.ExpiresAt) }

// Until is when the grant stopped (or will stop) being in force.
func (g Grant) Until() time.Time {
	if g.EndedAt != nil && g.EndedAt.Before(g.ExpiresAt) {
		return *g.EndedAt
	}
	return g.ExpiresAt
}

// Covers reports whether the grant applies to a repository.
func (g Grant) Covers(repo string) bool {
	return slices.Contains(g.Repos, "*") || slices.Contains(g.Repos, repo)
}

// OverdueAt reports whether the grant is unreviewed past its review deadline.
func (g Grant) OverdueAt(t time.Time) bool {
	return g.Review == nil && t.After(g.Until().Add(BreakGlassReviewDue))
}
