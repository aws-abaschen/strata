package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/layers"
)

// LayerState is .strata/layers.json: what the last `strata layers pull`
// wrote, so push can tell inherited content from this repo's own edits.
// Inherited bytes are cached under .strata/layers/<hash>.
type LayerState struct {
	Chain []layers.Link          `json:"chain"`
	Files map[string]ManagedFile `json:"files"`
}

// ManagedFile is one inherited path in the working copy.
type ManagedFile struct {
	Mode      layers.Mode `json:"mode"`
	ModeSetBy string      `json:"modeSetBy"`
	// Inherited is the hash of the Layers' content, without this repo's part.
	Inherited core.Hash `json:"inherited"`
}

func layerStatePath(root string) string { return filepath.Join(root, StateDir, "layers.json") }
func layerCache(root string, h core.Hash) string {
	return filepath.Join(root, StateDir, "layers", string(h))
}

// LoadLayerState reads .strata/layers.json; no file means no managed paths.
func LoadLayerState(root string) (*LayerState, error) {
	st := &LayerState{Files: map[string]ManagedFile{}}
	b, err := os.ReadFile(layerStatePath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("%s: %w", layerStatePath(root), err)
	}
	if st.Files == nil {
		st.Files = map[string]ManagedFile{}
	}
	return st, nil
}

func (st *LayerState) save(root string) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(layerStatePath(root), b, 0o644)
}

// own returns this repo's contribution to a managed file, given its local
// bytes: nil when the file only holds inherited content.
func (st *LayerState) own(root, rel string, mf ManagedFile, local []byte) ([]byte, error) {
	inherited, err := os.ReadFile(layerCache(root, mf.Inherited))
	if err != nil {
		return nil, fmt.Errorf("%s: inherited content missing from cache, run strata layers pull: %w", rel, err)
	}
	switch mf.Mode {
	case layers.Locked:
		if !bytes.Equal(local, inherited) {
			return nil, fmt.Errorf("%w: %s is locked by layer %s; run strata layers pull to restore it", core.ErrPolicy, rel, mf.ModeSetBy)
		}
		return nil, nil
	case layers.Append:
		if !bytes.HasPrefix(local, inherited) {
			return nil, fmt.Errorf("%w: %s is append-only (layer %s): add lines after the inherited ones, do not edit them", core.ErrPolicy, rel, mf.ModeSetBy)
		}
		rest := local[len(inherited):]
		if len(inherited) > 0 && inherited[len(inherited)-1] != '\n' {
			rest = bytes.TrimPrefix(rest, []byte("\n")) // the separator JoinParts added
		}
		if len(rest) == 0 {
			return nil, nil
		}
		return rest, nil
	default: // override
		if bytes.Equal(local, inherited) {
			return nil, nil
		}
		return local, nil
	}
}

