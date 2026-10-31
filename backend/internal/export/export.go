// Package export writes a Strata repository out as a plain Git repository
// plus a sidecar of evidence (sealed changes, trees, audit ledger, anchors,
// annotations, approvals), and verifies such an export offline. The format
// is specified in docs/export-format.md.
package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Format identifies this layout in manifest.json.
const Format = "strata-export/1"

// Source is what an export reads. The CLI implements it over the API, so an
// export needs only read access and works against any deployment.
type Source interface {
	Repo(ctx context.Context) (core.Repo, error)
	Refs(ctx context.Context) ([]core.Ref, error)
	Change(ctx context.Context, rev core.Hash) (core.Change, []core.Annotation, error)
	Approvals(ctx context.Context, rev core.Hash) ([]core.Approval, error)
	// Object returns an object's bytes, already checked against h.
	Object(ctx context.Context, h core.Hash) ([]byte, error)
	Audit(ctx context.Context) ([]core.AuditRecord, error)
	Anchors(ctx context.Context) ([]core.AnchorProof, error)
	// BreakGlass returns the break-glass ledger and its anchors.
	BreakGlass(ctx context.Context) ([]core.AuditRecord, []core.AnchorProof, error)
}

// Options control an export.
type Options struct {
	BreakGlass bool   // also export the break-glass ledger
	API        string // recorded in the manifest
	By         string // who ran the export, recorded in the manifest
	Now        time.Time
}

// Omitted is a tree entry Git cannot hold. It stays in the sidecar tree.
type Omitted struct {
	Tree core.Hash `json:"tree"`
	Name string    `json:"name"`
}

// Manifest describes an export. It is informational: the verifier derives
// everything it checks from the ledger and the content itself.
type Manifest struct {
	Format      string    `json:"format"`
	Repo        string    `json:"repo"`
	API         string    `json:"api,omitempty"`
	ExportedBy  string    `json:"exportedBy,omitempty"`
	ExportedAt  time.Time `json:"exportedAt"`
	Changes     int       `json:"changes"`
	Trees       int       `json:"trees"`
	Blobs       int       `json:"blobs"`
	Records     int       `json:"records"`
	Anchors     int       `json:"anchors"`
	Refs        int       `json:"refs"`
	Omitted     []Omitted `json:"omitted"`
	OmittedRefs []string  `json:"omittedRefs"`
	BreakGlass  bool      `json:"breakGlass"`
}

type writer struct {
	ctx     context.Context
	src     Source
	git     gitRepo
	side    string
	changes map[core.Hash]core.Change
	commits map[core.Hash]string
	trees   map[core.Hash]string
	blobs   map[core.Hash]string
	m       *Manifest
}

