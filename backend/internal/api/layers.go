package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/layers"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

const (
	maxInlineFile  = 256 << 10
	maxInlineTotal = 4 << 20
	maxFleet       = 1000
)

// checkExtends validates the Layer a repository wants to inherit from: it
// must exist, be a Layer, and not create a cycle or a too-deep chain.
func (a *API) checkExtends(ctx context.Context, rp core.Repo) error {
	if rp.Extends == nil {
		return nil
	}
	if _, err := core.ParseLayerRef(rp.Extends.String()); err != nil {
		return err
	}
	_, err := a.layers.Chain(ctx, rp)
	return err
}

// checkLocked rejects a snapshot that contains files a Layer locked.
func (a *API) checkLocked(ctx context.Context, rp core.Repo, tree core.Hash) error {
	if rp.Extends == nil {
		return nil
	}
	eff, err := a.layers.Resolve(ctx, rp, "")
	if err != nil {
		return err
	}
	locked := eff.Locked()
	if len(locked) == 0 {
		return nil
	}
	v, err := a.layers.Collisions(ctx, rp.Name, tree, locked)
	if err != nil || len(v) == 0 {
		return err
	}
	var paths []string
	for _, x := range v {
		paths = append(paths, fmt.Sprintf("%s (locked by %s)", x.Path, x.LockedBy))
	}
	return fmt.Errorf("%w: this change contains files a layer locked: %s. Remove them; they are inherited", core.ErrPolicy, strings.Join(paths, ", "))
}

// updateSettings changes a repository's description or the Layer it
// extends: PUT /repos/{repo}/settings {"extends": {"layer","ref"} | null}.
func (a *API) updateSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var in struct {
		Description *string         `json:"description"`
		Extends     json.RawMessage `json:"extends"`
	}
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	previous := rp.Extends
	if in.Description != nil {
		if len(*in.Description) > 1024 {
			a.fail(w, r, badRequest("description longer than 1024 bytes"))
			return
		}
		rp.Description = *in.Description
	}
	if len(in.Extends) > 0 {
		if bytes.Equal(in.Extends, []byte("null")) {
			rp.Extends = nil
		} else {
			var l core.LayerRef
			if err := json.Unmarshal(in.Extends, &l); err != nil {
				a.fail(w, r, badRequest("extends: %v", err))
				return
			}
			if l.Ref == "" {
				l.Ref = "main"
			}
			rp.Extends = &l
		}
	}
	if err := a.checkExtends(ctx, rp); err != nil {
		a.fail(w, r, err)
		return
	}
	rp.UpdatedBy, rp.UpdatedAt = identity(r), a.now().UTC()
	entry, err := a.audit(r, "RepoSettingsChanged", map[string]any{"repo": rp.Name, "settings": rp, "previousExtends": previous})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.Meta.UpdateRepo(ctx, rp, previous, entry); err != nil {
		a.fail(w, r, err)
		return
	}

	// Tell the caller now if the current main already holds files the new
	// Layer locks: the next push will be rejected until they are removed.
	violations := []layers.Violation{}
	if rp.Extends != nil {
		if eff, err := a.layers.Resolve(ctx, rp, "main"); err == nil {
			for _, v := range eff.Violations {
				if v.Source == rp.Name {
					violations = append(violations, v)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": rp, "violations": violations})
}

// effectiveLayers returns the resolved configuration of a repository:
// GET /repos/{repo}/layers?ref=main&content=true. ref="" (or own=false)
// leaves the repository's own files out. content=true inlines small text
// files, so tools and AI agents can read their rules in one call.
func (a *API) effectiveLayers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	ref := q.Get("ref")
	if ref == "" {
		ref = "main"
	}
	if q.Get("own") == "false" {
		ref = ""
	} else if err := core.ValidateRefName(ref); err != nil {
		a.fail(w, r, err)
		return
	}
	eff, err := a.layers.Resolve(ctx, rp, ref)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if q.Get("content") == "true" {
		total := 0
		for i, f := range eff.Files {
			var size int64
			for _, p := range f.Parts {
				size += p.Size
			}
			if size > maxInlineFile || total+int(size) > maxInlineTotal {
				continue
			}
			b, err := a.layers.Render(ctx, f)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
				continue // binary: fetch the parts instead
			}
			s := string(b)
			eff.Files[i].Content = &s
			total += len(b)
		}
	}
	writeJSON(w, http.StatusOK, eff)
}

// Subscriber is one repository in a Layer's fleet.
type Subscriber struct {
	Repo  core.Repo `json:"repo"`
	Via   string    `json:"via"`   // the Layer it extends directly
	Depth int       `json:"depth"` // 1 = extends this Layer directly
}

// subscribers lists every repository that inherits from a Layer, directly
// or through other Layers: GET /repos/{repo}/subscribers.
func (a *API) subscribers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rp, err := a.repo(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if rp.Kind != core.KindLayer {
		a.fail(w, r, badRequest("%s is not a layer", rp.Name))
		return
	}
	out := []Subscriber{}
	type item struct {
		layer string
		depth int
	}
	queue := []item{{rp.Name, 1}}
	seen := map[string]bool{rp.Name: true}
	for len(queue) > 0 && len(out) < maxFleet {
		cur := queue[0]
		queue = queue[1:]
		subs, err := a.Meta.ListSubscribers(ctx, cur.layer)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			a.fail(w, r, err)
			return
		}
		for _, s := range subs {
			if seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			out = append(out, Subscriber{Repo: s, Via: cur.layer, Depth: cur.depth})
			if s.Kind == core.KindLayer && cur.depth < layers.MaxDepth {
				queue = append(queue, item{s.Name, cur.depth + 1})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"layer": rp.Name, "subscribers": out})
}