// PullLayers resolves the repository's Layers and writes the merged files
// into the working copy. Local edits to override and append files are kept.
func PullLayers(ctx context.Context, c *Client, st State, root string, out io.Writer) error {
	prev, err := LoadLayerState(root)
	if err != nil {
		return err
	}
	eff, err := c.Layers(ctx, st.Repo, "", false)
	if err != nil {
		return err
	}
	next := &LayerState{Chain: eff.Chain, Files: map[string]ManagedFile{}}
	if err := os.MkdirAll(filepath.Join(root, StateDir, "layers"), 0o755); err != nil {
		return err
	}

	var wrote, kept, locked int
	for _, f := range eff.Files {
		inherited, err := renderInherited(ctx, c, f)
		if err != nil {
			return err
		}
		ih := core.HashBytes(inherited)
		if err := os.WriteFile(layerCache(root, ih), inherited, 0o644); err != nil {
			return err
		}
		mf := ManagedFile{Mode: f.Mode, ModeSetBy: f.ModeSetBy, Inherited: ih}
		next.Files[f.Path] = mf

		dst := filepath.Join(root, filepath.FromSlash(f.Path))
		var own []byte
		if local, err := os.ReadFile(dst); err == nil && f.Mode != layers.Locked {
			if old, ok := prev.Files[f.Path]; ok {
				// Previously managed: strip what the Layers gave last time.
				// A mode change can make old edits invalid; keep them then.
				own, err = prev.own(root, f.Path, ManagedFile{Mode: f.Mode, ModeSetBy: old.ModeSetBy, Inherited: old.Inherited}, local)
				if err != nil {
					own = local
				}
			} else {
				own = local // committed by this repo (an override or appended lines)
			}
		}
		merged := inherited
		switch {
		case f.Mode == layers.Override && own != nil:
			merged = own
			kept++
		case f.Mode == layers.Append && own != nil:
			merged = core.JoinParts(inherited, own)
			kept++
		}
		if err := writeManaged(dst, merged, f.Mode == layers.Locked); err != nil {
			return err
		}
		wrote++
		if f.Mode == layers.Locked {
			locked++
		}
	}

	// Paths no longer inherited: remove them if untouched, else keep them as
	// this repo's own files.
	for p, old := range prev.Files {
		if _, still := next.Files[p]; still {
			continue
		}
		dst := filepath.Join(root, filepath.FromSlash(p))
		os.Chmod(dst, 0o644)
		local, err := os.ReadFile(dst)
		if err != nil {
			continue
		}
		if own, err := prev.own(root, p, old, local); err == nil && own == nil {
			os.Remove(dst)
		} else {
			fmt.Fprintf(out, "%s is no longer inherited; kept your local copy\n", p)
		}
	}
	if err := next.save(root); err != nil {
		return err
	}
	var chain []string
	for _, l := range eff.Chain {
		chain = append(chain, l.Layer+"@"+l.Ref)
	}
	fmt.Fprintf(out, "layers %v: %d files (%d locked, %d with local changes)\n", chain, wrote, locked, kept)
	for _, v := range eff.Violations {
		fmt.Fprintf(out, "  warning: %s from %s ignored, locked by %s\n", v.Path, v.Source, v.LockedBy)
	}
	return nil
}

func renderInherited(ctx context.Context, c *Client, f layers.File) ([]byte, error) {
	var parts [][]byte
	for _, p := range f.Parts {
		rc, err := c.Download(ctx, p.Source, p.Hash)
		if err != nil {
			return nil, fmt.Errorf("%s from %s: %w", f.Path, p.Source, err)
		}
		b, err := io.ReadAll(io.LimitReader(rc, layers.MaxRenderedFile+1))
		rc.Close()
		if err != nil {
			return nil, err
		}
		if core.HashBytes(b) != p.Hash {
			return nil, fmt.Errorf("%s from %s: content does not match its hash", f.Path, p.Source)
		}
		parts = append(parts, b)
	}
	if f.Mode == layers.Append {
		return core.JoinParts(parts...), nil
	}
	return parts[len(parts)-1], nil
}

// writeManaged writes a file; locked files are made read-only as a hint.
func writeManaged(dst string, b []byte, readOnly bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	os.Chmod(dst, 0o644) // a previous pull may have made it read-only
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return err
	}
	if readOnly {
		return os.Chmod(dst, 0o444)
	}
	return nil
}

func printEffective(w io.Writer, eff layers.Effective) {
	if len(eff.Chain) == 0 {
		fmt.Fprintln(w, "this repository does not extend a layer")
		return
	}
	fmt.Fprint(w, "chain:")
	for _, l := range eff.Chain {
		rev := "(empty)"
		if !l.Missing {
			rev = l.Revision.Short()
		}
		fmt.Fprintf(w, " %s@%s %s ->", l.Layer, l.Ref, rev)
	}
	fmt.Fprintf(w, " %s\n", eff.Repo)
	sort.Slice(eff.Files, func(i, j int) bool { return eff.Files[i].Path < eff.Files[j].Path })
	for _, f := range eff.Files {
		var from []string
		for _, p := range f.Parts {
			from = append(from, p.Source)
		}
		fmt.Fprintf(w, "  %-8s %-40s from %v (mode set by %s)\n", f.Mode, f.Path, from, f.ModeSetBy)
	}
	for _, v := range eff.Violations {
		fmt.Fprintf(w, "  ignored: %s from %s, locked by %s\n", v.Path, v.Source, v.LockedBy)
	}
}
