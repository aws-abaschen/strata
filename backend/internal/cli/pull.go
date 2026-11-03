package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/merge"
)

// ConflictState is .strata/conflicts.json: files a pull could not merge.
// Push refuses until each one is resolved.
type ConflictState struct {
	Theirs core.Hash `json:"theirs"`
	Files  []string  `json:"files"`
}

func conflictPath(root string) string { return filepath.Join(root, StateDir, "conflicts.json") }

// unresolved returns the conflicted files that still hold markers or a
// .theirs copy, and clears the record when none are left.
func unresolved(root string) ([]string, error) {
	b, err := os.ReadFile(conflictPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs ConflictState
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, err
	}
	var left []string
	for _, f := range cs.Files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if _, err := os.Stat(p + ".theirs"); err == nil {
			left = append(left, f)
			continue
		}
		if c, err := os.ReadFile(p); err == nil && merge.HasConflictMarkers(c) {
			left = append(left, f)
		}
	}
	if len(left) == 0 {
		os.Remove(conflictPath(root))
	}
	return left, nil
}

// remoteFiles lists every file of a tree stored on the server.
func remoteFiles(ctx context.Context, c *Client, repo string, root core.Hash) (map[string]core.TreeEntry, error) {
	out := map[string]core.TreeEntry{}
	if root == "" {
		return out, nil
	}
	var walk func(h core.Hash, prefix string) error
	walk = func(h core.Hash, prefix string) error {
		b, err := download(ctx, c, repo, h)
		if err != nil {
			return err
		}
		t, err := core.DecodeTree(b, h)
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
		}
		return nil
	}
	return out, walk(root, "")
}

func download(ctx context.Context, c *Client, repo string, h core.Hash) ([]byte, error) {
	rc, err := c.Download(ctx, repo, h)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	const limit = 1 << 30
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("object %s is larger than 1 GiB, which this command does not handle yet", h.Short())
	}
	if core.HashBytes(b) != h {
		return nil, fmt.Errorf("object %s: content does not match its hash", h.Short())
	}
	return b, nil
}

func sameEntry(a, b *core.TreeEntry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Hash == b.Hash && (a.Kind == core.KindExec) == (b.Kind == core.KindExec)
}

func entry(m map[string]core.TreeEntry, p string) *core.TreeEntry {
	if e, ok := m[p]; ok {
		return &e
	}
	return nil
}

func writeFile(root, rel string, b []byte, kind core.EntryKind) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if kind == core.KindExec {
		mode = 0o755
	}
	os.Chmod(p, 0o644) // may be a read-only locked Layer file
	return os.WriteFile(p, b, mode)
}

// PullResult summarizes a pull.
type PullResult struct {
	From, To  core.Hash
	Updated   int // files taken from the server
	Merged    int // files changed on both sides, merged cleanly
	Conflicts []string
	Kept      []string // changed here, deleted on the server: your copy kept
}

// Pull brings the working copy onto the server's latest revision of the
// current ref, keeping local edits: a three-way merge of HEAD (base), the
// working copy (yours) and the server (theirs). HEAD then moves to the
// server's revision, so the next push is a fast-forward.
func Pull(ctx context.Context, c *Client, st *State, root string) (PullResult, error) {
	res := PullResult{From: st.Head}
	if left, err := unresolved(root); err != nil {
		return res, err
	} else if len(left) > 0 {
		return res, fmt.Errorf("resolve the conflicts from the last pull first: %v", left)
	}
	ref, ok, err := c.GetRef(ctx, st.Repo, st.Ref)
	if err != nil {
		return res, err
	}
	if !ok || ref.Target == st.Head {
		res.To = st.Head
		return res, nil
	}
	theirsC, err := c.GetChange(ctx, st.Repo, ref.Target)
	if err != nil {
		return res, err
	}
	var baseTree core.Hash
	if st.Head != "" {
		h, err := c.GetChange(ctx, st.Repo, st.Head)
		if err != nil {
			return res, err
		}
		baseTree = h.Change.Tree
	}
	base, err := remoteFiles(ctx, c, st.Repo, baseTree)
	if err != nil {
		return res, err
	}
	theirs, err := remoteFiles(ctx, c, st.Repo, theirsC.Change.Tree)
	if err != nil {
		return res, err
	}
	snap, err := TakeSnapshot(root)
	if err != nil {
		return res, err
	}
	ours := snap.Paths

	paths := map[string]bool{}
	for _, m := range []map[string]core.TreeEntry{base, ours, theirs} {
		for p := range m {
			paths[p] = true
		}
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	label := ref.Target.Short()
	for _, p := range sorted {
		b, o, t := entry(base, p), entry(ours, p), entry(theirs, p)
		switch {
		case sameEntry(o, t), sameEntry(t, b):
			// Nothing to do: already equal, or only you changed it.
		case sameEntry(o, b):
			// Only the server changed it.
			if t == nil {
				os.Remove(filepath.Join(root, filepath.FromSlash(p)))
			} else {
				data, err := download(ctx, c, st.Repo, t.Hash)
				if err != nil {
					return res, err
				}
				if err := writeFile(root, p, data, t.Kind); err != nil {
					return res, err
				}
			}
			res.Updated++
		case o == nil:
			// Deleted here, changed on the server: take the server's.
			data, err := download(ctx, c, st.Repo, t.Hash)
			if err != nil {
				return res, err
			}
			if err := writeFile(root, p, data, t.Kind); err != nil {
				return res, err
			}
			res.Updated++
		case t == nil:
			res.Kept = append(res.Kept, p)
		default:
			// Changed on both sides.
			mine, err := snapshotBytes(snap, *o)
			if err != nil {
				return res, err
			}
			var orig []byte
			if b != nil {
				if orig, err = download(ctx, c, st.Repo, b.Hash); err != nil {
					return res, err
				}
			}
			other, err := download(ctx, c, st.Repo, t.Hash)
			if err != nil {
				return res, err
			}
			if merge.IsText(mine) && merge.IsText(other) && merge.IsText(orig) {
				if merged, n, ok := merge.Merge3(orig, mine, other, label); ok {
					if err := writeFile(root, p, merged, o.Kind); err != nil {
						return res, err
					}
					if n > 0 {
						res.Conflicts = append(res.Conflicts, p)
					} else {
						res.Merged++
					}
					continue
				}
			}
			// Binary or too large: keep yours, put theirs next to it.
			if err := writeFile(root, p+".theirs", other, t.Kind); err != nil {
				return res, err
			}
			res.Conflicts = append(res.Conflicts, p)
		}
	}

	st.Head, st.ChangeID = theirsC.Change.Revision, theirsC.Change.ChangeID
	if err := SaveState(root, *st); err != nil {
		return res, err
	}
	if len(res.Conflicts) > 0 {
		b, _ := json.MarshalIndent(ConflictState{Theirs: ref.Target, Files: res.Conflicts}, "", "  ")
		if err := os.WriteFile(conflictPath(root), b, 0o644); err != nil {
			return res, err
		}
	}
	res.To = st.Head
	return res, nil
}

// snapshotBytes returns the bytes a snapshot recorded for a file: the
// repo's own contribution for Layer-managed files, else the file itself.
func snapshotBytes(s *Snapshot, e core.TreeEntry) ([]byte, error) {
	src := s.Objects[e.Hash]
	if src.path == "" {
		return src.data, nil
	}
	return os.ReadFile(src.path)
}
