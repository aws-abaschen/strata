// Package authz decides who may do what, with Cedar policies that live in a
// Strata repository (default "strata-policies"): permissions are versioned,
// reviewed and audited like code.
//
// Entities:
//
//	User::"<email>"     parents: Group::"<group>" for each identity group
//	Repo::"<name>"      attrs: name, kind ("code"|"layer"); parent: the Layer it
//	                    extends, so `resource in Repo::"payments-rules"` covers
//	                    every repo inheriting from payments-rules
//	Action::"read" | "write" | "annotate" | "admin" | "create"
//
// Until the policy repository has a change on main, only members of
// Group::"strata-admins" are allowed, so they can install the first policies
// (every signed-in user with OpenBootstrap, dev and tests only). Once
// policies exist, administrators have no standing access: they activate a
// time-boxed break-glass grant (see Grant), which overrides a deny in code,
// not in Cedar, so no policy (however broken) can lock everyone out.
package authz

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cedar-policy/cedar-go"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// Action is what a request wants to do to a repository.
type Action string

const (
	Read     Action = "read"     // clone, log, browse, layers, audit
	Write    Action = "write"    // push changes, move refs
	Annotate Action = "annotate" // add work items, labels, notes
	Admin    Action = "admin"    // change settings (the Layer it extends)
	Create   Action = "create"   // create the repository
	Approve  Action = "approve"  // sign off on a change (never one's own)
	// Audit reads the break-glass grants and their ledger, on the resource
	// Repo::"_breakglass" (administrators always may).
	Audit Action = "audit"
)

// DefaultPolicyRepo holds the policies unless configured otherwise.
const DefaultPolicyRepo = "strata-policies"

// AdminGroup is always allowed unless a policy forbids it.
const AdminGroup = "strata-admins"

const (
	bootstrapPolicy      = `permit (principal, action, resource);`
	adminBootstrapPolicy = `permit (principal in Group::"strata-admins", action, resource);`
	maxPolicyBytes       = 1 << 20
	maxPolicyFiles       = 200
)

// Principal is the caller.
type Principal struct {
	ID     string
	Groups []string
	// AuthTime is when the caller last signed in (the token's auth_time), zero
	// if unknown.
	AuthTime time.Time
}

// IsAdmin reports whether the caller is in AdminGroup.
func (p Principal) IsAdmin() bool { return slices.Contains(p.Groups, AdminGroup) }

// Authorizer evaluates requests. It caches the policy set per revision of
// the policy repository's main.
type Authorizer struct {
	Meta       store.Meta
	Objects    store.Objects
	PolicyRepo string
	// OpenBootstrap lets every signed-in user do everything until the policy
	// repository has a change on main. Off in production: until policies
	// exist, only members of strata-admins are allowed. Only the dev server
	// and tests turn it on; it is deliberately not read from the environment.
	OpenBootstrap bool
	Now           func() time.Time

	mu        sync.Mutex
	cachedRev core.Hash
	cached    *cedar.PolicySet
}

func (a *Authorizer) repoName() string {
	if a.PolicyRepo == "" {
		return DefaultPolicyRepo
	}
	return a.PolicyRepo
}

// IsPolicyRepo reports whether repo holds the policies.
func (a *Authorizer) IsPolicyRepo(repo string) bool { return repo == a.repoName() }

// ParsePolicies parses .cedar files (path -> content) into one policy set.
// Policy ids are "<path>#<n>", so a decision names the file it came from.
func ParsePolicies(files map[string][]byte) (*cedar.PolicySet, error) {
	ps := cedar.NewPolicySet()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		one, err := cedar.NewPolicySetFromBytes(n, files[n])
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", core.ErrInvalid, n, err)
		}
		i := 0
		for _, p := range one.All() {
			ps.Add(cedar.PolicyID(fmt.Sprintf("%s#%d", n, i)), p)
			i++
		}
	}
	return ps, nil
}

func builtin(ps *cedar.PolicySet, id, src string) {
	one, err := cedar.NewPolicySetFromBytes(id, []byte(src))
	if err != nil {
		panic(err)
	}
	for _, p := range one.All() {
		ps.Add(cedar.PolicyID(id), p)
	}
}

// PolicyFiles reads every *.cedar file of a tree.
func PolicyFiles(ctx context.Context, objs store.Objects, repo string, root core.Hash) (map[string][]byte, error) {
	files := map[string][]byte{}
	var walk func(h core.Hash, prefix string) error
	walk = func(h core.Hash, prefix string) error {
		b, err := objs.Get(ctx, repo, h, 16<<20)
		if err != nil {
			return err
		}
		t, err := core.DecodeTree(b, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := path.Join(prefix, e.Name)
			switch {
			case e.Kind == core.KindDir:
				if err := walk(e.Hash, p); err != nil {
					return err
				}
			case strings.HasSuffix(e.Name, ".cedar"):
				if len(files) == maxPolicyFiles {
					return fmt.Errorf("%w: more than %d policy files", core.ErrInvalid, maxPolicyFiles)
				}
				c, err := objs.Get(ctx, repo, e.Hash, maxPolicyBytes)
				if err != nil {
					return err
				}
				files[p] = c
			}
		}
		return nil
	}
	return files, walk(root, "")
}

