// Package layers resolves the top-down configuration cascade. A Layer is a
// repository of kind "layer"; any repository can extend one Layer at a ref,
// and Layers can extend Layers, forming a chain such as
// org-rules -> payments-rules -> team-cards-rules -> cards-api.
//
// Each Layer may hold a manifest, strata.layer.json, that sets how lower
// levels may change each of its files:
//
//	locked    lower levels cannot change it (pushes touching it are rejected)
//	override  lower levels may replace it (the default)
//	append    lower levels may add to it; contributions are concatenated
//
// Resolution runs from the top of the chain down. The repository itself is
// the last level.
package layers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// ManifestPath is the Layer manifest, at the root of a Layer repository.
const ManifestPath = "strata.layer.json"

// Limits.
const (
	MaxDepth        = 8
	MaxLayerFiles   = 5000
	MaxManifest     = 64 << 10
	MaxRules        = 200
	MaxRenderedFile = 1 << 20
)

// Mode says how lower levels may change a file.
type Mode string

const (
	Locked   Mode = "locked"
	Override Mode = "override"
	Append   Mode = "append"
)

func (m Mode) valid() bool { return m == Locked || m == Override || m == Append }

// Rule sets the mode of the files matching Path, a glob where * matches
// within one path segment and ** matches any number of segments.
type Rule struct {
	Path string `json:"path"`
	Mode Mode   `json:"mode"`
}

// Manifest is strata.layer.json.
type Manifest struct {
	Default Mode   `json:"default,omitempty"`
	Rules   []Rule `json:"rules,omitempty"`
}

// ParseManifest validates manifest bytes.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%w: %s: %v", core.ErrInvalid, ManifestPath, err)
	}
	if m.Default == "" {
		m.Default = Override
	}
	if !m.Default.valid() {
		return m, fmt.Errorf("%w: %s: default mode %q", core.ErrInvalid, ManifestPath, m.Default)
	}
	if len(m.Rules) > MaxRules {
		return m, fmt.Errorf("%w: %s: at most %d rules", core.ErrInvalid, ManifestPath, MaxRules)
	}
	for _, r := range m.Rules {
		if r.Path == "" || !r.Mode.valid() {
			return m, fmt.Errorf("%w: %s: rule %q mode %q", core.ErrInvalid, ManifestPath, r.Path, r.Mode)
		}
		if _, err := path.Match(strings.ReplaceAll(r.Path, "**", "*"), ""); err != nil {
			return m, fmt.Errorf("%w: %s: pattern %q: %v", core.ErrInvalid, ManifestPath, r.Path, err)
		}
	}
	return m, nil
}

// ModeFor returns the mode of p: the first matching rule, else the default.
func (m Manifest) ModeFor(p string) Mode {
	for _, r := range m.Rules {
		if Match(r.Path, p) {
			return r.Mode
		}
	}
	if m.Default == "" {
		return Override
	}
	return m.Default
}

