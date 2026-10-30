// Package api is Strata's HTTP API. It runs in Lambda behind API Gateway
// (see lambda.go) and in tests against the in-memory store.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/layers"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// Limits.
const (
	maxTreeBytes     = 16 << 20
	maxObjectBytes   = 5 << 30 // single S3 PUT
	maxNegotiate     = 1000
	maxTreeEntries   = 200_000
	checkConcurrency = 16
	defaultLimit     = 50
	maxLimit         = 200
)

// API serves the HTTP endpoints.
type API struct {
	Objects store.Objects
	layers  *layers.Resolver
	Meta    store.Meta
	Events  store.Events
	// WebURL is the web console, used for links in events (Jira, Slack...).
	WebURL string
	Now    func() time.Time
	Log    *slog.Logger
}

type ctxKey struct{}

// WithIdentity returns a context carrying the caller's identity. The Lambda
// adapter sets it from the verified JWT; the API never trusts a client-sent
// author.
func WithIdentity(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

func identity(r *http.Request) string {
	id, _ := r.Context().Value(ctxKey{}).(string)
	return id
}

func (a *API) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *API) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Handler returns the routed API.
func (a *API) Handler() http.Handler {
	if a.Events == nil {
		a.Events = store.NoEvents{}
	}
	a.layers = &layers.Resolver{Meta: a.Meta, Objects: a.Objects}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me", a.me)
	mux.HandleFunc("GET /repos", a.listRepos)
	mux.HandleFunc("POST /repos", a.createRepo)
	mux.HandleFunc("GET /repos/{repo}", a.getRepo)
	mux.HandleFunc("GET /repos/{repo}/refs", a.listRefs)
	mux.HandleFunc("PUT /repos/{repo}/refs/{ref...}", a.updateRef)
	mux.HandleFunc("POST /repos/{repo}/objects/negotiate", a.negotiate)
	mux.HandleFunc("GET /repos/{repo}/objects/{hash}", a.objectURL)
	mux.HandleFunc("GET /repos/{repo}/tree/{rev}", a.tree)
	mux.HandleFunc("GET /repos/{repo}/log", a.history)
	mux.HandleFunc("POST /repos/{repo}/changes", a.createChange)
	mux.HandleFunc("GET /repos/{repo}/changes/{rev}", a.getChange)
	mux.HandleFunc("POST /repos/{repo}/changes/{rev}/annotations", a.annotate)
	mux.HandleFunc("GET /work-items/{system}/{key}", a.findWorkItem)
	mux.HandleFunc("GET /labels/{label...}", a.findLabel)
	mux.HandleFunc("PUT /repos/{repo}/settings", a.updateSettings)
	mux.HandleFunc("GET /repos/{repo}/layers", a.effectiveLayers)
	mux.HandleFunc("GET /repos/{repo}/subscribers", a.subscribers)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if identity(r) == "" {
			writeJSON(w, http.StatusUnauthorized, errBody{"unauthenticated"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type errBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, errBody{err.Error()})
	case errors.Is(err, core.ErrPolicy):
		writeJSON(w, http.StatusForbidden, errBody{err.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, errBody{err.Error()})
	case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrConflict):
		writeJSON(w, http.StatusConflict, errBody{err.Error()})
	default:
		a.log().ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody{"internal error"})
	}
}

func badRequest(format string, a ...any) error {
	return fmt.Errorf("%w: %s", core.ErrInvalid, fmt.Sprintf(format, a...))
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("body: %v", err)
	}
	return nil
}

func limit(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return defaultLimit
	}
	return min(n, maxLimit)
}

// repo loads the {repo} path value and checks it exists.
func (a *API) repo(r *http.Request) (core.Repo, error) {
	name := r.PathValue("repo")
	if err := core.ValidateRepoName(name); err != nil {
		return core.Repo{}, err
	}
	rp, err := a.Meta.GetRepo(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		return core.Repo{}, fmt.Errorf("repository %q: %w", name, store.ErrNotFound)
	}
	return rp, err
}

