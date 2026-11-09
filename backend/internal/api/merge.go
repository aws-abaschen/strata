package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/authz"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/merge"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// Server-side merge limits keep one request well inside the Lambda timeout.
const (
	maxMergeFileBytes = 1 << 20 // per side, for a line-by-line merge
	maxMergeFiles     = 200     // files merged line by line
	maxMergeTrees     = 5000    // directories read
	maxConflicts      = 100     // conflicts reported
	// maxMergeTime is the time one merge may spend reading and merging. The
	// API Lambda stops at 29 s; past this budget the merge is refused with a
	// clear error instead of a timeout.
	maxMergeTime = 20 * time.Second
)

// Merge outcomes.
const (
	MergeUpToDate    = "up-to-date"   // the target already has the source
	MergeFastForward = "fast-forward" // the target is behind: move it
	MergeClean       = "merged"       // a merge change with two parents
	MergeConflicts   = "conflicts"    // nothing written
)

// Conflict is one path the server could not merge.
type Conflict struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	// Limit is true when the two sides may well merge, but the file is past
	// what the server merges (size or count). Editing the branch does not
	// help: it needs a merge in a working copy.
	Limit bool `json:"limit,omitempty"`
}

// MergeResult is the reply of POST /repos/{repo}/merge.
type MergeResult struct {
	Status    string       `json:"status"`
	Source    core.Hash    `json:"source"`
	Target    core.Hash    `json:"target"`
	Base      core.Hash    `json:"base,omitempty"`
	Into      string       `json:"into"`
	Conflicts []Conflict   `json:"conflicts"`
	Ref       *core.Ref    `json:"ref,omitempty"`
	Change    *core.Change `json:"change,omitempty"`
}

type mergeInput struct {
	Source string `json:"source"` // ref brought in
	Target string `json:"target"` // ref merged into
	// Into is where the result goes (default Target), for example a review
	// ref when the target needs approvals on the exact revision that lands.
	Into     string        `json:"into"`
	Message  string        `json:"message"`
	Metadata core.Metadata `json:"metadata"`
	// DryRun only reports what a merge would do. It needs read access.
	DryRun bool `json:"dryRun"`
}

