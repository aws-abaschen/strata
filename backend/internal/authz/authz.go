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
// Until the policy repository has a change on main, every signed-in user is
// allowed (bootstrap). Members of Group::"strata-admins" are always allowed
// unless a policy explicitly forbids it (break-glass).
package authz

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"

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
)

// DefaultPolicyRepo holds the policies unless configured otherwise.
const DefaultPolicyRepo = "strata-policies"

// AdminGroup is always allowed unless a policy forbids it.
const AdminGroup = "strata-admins"

const (
	bootstrapPolicy  = `permit (principal, action, resource);`
	breakGlassPolicy = `permit (principal in Group::"strata-admins", action, resource);`
	maxPolicyBytes   = 1 << 20
	maxPolicyFiles   = 200
)

// Principal is the caller.
type Principal struct {
	ID     string
	Groups []string
}

// Authorizer evaluates requests. It caches the policy set per revision of
// the policy repository's main.
type Authorizer struct {
	Meta       store.Meta
	Objects    store.Objects
	PolicyRepo string

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
		builtin(ps, "bootstrap", bootstrapPolicy)
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
	builtin(ps, "break-glass", breakGlassPolicy)
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
}

// Check evaluates one request.
func (a *Authorizer) Check(ctx context.Context, p Principal, action Action, repo core.Repo) (Decision, error) {
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
		Context:   cedar.NewRecord(cedar.RecordMap{}),
	})
	d := Decision{Allowed: ok == cedar.Allow}
	for _, r := range diag.Reasons {
		d.Policies = append(d.Policies, string(r.PolicyID))
	}
	return d, nil
}

// Require returns a core.ErrPolicy error unless the request is allowed.
func (a *Authorizer) Require(ctx context.Context, p Principal, action Action, repo core.Repo) error {
	d, err := a.Check(ctx, p, action, repo)
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