// Match reports whether slash-separated path p matches pattern.
func Match(pattern, p string) bool {
	return matchSegs(strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// Part is one level's contribution to a file. Its bytes live in Source's
// object store.
type Part struct {
	Source   string    `json:"source"`
	Revision core.Hash `json:"revision,omitempty"`
	Hash     core.Hash `json:"hash"`
	Size     int64     `json:"size"`
}

// File is one inherited path and how it resolved.
type File struct {
	Path string `json:"path"`
	Mode Mode   `json:"mode"`
	// ModeSetBy is the level whose manifest set Mode.
	ModeSetBy string         `json:"modeSetBy"`
	Kind      core.EntryKind `json:"kind"`
	// Parts in order, top first. override/locked files have exactly one.
	Parts []Part `json:"parts"`
	// Content is the rendered text, when asked for and small enough.
	Content *string `json:"content,omitempty"`
}

// Link is one Layer in the chain.
type Link struct {
	Layer    string    `json:"layer"`
	Ref      string    `json:"ref"`
	Revision core.Hash `json:"revision,omitempty"`
	// Missing means the ref does not exist yet: the Layer adds nothing.
	Missing bool `json:"missing,omitempty"`
}

// Violation is a lower level that tried to change a locked file. Its
// contribution is ignored.
type Violation struct {
	Path     string `json:"path"`
	Source   string `json:"source"`
	LockedBy string `json:"lockedBy"`
}

// Effective is the resolved configuration of a repository.
type Effective struct {
	Repo       string      `json:"repo"`
	Chain      []Link      `json:"chain"`
	Files      []File      `json:"files"`
	Violations []Violation `json:"violations"`
}

// Locked returns locked path -> the level that locked it.
func (e Effective) Locked() map[string]string {
	out := map[string]string{}
	for _, f := range e.Files {
		if f.Mode == Locked {
			out[f.Path] = f.ModeSetBy
		}
	}
	return out
}

// Resolver reads Layers from the stores.
type Resolver struct {
	Meta    store.Meta
	Objects store.Objects
}

// Chain returns the Layers a repository inherits from, top first.
func (r *Resolver) Chain(ctx context.Context, repo core.Repo) ([]core.LayerRef, error) {
	var chain []core.LayerRef
	seen := map[string]bool{repo.Name: true}
	for cur := repo.Extends; cur != nil; {
		if seen[cur.Layer] {
			return nil, fmt.Errorf("%w: layer cycle through %q", core.ErrInvalid, cur.Layer)
		}
		if len(chain) == MaxDepth {
			return nil, fmt.Errorf("%w: layer chain deeper than %d", core.ErrInvalid, MaxDepth)
		}
		seen[cur.Layer] = true
		l, err := r.Meta.GetRepo(ctx, cur.Layer)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: layer %q does not exist", core.ErrInvalid, cur.Layer)
		}
		if err != nil {
			return nil, err
		}
		if l.Kind != core.KindLayer {
			return nil, fmt.Errorf("%w: %q is not a layer repository", core.ErrInvalid, cur.Layer)
		}
		chain = append(chain, *cur)
		cur = l.Extends
	}
	slices.Reverse(chain)
	return chain, nil
}

type walker struct {
	r     *Resolver
	trees map[string]core.Tree
}

func (w *walker) tree(ctx context.Context, repo string, h core.Hash) (core.Tree, error) {
	k := repo + "/" + string(h)
	if t, ok := w.trees[k]; ok {
		return t, nil
	}
	b, err := w.r.Objects.Get(ctx, repo, h, 16<<20)
	if err != nil {
		return core.Tree{}, fmt.Errorf("%s tree %s: %w", repo, h.Short(), err)
	}
	t, err := core.DecodeTree(b, h)
	if err != nil {
		return core.Tree{}, err
	}
	w.trees[k] = t
	return t, nil
}

// flatten lists every file under root as path -> entry.
func (w *walker) flatten(ctx context.Context, repo string, root core.Hash) (map[string]core.TreeEntry, error) {
	out := map[string]core.TreeEntry{}
	var walk func(h core.Hash, prefix string) error
	walk = func(h core.Hash, prefix string) error {
		t, err := w.tree(ctx, repo, h)
		if err != nil {
			return err
		}
		for _, e := range t.Entries {
			p := path.Join(prefix, e.Name)
			if e.Kind == core.KindDir {
				if err := walk(e.Hash, p); err != nil {
					return err
				}
				continue
			}
			out[p] = e
			if len(out) > MaxLayerFiles {
				return fmt.Errorf("%w: layer %s has more than %d files", core.ErrInvalid, repo, MaxLayerFiles)
			}
		}
		return nil
	}
	return out, walk(root, "")
}

// lookup finds a file in a tree, or returns nil.
func (w *walker) lookup(ctx context.Context, repo string, root core.Hash, p string) (*core.TreeEntry, error) {
	cur := root
	parts := strings.Split(p, "/")
	for i, name := range parts {
		t, err := w.tree(ctx, repo, cur)
		if err != nil {
			return nil, err
		}
		var found *core.TreeEntry
		for _, e := range t.Entries {
			if e.Name == name {
				found = &e
				break
			}
		}
		if found == nil {
			return nil, nil
		}
		if i == len(parts)-1 {
			if found.Kind == core.KindDir {
				return nil, nil
			}
			return found, nil
		}
		if found.Kind != core.KindDir {
			return nil, nil
		}
		cur = found.Hash
	}
	return nil, nil
}

// head returns the change a ref points at, or nil if the ref is missing.
func (r *Resolver) head(ctx context.Context, repo, ref string) (*core.Change, error) {
	rf, err := r.Meta.GetRef(ctx, repo, ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := r.Meta.GetChange(ctx, repo, rf.Target)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Resolve computes a repository's effective configuration. If ref is not
// empty, the repository's own files at that ref are applied as the last
// level; otherwise only the inherited Layers are resolved.
func (r *Resolver) Resolve(ctx context.Context, repo core.Repo, ref string) (Effective, error) {
	eff := Effective{Repo: repo.Name, Chain: []Link{}, Files: []File{}, Violations: []Violation{}}
	chain, err := r.Chain(ctx, repo)
	if err != nil {
		return eff, err
	}
	w := &walker{r: r, trees: map[string]core.Tree{}}
	files := map[string]*File{}

	for _, l := range chain {
		link := Link{Layer: l.Layer, Ref: l.Ref}
		c, err := r.head(ctx, l.Layer, l.Ref)
		if err != nil {
			return eff, err
		}
		if c == nil {
			link.Missing = true
			eff.Chain = append(eff.Chain, link)
			continue
		}
		link.Revision = c.Revision
		eff.Chain = append(eff.Chain, link)

		entries, err := w.flatten(ctx, l.Layer, c.Tree)
		if err != nil {
			return eff, err
		}
		man := Manifest{Default: Override}
		if me, ok := entries[ManifestPath]; ok {
			b, err := r.Objects.Get(ctx, l.Layer, me.Hash, MaxManifest)
			if err != nil {
				return eff, fmt.Errorf("layer %s: %w", l.Layer, err)
			}
			if man, err = ParseManifest(b); err != nil {
				return eff, fmt.Errorf("layer %s: %w", l.Layer, err)
			}
			delete(entries, ManifestPath)
		}
		for p, e := range entries {
			part := Part{Source: l.Layer, Revision: c.Revision, Hash: e.Hash, Size: e.Size}
			mode := man.ModeFor(p)
			f, ok := files[p]
			if !ok {
				files[p] = &File{Path: p, Mode: mode, ModeSetBy: l.Layer, Kind: e.Kind, Parts: []Part{part}}
				continue
			}
			switch f.Mode {
			case Locked:
				eff.Violations = append(eff.Violations, Violation{Path: p, Source: l.Layer, LockedBy: f.ModeSetBy})
			case Override:
				f.Parts, f.Mode, f.ModeSetBy, f.Kind = []Part{part}, mode, l.Layer, e.Kind
			case Append:
				f.Parts = append(f.Parts, part)
			}
		}
	}

	if ref != "" && len(files) > 0 {
		c, err := r.head(ctx, repo.Name, ref)
		if err != nil {
			return eff, err
		}
		if c != nil {
			for p, f := range files {
				e, err := w.lookup(ctx, repo.Name, c.Tree, p)
				if err != nil {
					return eff, err
				}
				if e == nil {
					continue
				}
				part := Part{Source: repo.Name, Revision: c.Revision, Hash: e.Hash, Size: e.Size}
				switch f.Mode {
				case Locked:
					eff.Violations = append(eff.Violations, Violation{Path: p, Source: repo.Name, LockedBy: f.ModeSetBy})
				case Override:
					f.Parts = []Part{part}
				case Append:
					f.Parts = append(f.Parts, part)
				}
			}
		}
	}

	for _, f := range files {
		eff.Files = append(eff.Files, *f)
	}
	sort.Slice(eff.Files, func(i, j int) bool { return eff.Files[i].Path < eff.Files[j].Path })
	sort.Slice(eff.Violations, func(i, j int) bool { return eff.Violations[i].Path < eff.Violations[j].Path })
	return eff, nil
}

// Collisions returns the locked paths that a tree contains. The API uses it
// to reject pushes that would change a locked file.
func (r *Resolver) Collisions(ctx context.Context, repo string, tree core.Hash, locked map[string]string) ([]Violation, error) {
	w := &walker{r: r, trees: map[string]core.Tree{}}
	var out []Violation
	for p, by := range locked {
		e, err := w.lookup(ctx, repo, tree, p)
		if err != nil {
			return nil, err
		}
		if e != nil {
			out = append(out, Violation{Path: p, Source: repo, LockedBy: by})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Render returns a file's resolved bytes: the single part, or the
// concatenation of all parts for append files.
func (r *Resolver) Render(ctx context.Context, f File) ([]byte, error) {
	var parts [][]byte
	for _, p := range f.Parts {
		b, err := r.Objects.Get(ctx, p.Source, p.Hash, MaxRenderedFile)
		if err != nil {
			return nil, fmt.Errorf("%s from %s: %w", f.Path, p.Source, err)
		}
		parts = append(parts, b)
	}
	if f.Mode == Append {
		return core.JoinParts(parts...), nil
	}
	return parts[len(parts)-1], nil
}