// mergeRefs merges one ref into another on the server.
//
//   - The target already contains the source: nothing to do.
//   - The target is behind the source: the result ref moves to the source
//     revision (a fast-forward, so approvals on that revision count).
//   - Both moved: a change with parents [target, source] and the merged
//     tree. It is new, so a policy that demands approvals sends it to a
//     review ref first (Into), to be approved and promoted like any change.
//
// Conflicts write nothing: they are reported with their paths (409).
func (a *API) mergeRefs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in mergeInput
	if err := readJSON(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	action := authz.Write
	if in.DryRun {
		action = authz.Read
	}
	rp, err := a.repoFor(r, action)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if in.Into == "" {
		in.Into = in.Target
	}
	for _, n := range []string{in.Source, in.Target, in.Into} {
		if err := core.ValidateRefName(n); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	if in.Source == in.Target {
		a.fail(w, r, badRequest("source and target are the same ref"))
		return
	}
	src, err := a.Meta.GetRef(ctx, rp.Name, in.Source)
	if err != nil {
		a.fail(w, r, fmt.Errorf("ref %q: %w", in.Source, err))
		return
	}
	tgt, err := a.Meta.GetRef(ctx, rp.Name, in.Target)
	if err != nil {
		a.fail(w, r, fmt.Errorf("ref %q: %w", in.Target, err))
		return
	}
	res := MergeResult{Source: src.Target, Target: tgt.Target, Into: in.Into, Conflicts: []Conflict{}}
	srcChange, err := a.Meta.GetChange(ctx, rp.Name, src.Target)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rw, err := a.walkRange(ctx, rp.Name, srcChange, tgt.Target, true)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	switch {
	case src.Target == tgt.Target:
		res.Status, res.Base = MergeUpToDate, tgt.Target
	case rw.Truncated:
		a.fail(w, r, badRequest("the branches are more than %d changes apart, which is more than the server merges", maxRangeWalk))
		return
	case len(rw.Common) == 0:
		a.fail(w, r, badRequest("%s and %s share no history", in.Source, in.Target))
		return
	case rw.BaseInHead:
		res.Status, res.Base = MergeFastForward, tgt.Target
	case len(rw.Common) > 1:
		a.fail(w, r, badRequest("%s and %s have %d merge bases (a criss-cross merge), which the server does not merge", in.Source, in.Target, len(rw.Common)))
		return
	case len(rw.InRange) == 0:
		res.Status, res.Base = MergeUpToDate, rw.Common[0]
	default:
		res.Status, res.Base = MergeClean, rw.Common[0]
	}

	// The result ref's current value, for compare-and-swap.
	expected := tgt.Target
	if in.Into != in.Target {
		cur, err := a.Meta.GetRef(ctx, rp.Name, in.Into)
		switch {
		case errors.Is(err, store.ErrNotFound):
			expected = ""
		case err != nil:
			a.fail(w, r, err)
			return
		default:
			expected = cur.Target
		}
	}

	if res.Status == MergeUpToDate {
		writeJSON(w, http.StatusOK, res)
		return
	}
	tgtChange, err := a.Meta.GetChange(ctx, rp.Name, tgt.Target)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// A result ref other than the target (a review ref) only moves forward.
	// Replacing an earlier merge result there would strand its approvals.
	if expected != "" && expected != tgt.Target && !in.DryRun {
		tips := []core.Change{srcChange}
		if res.Status == MergeClean {
			tips = append(tips, tgtChange)
		}
		ok, err := a.reaches(ctx, rp.Name, tips, expected)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if !ok {
			a.fail(w, r, fmt.Errorf("%w: ref %q already holds %s, which this merge does not contain: use another name, or delete the ref first", store.ErrConflict, in.Into, expected.Short()))
			return
		}
	}

	switch res.Status {
	case MergeFastForward:
		if !in.DryRun {
			ref, err := a.moveRef(r, rp, in.Into, src.Target, expected)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			res.Ref = &ref
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	base, err := a.Meta.GetChange(ctx, rp.Name, res.Base)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	m := &treeMerger{a: a, ctx: ctx, repo: rp.Name, dry: in.DryRun, label: in.Source, deadline: time.Now().Add(maxMergeTime)}
	tree, err := m.tree("", base.Tree, tgtChange.Tree, srcChange.Tree)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(m.conflicts) > 0 {
		res.Status, res.Conflicts = MergeConflicts, m.conflicts
		status := http.StatusConflict
		if in.DryRun {
			status = http.StatusOK
		}
		writeJSON(w, status, res)
		return
	}
	if in.DryRun {
		writeJSON(w, http.StatusOK, res)
		return
	}

	msg := in.Message
	if msg == "" {
		msg = fmt.Sprintf("Merge %s into %s", in.Source, in.Target)
	}
	// Link the merge to the work items it brings in, as many as fit.
	md := in.Metadata
	if err := md.Normalize(); err != nil {
		a.fail(w, r, err)
		return
	}
	have := map[string]bool{}
	for _, wi := range append(core.ParseWorkItems(msg), md.WorkItems...) {
		have[wi.String()] = true
	}
	for _, c := range rw.InRange {
		for _, wi := range c.Metadata.WorkItems {
			if !have[wi.String()] && len(have) < core.MaxWorkItems {
				have[wi.String()] = true
				md.WorkItems = append(md.WorkItems, wi)
			}
		}
	}
	c, err := a.writeChange(r, rp, createChangeInput{
		Tree: tree, Parents: []core.Hash{tgt.Target, src.Target}, Message: msg, Metadata: md,
		Ref: in.Into, ExpectedRef: &expected,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	res.Change = &c
	writeJSON(w, http.StatusCreated, res)
}

// treeMerger merges three trees: base, ours (the target) and theirs (the
// source). Merged files and trees are stored unless dry is set.
type treeMerger struct {
	a         *API
	ctx       context.Context
	repo      string
	dry       bool
	label     string
	conflicts []Conflict
	files     int
	trees     int
	deadline  time.Time // zero = no time budget
}

// reaches reports whether rev is one of tips or in the history of one.
// A history too long to read counts as not reached.
func (a *API) reaches(ctx context.Context, repo string, tips []core.Change, rev core.Hash) (bool, error) {
	for _, tip := range tips {
		rw, err := a.walkRange(ctx, repo, tip, rev, false)
		if err != nil {
			return false, err
		}
		if rw.BaseInHead && !rw.Truncated {
			return true, nil
		}
	}
	return false, nil
}

func (m *treeMerger) conflict(p, reason string) {
	if len(m.conflicts) < maxConflicts {
		m.conflicts = append(m.conflicts, Conflict{Path: p, Reason: reason})
	}
}

// overLimit records a file the server will not merge because of its size
// or the number of files, not because the two sides clash.
func (m *treeMerger) overLimit(p, reason string) {
	if len(m.conflicts) < maxConflicts {
		m.conflicts = append(m.conflicts, Conflict{Path: p, Reason: reason, Limit: true})
	}
}

// inTime fails once the merge has used its time budget.
func (m *treeMerger) inTime() error {
	if !m.deadline.IsZero() && time.Now().After(m.deadline) {
		return badRequest("the merge took more than %s on the server, which is its limit: nothing was merged", maxMergeTime)
	}
	return m.ctx.Err()
}

func (m *treeMerger) put(b []byte) (core.Hash, error) {
	if m.dry {
		return core.HashBytes(b), nil
	}
	return m.a.Objects.PutObject(m.ctx, m.repo, b)
}

func (m *treeMerger) load(h core.Hash) (core.Tree, error) {
	if h == "" {
		return core.Tree{}, nil
	}
	m.trees++
	if m.trees > maxMergeTrees {
		return core.Tree{}, badRequest("more than %d directories to merge, which is more than the server merges", maxMergeTrees)
	}
	if err := m.inTime(); err != nil {
		return core.Tree{}, err
	}
	return m.a.loadTree(m.ctx, m.repo, h)
}

// tree merges directory p. base is "" when the directory is new on both
// sides. Below the root, a directory left with no entries returns "": like
// a push, a merge stores no empty directories.
func (m *treeMerger) tree(p string, base, ours, theirs core.Hash) (core.Hash, error) {
	switch {
	case ours == theirs, base == theirs:
		return ours, nil
	case base == ours:
		return theirs, nil
	}
	var sides [3]map[string]*core.TreeEntry
	names := map[string]bool{}
	for i, h := range []core.Hash{base, ours, theirs} {
		t, err := m.load(h)
		if err != nil {
			return "", err
		}
		sides[i] = map[string]*core.TreeEntry{}
		for _, e := range t.Entries {
			sides[i][e.Name] = &e
			names[e.Name] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var out core.Tree
	for _, n := range sorted {
		e, err := m.entry(path.Join(p, n), sides[0][n], sides[1][n], sides[2][n])
		if err != nil {
			return "", err
		}
		if e != nil {
			e.Name = n
			out.Entries = append(out.Entries, *e)
		}
	}
	if len(out.Entries) == 0 && p != "" {
		return "", nil
	}
	b, _, err := out.Encode()
	if err != nil {
		return "", err
	}
	return m.put(b)
}

func sameEntry(x, y *core.TreeEntry) bool {
	if x == nil || y == nil {
		return x == y
	}
	return x.Kind == y.Kind && x.Hash == y.Hash
}

func isDir(e *core.TreeEntry) bool  { return e != nil && e.Kind == core.KindDir }
func isFile(e *core.TreeEntry) bool { return e != nil && e.Kind != core.KindDir }

// entry merges one name. nil means the name is absent from the result.
func (m *treeMerger) entry(p string, base, ours, theirs *core.TreeEntry) (*core.TreeEntry, error) {
	switch {
	case sameEntry(ours, theirs), sameEntry(base, theirs):
		return ours, nil
	case sameEntry(base, ours):
		return theirs, nil
	case isDir(ours) && isDir(theirs):
		var bh core.Hash
		if isDir(base) {
			bh = base.Hash
		}
		h, err := m.tree(p, bh, ours.Hash, theirs.Hash)
		if err != nil {
			return nil, err
		}
		if h == "" {
			return nil, nil // both sides emptied it between them
		}
		return &core.TreeEntry{Kind: core.KindDir, Hash: h}, nil
	case isFile(ours) && isFile(theirs) && (base == nil || isFile(base)):
		return m.file(p, base, ours, theirs)
	case ours == nil || theirs == nil:
		m.conflict(p, "deleted on one side, changed on the other")
	default:
		m.conflict(p, "a file on one side, a directory on the other")
	}
	return ours, nil
}

// file merges a file changed on both sides: the exec bit and the content
// are merged separately.
func (m *treeMerger) file(p string, base, ours, theirs *core.TreeEntry) (*core.TreeEntry, error) {
	kind := ours.Kind
	if ours.Kind != theirs.Kind {
		switch {
		case base != nil && base.Kind == ours.Kind:
			kind = theirs.Kind
		case base != nil && base.Kind == theirs.Kind:
		default:
			m.conflict(p, "the executable bit differs")
			return ours, nil
		}
	}
	out := &core.TreeEntry{Kind: kind, Hash: ours.Hash, Size: ours.Size}
	switch {
	case ours.Hash == theirs.Hash:
		return out, nil
	case base == nil:
		m.conflict(p, "added on both sides with different content")
		return ours, nil
	case base.Hash == ours.Hash:
		out.Hash, out.Size = theirs.Hash, theirs.Size
		return out, nil
	case base.Hash == theirs.Hash:
		return out, nil
	}
	if base.Size > maxMergeFileBytes || ours.Size > maxMergeFileBytes || theirs.Size > maxMergeFileBytes {
		m.overLimit(p, "changed on both sides and too large to merge on the server")
		return ours, nil
	}
	m.files++
	if m.files > maxMergeFiles {
		m.overLimit(p, "changed on both sides; too many such files to merge on the server")
		return ours, nil
	}
	if err := m.inTime(); err != nil {
		return nil, err
	}
	var blobs [3][]byte
	for i, e := range []*core.TreeEntry{base, ours, theirs} {
		b, err := m.a.Objects.Get(m.ctx, m.repo, e.Hash, maxMergeFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		blobs[i] = b
	}
	if !merge.IsText(blobs[0]) || !merge.IsText(blobs[1]) || !merge.IsText(blobs[2]) {
		m.conflict(p, "binary file changed on both sides")
		return ours, nil
	}
	merged, n, ok := merge.Merge3(blobs[0], blobs[1], blobs[2], m.label)
	switch {
	case !ok:
		m.overLimit(p, "changed on both sides and too large to merge on the server")
		return ours, nil
	case n > 0:
		m.conflict(p, "both sides changed the same lines")
		return ours, nil
	}
	h, err := m.put(merged)
	if err != nil {
		return nil, err
	}
	out.Hash, out.Size = h, int64(len(merged))
	return out, nil
}