func hashParam(r *http.Request, name string) (core.Hash, error) {
	h := core.Hash(r.PathValue(name))
	if !h.Valid() {
		return "", badRequest("%s must be a 64-character hex hash", name)
	}
	return h, nil
}

func (a *API) publish(ctx context.Context, detailType string, detail any) {
	// Best effort for now. Next step: DynamoDB Streams -> EventBridge Pipes,
	// so events are delivered even if this call fails.
	if err := a.Events.Publish(ctx, detailType, detail); err != nil {
		a.log().ErrorContext(ctx, "publish event", "type", detailType, "err", err)
	}
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"identity": identity(r)})
}

func (a *API) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := a.Meta.ListRepos(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

func (a *API) createRepo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Kind        core.RepoKind  `json:"kind"`
		Extends     *core.LayerRef `json:"extends"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := core.ValidateRepoName(in.Name); err != nil {
		a.fail(w, r, err)
		return
	}
	if len(in.Description) > 1024 {
		a.fail(w, r, badRequest("description longer than 1024 bytes"))
		return
	}
	if err := in.Kind.Validate(); err != nil {
		a.fail(w, r, err)
		return
	}
	rp := core.Repo{Name: in.Name, Description: in.Description, Kind: in.Kind, Extends: in.Extends, CreatedBy: identity(r), CreatedAt: a.now().UTC()}
	if err := a.checkExtends(r.Context(), rp); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.CreateRepo(r.Context(), rp); err != nil {
		a.fail(w, r, err)
		return
	}
	a.publish(r.Context(), "RepoCreated", rp)
	writeJSON(w, http.StatusCreated, rp)
}

func (a *API) getRepo(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rp)
}

func (a *API) listRefs(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	refs, err := a.Meta.ListRefs(r.Context(), rp.Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"refs": refs})
}

// updateRef moves, creates or deletes a ref with compare-and-swap.
func (a *API) updateRef(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	name := r.PathValue("ref")
	if err := core.ValidateRefName(name); err != nil {
		a.fail(w, r, err)
		return
	}
	var in struct {
		Target   core.Hash `json:"target"`
		Expected core.Hash `json:"expected"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if in.Target != "" {
		if _, err := a.Meta.GetChange(r.Context(), rp.Name, in.Target); err != nil {
			a.fail(w, r, fmt.Errorf("target revision: %w", err))
			return
		}
	}
	ref := core.Ref{Name: name, Target: in.Target, UpdatedBy: identity(r), UpdatedAt: a.now().UTC()}
	if err := a.Meta.UpdateRef(r.Context(), rp.Name, ref, in.Expected); err != nil {
		a.fail(w, r, err)
		return
	}
	a.publish(r.Context(), "RefUpdated", map[string]any{"repo": rp.Name, "ref": ref, "previous": in.Expected})
	writeJSON(w, http.StatusOK, ref)
}