// Validate checks the policies a new tree of the policy repository would
// install, so a bad push cannot lock everyone out.
func (a *Authorizer) Validate(ctx context.Context, tree core.Hash) error {
	files, err := PolicyFiles(ctx, a.Objects, a.repoName(), tree)
	if err != nil {
		return err
	}
	_, err = ParsePolicies(files)
	return err
}

// Policies returns the active policy set and the revision it came from (""
// in bootstrap mode).
func (a *Authorizer) Policies(ctx context.Context) (*cedar.PolicySet, core.Hash, error) {
	repo := a.repoName()
	ref, err := a.Meta.GetRef(ctx, repo, "main")
	var rev core.Hash
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, "", err
	default:
		rev = ref.Target
	}
	a.mu.Lock()
	if a.cached != nil && a.cachedRev == rev {
		ps := a.cached
		a.mu.Unlock()
		return ps, rev, nil
	}
	a.mu.Unlock()

	var ps *cedar.PolicySet
	if rev == "" {
		ps = cedar.NewPolicySet()
		if a.OpenBootstrap {
			builtin(ps, "bootstrap", bootstrapPolicy)
		}
	} else {
		c, err := a.Meta.GetChange(ctx, repo, rev)
		if err != nil {
			return nil, "", err
		}
		files, err := PolicyFiles(ctx, a.Objects, repo, c.Tree)
		if err != nil {
			return nil, "", err
		}
		if ps, err = ParsePolicies(files); err != nil {
			return nil, "", err
		}
	}
	if rev == "" {
		builtin(ps, "admin-bootstrap", adminBootstrapPolicy)
	}
	a.mu.Lock()
	a.cached, a.cachedRev = ps, rev
	a.mu.Unlock()
	return ps, rev, nil
}

func repoUID(name string) cedar.EntityUID { return cedar.NewEntityUID("Repo", cedar.String(name)) }