// Write exports the source repository into dir, which must be empty or absent.
func Write(ctx context.Context, src Source, dir string, opt Options) (Manifest, error) {
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		return Manifest{}, fmt.Errorf("%s is not empty", dir)
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	repo, err := src.Repo(ctx)
	if err != nil {
		return Manifest{}, err
	}
	m := &Manifest{Format: Format, Repo: repo.Name, API: opt.API, ExportedBy: opt.By, ExportedAt: opt.Now.UTC().Truncate(time.Second), Omitted: []Omitted{}, OmittedRefs: []string{}, BreakGlass: opt.BreakGlass}
	g, err := initGit(filepath.Join(dir, "git"))
	if err != nil {
		return *m, err
	}
	w := &writer{ctx: ctx, src: src, git: g, side: filepath.Join(dir, "strata"), m: m,
		changes: map[core.Hash]core.Change{}, commits: map[core.Hash]string{}, trees: map[core.Hash]string{}, blobs: map[core.Hash]string{}}
	for _, d := range []string{"changes", "trees"} {
		if err := os.MkdirAll(filepath.Join(w.side, d), 0o755); err != nil {
			return *m, err
		}
	}
	if err := w.json("repo.json", repo); err != nil {
		return *m, err
	}

	// The ledger lists every change ever written, superseded revisions
	// included, so it is the list of what to export.
	recs, err := src.Audit(ctx)
	if err != nil {
		return *m, err
	}
	proofs, err := src.Anchors(ctx)
	if err != nil {
		return *m, err
	}
	m.Records, m.Anchors = len(recs), len(proofs)
	if err := writeJSONL(w.path("audit.jsonl"), recs); err != nil {
		return *m, err
	}
	if err := w.json("anchors.json", nonNil(proofs)); err != nil {
		return *m, err
	}
	revs, err := ledgerRevisions(recs)
	if err != nil {
		return *m, err
	}

	var anns []core.Annotation
	var apps []core.Approval
	for _, rev := range revs {
		c, a, err := src.Change(ctx, rev)
		if err != nil {
			return *m, fmt.Errorf("change %s: %w", rev.Short(), err)
		}
		pre, err := c.Preimage()
		if err != nil {
			return *m, err
		}
		if c.Revision != rev || core.HashBytes(pre) != rev {
			return *m, fmt.Errorf("change %s: the server returned a change that does not hash to its revision", rev.Short())
		}
		if err := os.WriteFile(filepath.Join(w.side, "changes", string(rev)+".json"), pre, 0o644); err != nil {
			return *m, err
		}
		w.changes[rev] = c
		anns = append(anns, a...)
		ap, err := src.Approvals(ctx, rev)
		if err != nil {
			return *m, err
		}
		apps = append(apps, ap...)
	}
	for _, rev := range revs {
		sha, err := w.commit(rev)
		if err != nil {
			return *m, err
		}
		// Every revision keeps a ref, so amended-away revisions survive git gc.
		if err := g.setRef("refs/strata/revisions/"+string(rev), sha); err != nil {
			return *m, err
		}
	}
	m.Changes = len(revs)

	refs, err := src.Refs(ctx)
	if err != nil {
		return *m, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	head := ""
	for _, r := range refs {
		if r.Target == "" {
			continue
		}
		sha, ok := w.commits[r.Target]
		if !ok {
			return *m, fmt.Errorf("ref %s points at %s, which the ledger never recorded", r.Name, r.Target.Short())
		}
		if !validGitRef(r.Name) {
			m.OmittedRefs = append(m.OmittedRefs, r.Name)
			continue
		}
		if err := g.setRef("refs/heads/"+r.Name, sha); err != nil {
			return *m, err
		}
		m.Refs++
		if head == "" || r.Name == "main" {
			head = r.Name
		}
	}
	if head != "" {
		if err := os.WriteFile(filepath.Join(g.dir, "HEAD"), []byte("ref: refs/heads/"+head+"\n"), 0o644); err != nil {
			return *m, err
		}
	}
	if err := writeJSONL(w.path("refs.jsonl"), refs); err != nil {
		return *m, err
	}

	sort.SliceStable(anns, func(i, j int) bool { return anns[i].CreatedAt.Before(anns[j].CreatedAt) })
	sort.SliceStable(apps, func(i, j int) bool { return apps[i].CreatedAt.Before(apps[j].CreatedAt) })
	if err := writeJSONL(w.path("annotations.jsonl"), anns); err != nil {
		return *m, err
	}
	if err := writeJSONL(w.path("approvals.jsonl"), apps); err != nil {
		return *m, err
	}

	if opt.BreakGlass {
		bg, bgp, err := src.BreakGlass(ctx)
		if err != nil {
			return *m, fmt.Errorf("break-glass ledger: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(w.side, "break-glass"), 0o755); err != nil {
			return *m, err
		}
		if err := writeJSONL(w.path("break-glass/audit.jsonl"), bg); err != nil {
			return *m, err
		}
		if err := w.json("break-glass/anchors.json", nonNil(bgp)); err != nil {
			return *m, err
		}
	}
	m.Trees, m.Blobs = len(w.trees), len(w.blobs)
	return *m, w.json("manifest.json", m)
}

// commit writes the Git commit for rev, after its parents.
func (w *writer) commit(rev core.Hash) (string, error) {
	if sha, ok := w.commits[rev]; ok {
		return sha, nil
	}
	c, ok := w.changes[rev]
	if !ok {
		return "", fmt.Errorf("revision %s is referenced but the ledger never recorded it", rev.Short())
	}
	var parents []string
	for _, p := range c.Parents {
		sha, err := w.commit(p)
		if err != nil {
			return "", err
		}
		parents = append(parents, sha)
	}
	tree, err := w.tree(c.Tree)
	if err != nil {
		return "", err
	}
	sha, err := w.git.put("commit", encodeCommit(tree, parents, c))
	w.commits[rev] = sha
	return sha, err
}

// tree writes a Strata tree to the sidecar and its Git form to the repository.
func (w *writer) tree(h core.Hash) (string, error) {
	if sha, ok := w.trees[h]; ok {
		return sha, nil
	}
	b, err := w.src.Object(w.ctx, h)
	if err != nil {
		return "", fmt.Errorf("tree %s: %w", h.Short(), err)
	}
	t, err := core.DecodeTree(b, h)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(w.side, "trees", string(h)+".json"), b, 0o644); err != nil {
		return "", err
	}
	var es []gitEntry
	for _, e := range t.Entries {
		if omitName(e.Name) {
			w.m.Omitted = append(w.m.Omitted, Omitted{Tree: h, Name: e.Name})
			continue
		}
		var sha string
		if e.Kind == core.KindDir {
			sha, err = w.tree(e.Hash)
		} else {
			sha, err = w.blob(e.Hash)
		}
		if err != nil {
			return "", err
		}
		es = append(es, gitEntry{Mode: gitMode[e.Kind], Name: e.Name, SHA: sha})
	}
	body, err := encodeTree(es)
	if err != nil {
		return "", err
	}
	sha, err := w.git.put("tree", body)
	w.trees[h] = sha
	return sha, err
}

func (w *writer) blob(h core.Hash) (string, error) {
	if sha, ok := w.blobs[h]; ok {
		return sha, nil
	}
	b, err := w.src.Object(w.ctx, h)
	if err != nil {
		return "", fmt.Errorf("object %s: %w", h.Short(), err)
	}
	sha, err := w.git.put("blob", b)
	w.blobs[h] = sha
	return sha, err
}

func (w *writer) json(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.side, filepath.FromSlash(name)), append(b, '\n'), 0o644)
}

func (w *writer) path(name string) string { return filepath.Join(w.side, filepath.FromSlash(name)) }

func writeJSONL[T any](p string, vs []T) error {
	var b []byte
	for _, v := range vs {
		line, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b = append(append(b, line...), '\n')
	}
	return os.WriteFile(p, b, 0o644)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ledgerRevisions lists every ChangeCreated revision, in ledger order.
func ledgerRevisions(recs []core.AuditRecord) ([]core.Hash, error) {
	var out []core.Hash
	seen := map[core.Hash]bool{}
	for _, r := range recs {
		if r.Action != "ChangeCreated" {
			continue
		}
		var d struct {
			Revision core.Hash `json:"revision"`
		}
		if err := json.Unmarshal(r.Detail, &d); err != nil || !d.Revision.Valid() {
			return nil, fmt.Errorf("ledger seq %d: ChangeCreated without a revision", r.Seq)
		}
		if !seen[d.Revision] {
			seen[d.Revision] = true
			out = append(out, d.Revision)
		}
	}
	return out, nil
}

var errFailed = errors.New("export FAILED verification")