// negotiate takes the objects a client wants to push and returns upload URLs
// for the ones the repository does not have yet.
func (a *API) negotiate(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var in struct {
		Objects []struct {
			Hash core.Hash `json:"hash"`
			Size int64     `json:"size"`
		} `json:"objects"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if len(in.Objects) > maxNegotiate {
		a.fail(w, r, badRequest("at most %d objects per call", maxNegotiate))
		return
	}
	for _, o := range in.Objects {
		if !o.Hash.Valid() || o.Size < 0 || o.Size > maxObjectBytes {
			a.fail(w, r, badRequest("object %q: bad hash or size", o.Hash))
			return
		}
	}
	uploads := make([]*store.Upload, len(in.Objects))
	err = forEach(r.Context(), len(in.Objects), func(ctx context.Context, i int) error {
		o := in.Objects[i]
		ok, err := a.Objects.Exists(ctx, rp.Name, o.Hash)
		if err != nil || ok {
			return err
		}
		u, err := a.Objects.PresignPut(ctx, rp.Name, o.Hash, o.Size)
		uploads[i] = &u
		return err
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := []store.Upload{}
	for _, u := range uploads {
		if u != nil {
			out = append(out, *u)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": out})
}

func (a *API) objectURL(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	h, err := hashParam(r, "hash")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	u, err := a.Objects.PresignGet(r.Context(), rp.Name, h)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"hash": string(h), "url": u})
}

func (a *API) loadTree(ctx context.Context, repo string, h core.Hash) (core.Tree, error) {
	b, err := a.Objects.Get(ctx, repo, h, maxTreeBytes)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return core.Tree{}, badRequest("tree %s is missing", h.Short())
		}
		return core.Tree{}, err
	}
	return core.DecodeTree(b, h)
}

// tree lists a directory at a revision: GET /repos/{repo}/tree/{rev}?path=a/b.
// A path to a file returns the file entry and a download URL.
func (a *API) tree(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rev, err := hashParam(r, "rev")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	c, err := a.Meta.GetChange(r.Context(), rp.Name, rev)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	path := strings.Trim(r.URL.Query().Get("path"), "/")
	cur := c.Tree
	var parts []string
	if path != "" {
		parts = strings.Split(path, "/")
	}
	for i, p := range parts {
		t, err := a.loadTree(r.Context(), rp.Name, cur)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		var found *core.TreeEntry
		for _, e := range t.Entries {
			if e.Name == p {
				found = &e
				break
			}
		}
		if found == nil {
			a.fail(w, r, fmt.Errorf("path %q: %w", path, store.ErrNotFound))
			return
		}
		if found.Kind != core.KindDir {
			if i != len(parts)-1 {
				a.fail(w, r, fmt.Errorf("path %q: %w", path, store.ErrNotFound))
				return
			}
			u, err := a.Objects.PresignGet(r.Context(), rp.Name, found.Hash)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"path": path, "file": found, "url": u})
			return
		}
		cur = found.Hash
	}
	t, err := a.loadTree(r.Context(), rp.Name, cur)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "tree": cur, "entries": t.Entries})
}

// checkClosure verifies that every object a tree references exists, so a ref
// can never point at an incomplete snapshot.
func (a *API) checkClosure(ctx context.Context, repo string, root core.Hash) error {
	var (
		mu      sync.Mutex
		seen    = map[core.Hash]bool{}
		entries int
	)
	level := []core.Hash{root}
	for len(level) > 0 {
		var next []core.Hash
		trees := make([]core.Tree, len(level))
		err := forEach(ctx, len(level), func(ctx context.Context, i int) error {
			t, err := a.loadTree(ctx, repo, level[i])
			trees[i] = t
			return err
		})
		if err != nil {
			return err
		}
		var files []core.Hash
		for _, t := range trees {
			for _, e := range t.Entries {
				entries++
				if seen[e.Hash] {
					continue
				}
				seen[e.Hash] = true
				if e.Kind == core.KindDir {
					next = append(next, e.Hash)
				} else {
					files = append(files, e.Hash)
				}
			}
		}
		if entries > maxTreeEntries {
			return badRequest("snapshot has more than %d entries", maxTreeEntries)
		}
		var missing []core.Hash
		err = forEach(ctx, len(files), func(ctx context.Context, i int) error {
			ok, err := a.Objects.Exists(ctx, repo, files[i])
			if err == nil && !ok {
				mu.Lock()
				missing = append(missing, files[i])
				mu.Unlock()
			}
			return err
		})
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return badRequest("%d objects are missing, e.g. %s: upload them first", len(missing), missing[0].Short())
		}
		level = next
	}
	return nil
}

// ChangeEvent is the detail of ChangeCreated and ChangeAnnotated events.
type ChangeEvent struct {
	core.ChangeSummary
	Ref      string        `json:"ref,omitempty"`
	Metadata core.Metadata `json:"metadata"`
	Note     string        `json:"note,omitempty"`
	URL      string        `json:"url,omitempty"`
}

func (a *API) changeURL(repo string, rev core.Hash) string {
	if a.WebURL == "" {
		return ""
	}
	return strings.TrimRight(a.WebURL, "/") + "/repos/" + repo + "/changes/" + string(rev)
}

type createChangeInput struct {
	// ChangeID amends an existing change: the new revision keeps its id.
	ChangeID string        `json:"changeId"`
	Tree     core.Hash     `json:"tree"`
	Parents  []core.Hash   `json:"parents"`
	Message  string        `json:"message"`
	Metadata core.Metadata `json:"metadata"`
	// Ref is moved to the new revision, with compare-and-swap on ExpectedRef.
	// ExpectedRef defaults to the first parent (a fast-forward), or "no ref
	// yet" for a root change.
	Ref         string     `json:"ref"`
	ExpectedRef *core.Hash `json:"expectedRef"`
}

func (a *API) createChange(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var in createChangeInput
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if !in.Tree.Valid() {
		a.fail(w, r, badRequest("tree hash"))
		return
	}
	if len(in.Parents) > 8 {
		a.fail(w, r, badRequest("at most 8 parents"))
		return
	}
	for _, p := range in.Parents {
		if !p.Valid() {
			a.fail(w, r, badRequest("parent hash %q", p))
			return
		}
		if _, err := a.Meta.GetChange(ctx, rp.Name, p); err != nil {
			a.fail(w, r, fmt.Errorf("parent %s: %w", p.Short(), err))
			return
		}
	}
	if err := a.checkClosure(ctx, rp.Name, in.Tree); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.checkLocked(ctx, rp, in.Tree); err != nil {
		a.fail(w, r, err)
		return
	}

	md := core.Metadata{WorkItems: core.ParseWorkItems(in.Message)}
	md.Merge(in.Metadata)
	c := core.Change{
		ChangeID: in.ChangeID, Repo: rp.Name, Tree: in.Tree, Parents: in.Parents,
		Author: identity(r), CreatedAt: a.now(), Message: in.Message, Metadata: md,
	}
	if c.ChangeID == "" {
		c.ChangeID = core.NewChangeID()
	} else {
		if !core.ValidChangeID(c.ChangeID) {
			a.fail(w, r, badRequest("change id %q", c.ChangeID))
			return
		}
		pred, err := a.Meta.LatestRevision(ctx, rp.Name, c.ChangeID)
		if err != nil {
			a.fail(w, r, fmt.Errorf("change %s: %w", c.ChangeID, err))
			return
		}
		c.Predecessor = pred
	}
	doc, err := c.Seal()
	if err != nil {
		a.fail(w, r, err)
		return
	}

	var ref *store.RefUpdate
	if in.Ref != "" {
		if err := core.ValidateRefName(in.Ref); err != nil {
			a.fail(w, r, err)
			return
		}
		ref = &store.RefUpdate{Name: in.Ref}
		switch {
		case in.ExpectedRef != nil:
			ref.Expected = *in.ExpectedRef
		case len(in.Parents) > 0:
			ref.Expected = in.Parents[0]
		}
	}

	// S3 first: the sealed document is the record of truth; the index entry
	// only becomes visible once it is safely stored.
	if err := a.Objects.PutChangeDoc(ctx, c, doc); err != nil && !errors.Is(err, store.ErrExists) {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.PutChange(ctx, c, ref); err != nil {
		if errors.Is(err, store.ErrConflict) && ref != nil {
			err = fmt.Errorf("%w: ref %q or change %s moved since you read it; pull and retry", store.ErrConflict, ref.Name, c.ChangeID)
		}
		a.fail(w, r, err)
		return
	}
	ev := ChangeEvent{ChangeSummary: c.Summary(), Metadata: c.Metadata, URL: a.changeURL(c.Repo, c.Revision)}
	if ref != nil {
		ev.Ref = ref.Name
	}
	a.publish(ctx, "ChangeCreated", ev)
	writeJSON(w, http.StatusCreated, c)
}

func (a *API) getChange(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rev, err := hashParam(r, "rev")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	c, err := a.Meta.GetChange(r.Context(), rp.Name, rev)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	notes, err := a.Meta.ListAnnotations(r.Context(), rp.Name, rev)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	latest, err := a.Meta.LatestRevision(r.Context(), rp.Name, c.ChangeID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// Effective metadata = the change's own plus every annotation's.
	eff := c.Metadata
	for _, n := range notes {
		eff.Merge(n.Metadata)
	}
	eff.Normalize()
	writeJSON(w, http.StatusOK, map[string]any{"change": c, "annotations": notes, "effectiveMetadata": eff, "latestRevision": latest})
}

func (a *API) annotate(w http.ResponseWriter, r *http.Request) {
	rp, err := a.repo(r)
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
		Note     string        `json:"note"`
		Metadata core.Metadata `json:"metadata"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	if len(in.Note) > 16*1024 {
		a.fail(w, r, badRequest("note longer than 16 KiB"))
		return
	}
	in.Metadata.Merge(core.Metadata{WorkItems: core.ParseWorkItems(in.Note)})
	if err := in.Metadata.Normalize(); err != nil {
		a.fail(w, r, err)
		return
	}
	if in.Note == "" && len(in.Metadata.WorkItems)+len(in.Metadata.Labels)+len(in.Metadata.Attributes) == 0 {
		a.fail(w, r, badRequest("an annotation needs a note or metadata"))
		return
	}
	n := core.Annotation{Repo: rp.Name, Revision: rev, Author: identity(r), CreatedAt: a.now().UTC(), Note: in.Note, Metadata: in.Metadata}
	if err := a.Meta.PutAnnotation(r.Context(), n); err != nil {
		a.fail(w, r, err)
		return
	}
	if c, err := a.Meta.GetChange(r.Context(), rp.Name, rev); err == nil {
		s := c.Summary()
		s.Via = "annotation"
		a.publish(r.Context(), "ChangeAnnotated", ChangeEvent{ChangeSummary: s, Metadata: n.Metadata, Note: n.Note, URL: a.changeURL(rp.Name, rev)})
	}
	writeJSON(w, http.StatusCreated, n)
}

// history follows first parents from a ref or revision:
// GET /repos/{repo}/log?ref=main or ?rev=<hash>.
func (a *API) history(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	start := core.Hash(q.Get("rev"))
	if start == "" {
		name := q.Get("ref")
		if name == "" {
			name = "main"
		}
		ref, err := a.Meta.GetRef(ctx, rp.Name, name)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusOK, map[string]any{"changes": []core.Change{}})
			return
		}
		if err != nil {
			a.fail(w, r, err)
			return
		}
		start = ref.Target
	}
	if !start.Valid() {
		a.fail(w, r, badRequest("rev"))
		return
	}
	n := limit(r)
	out := []core.Change{}
	for cur := start; cur != "" && len(out) < n; {
		c, err := a.Meta.GetChange(ctx, rp.Name, cur)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		out = append(out, c)
		cur = ""
		if len(c.Parents) > 0 {
			cur = c.Parents[0]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"changes": out})
}