// entities builds the principal and the repository with its Layer chain.
func (a *Authorizer) entities(ctx context.Context, p Principal, repo core.Repo) (cedar.EntityMap, error) {
	em := cedar.EntityMap{}
	var groups []cedar.EntityUID
	for _, g := range p.Groups {
		uid := cedar.NewEntityUID("Group", cedar.String(g))
		groups = append(groups, uid)
		em[uid] = cedar.Entity{UID: uid}
	}
	user := cedar.NewEntityUID("User", cedar.String(p.ID))
	em[user] = cedar.Entity{UID: user, Parents: cedar.NewEntityUIDSet(groups...),
		Attributes: cedar.NewRecord(cedar.RecordMap{"email": cedar.String(p.ID)})}

	cur := repo
	for depth := 0; depth <= 8; depth++ {
		uid := repoUID(cur.Name)
		if _, seen := em[uid]; seen {
			break
		}
		var parents []cedar.EntityUID
		if cur.Extends != nil {
			parents = append(parents, repoUID(cur.Extends.Layer))
		}
		kind := cur.Kind
		if kind == "" {
			kind = core.KindCode
		}
		em[uid] = cedar.Entity{UID: uid, Parents: cedar.NewEntityUIDSet(parents...), Attributes: cedar.NewRecord(cedar.RecordMap{
			"name": cedar.String(cur.Name), "kind": cedar.String(string(kind)),
		})}
		if cur.Extends == nil {
			break
		}
		next, err := a.Meta.GetRepo(ctx, cur.Extends.Layer)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return em, nil
}

// Decision explains an outcome.
type Decision struct {
	Allowed bool
	// Policies that determined the outcome (permits, or forbids on a deny).
	Policies []string
	// BreakGlass is the grant that allowed a request policy denied.
	BreakGlass string
}

// Facts are what the server computed about a ref move, exposed to policies
// as `context`. Every request carries all of them, with neutral defaults, so
// a policy never fails on a missing attribute.
type Facts struct {
	Ref         string // ref being moved; "" if the request moves none
	FastForward bool   // the new target descends from the current one
	Approvals   int    // distinct approvers of the target, none of them an author in the range
	HasWorkItem bool   // the change links at least one work item
}

func (f Facts) record() cedar.Record {
	return cedar.NewRecord(cedar.RecordMap{
		"ref": cedar.String(f.Ref), "fastForward": cedar.Boolean(f.FastForward),
		"approvals": cedar.Long(f.Approvals), "hasWorkItem": cedar.Boolean(f.HasWorkItem),
	})
}

// Check evaluates one request that moves no ref.
func (a *Authorizer) Check(ctx context.Context, p Principal, action Action, repo core.Repo) (Decision, error) {
	return a.CheckWith(ctx, p, action, repo, Facts{FastForward: true})
}

// CheckWith evaluates one request with facts about the ref move.
func (a *Authorizer) CheckWith(ctx context.Context, p Principal, action Action, repo core.Repo, f Facts) (Decision, error) {
	ps, _, err := a.Policies(ctx)
	if err != nil {
		return Decision{}, err
	}
	em, err := a.entities(ctx, p, repo)
	if err != nil {
		return Decision{}, err
	}
	ok, diag := cedar.Authorize(ps, em, cedar.Request{
		Principal: cedar.NewEntityUID("User", cedar.String(p.ID)),
		Action:    cedar.NewEntityUID("Action", cedar.String(action)),
		Resource:  repoUID(repo.Name),
		Context:   f.record(),
	})
	d := Decision{Allowed: ok == cedar.Allow}
	if len(diag.Errors) > 0 {
		// A forbid that errors is skipped by Cedar, which would fail open.
		d.Allowed = false
		for _, e := range diag.Errors {
			d.Policies = append(d.Policies, string(e.PolicyID)+" (evaluation error)")
		}
	} else {
		for _, r := range diag.Reasons {
			d.Policies = append(d.Policies, string(r.PolicyID))
		}
	}
	if d.Allowed {
		return d, nil
	}
	// Looked up only after a deny, so a grant tags exactly the requests it
	// let through.
	g, err := a.grantFor(ctx, p, repo.Name)
	if err != nil || g == nil {
		return d, err
	}
	if t := TraceFrom(ctx); t != nil {
		t.mu.Lock()
		t.used = g.ID
		t.mu.Unlock()
	}
	return Decision{Allowed: true, Policies: []string{"break-glass:" + g.ID}, BreakGlass: g.ID}, nil
}

func (a *Authorizer) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// ActiveGrant returns the caller's grant in force now, or nil. Group
// membership is checked on every request, so leaving strata-admins ends the
// grant's effect at once.
func (a *Authorizer) ActiveGrant(ctx context.Context, p Principal) (*core.Grant, error) {
	if !p.IsAdmin() {
		return nil, nil
	}
	t := TraceFrom(ctx)
	if t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.loaded {
			return t.grant, nil
		}
	}
	var out *core.Grant
	g, err := a.Meta.ActiveGrant(ctx, p.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	case g.User == p.ID && g.ActiveAt(a.now()):
		out = &g
	}
	if t != nil {
		t.loaded, t.grant = true, out
	}
	return out, nil
}

func (a *Authorizer) grantFor(ctx context.Context, p Principal, repo string) (*core.Grant, error) {
	g, err := a.ActiveGrant(ctx, p)
	if err != nil || g == nil || !g.Covers(repo) {
		return nil, err
	}
	return g, nil
}

type traceKey struct{}

// Trace collects, per request, what authorization used: the grant lookup
// (done once) and the grant id if one let a request through.
type Trace struct {
	mu     sync.Mutex
	loaded bool
	grant  *core.Grant
	used   string
}

// WithTrace returns a context carrying a fresh Trace.
func WithTrace(ctx context.Context) context.Context {
	return context.WithValue(ctx, traceKey{}, &Trace{})
}

// TraceFrom returns the request's Trace, or nil.
func TraceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// BreakGlass returns the grant id that let a request through, or "".
func (t *Trace) BreakGlass() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.used
}

// Require returns a core.ErrPolicy error unless the request is allowed.
func (a *Authorizer) Require(ctx context.Context, p Principal, action Action, repo core.Repo) error {
	return a.RequireWith(ctx, p, action, repo, Facts{FastForward: true})
}

// RequireWith is Require with facts about a ref move.
func (a *Authorizer) RequireWith(ctx context.Context, p Principal, action Action, repo core.Repo, f Facts) error {
	d, err := a.CheckWith(ctx, p, action, repo, f)
	if err != nil {
		return err
	}
	if !d.Allowed {
		why := "no policy permits it"
		if len(d.Policies) > 0 {
			why = "forbidden by " + strings.Join(d.Policies, ", ")
		}
		return fmt.Errorf("%w: %s may not %s %s (%s)", core.ErrPolicy, p.ID, action, repo.Name, why)
	}
	return nil
}

// Mode describes how requests are being authorized, for /me and operators.
func (a *Authorizer) Mode(ctx context.Context) (string, error) {
	_, rev, err := a.Policies(ctx)
	switch {
	case err != nil:
		return "", err
	case rev != "":
		return "policies", nil
	case a.OpenBootstrap:
		return "open-bootstrap", nil
	}
	return "not-configured", nil
}