func dedupe(in []core.ChangeSummary) []core.ChangeSummary {
	seen := map[string]bool{}
	out := []core.ChangeSummary{}
	for _, s := range in {
		k := s.Repo + "/" + string(s.Revision)
		if !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

func (a *API) findWorkItem(w http.ResponseWriter, r *http.Request) {
	wi, err := core.ParseWorkItem(r.PathValue("system") + ":" + r.PathValue("key"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	md := core.Metadata{WorkItems: []core.WorkItem{wi}}
	md.Normalize()
	wi = md.WorkItems[0]
	res, err := a.Meta.FindByWorkItem(r.Context(), wi.System, wi.Key, limit(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workItem": wi, "changes": dedupe(res)})
}

func (a *API) findLabel(w http.ResponseWriter, r *http.Request) {
	md := core.Metadata{Labels: []string{r.PathValue("label")}}
	if err := md.Normalize(); err != nil {
		a.fail(w, r, err)
		return
	}
	res, err := a.Meta.FindByLabel(r.Context(), md.Labels[0], limit(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"label": md.Labels[0], "changes": dedupe(res)})
}

// forEach runs fn for 0..n-1 with bounded concurrency and returns the first error.
func forEach(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, checkConcurrency)
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for i := range n {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := fn(ctx, i); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}()
	}
	wg.Wait()
	return first
}
