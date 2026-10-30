package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// StateDir holds the working copy's state, like .git.
const StateDir = ".strata"

// State is .strata/state.json.
type State struct {
	API      string    `json:"api"`
	Repo     string    `json:"repo"`
	Ref      string    `json:"ref"`
	Head     core.Hash `json:"head,omitempty"`
	ChangeID string    `json:"changeId,omitempty"`
}

// LoadState finds .strata/state.json in dir or a parent, and returns the
// working-copy root.
func LoadState(dir string) (State, string, error) {
	dir, _ = filepath.Abs(dir)
	for {
		b, err := os.ReadFile(filepath.Join(dir, StateDir, "state.json"))
		if err == nil {
			var s State
			return s, dir, json.Unmarshal(b, &s)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return State{}, "", errors.New("not a strata working copy (run strata init or strata clone)")
		}
		dir = parent
	}
}

// SaveState writes .strata/state.json.
func SaveState(root string, s State) error {
	if err := os.MkdirAll(filepath.Join(root, StateDir), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(filepath.Join(root, StateDir, "state.json"), b, 0o644)
}

// source is where an object's bytes come from during a push.
type source struct {
	path string // a file on disk, or
	data []byte // an encoded tree
	size int64
}

// Snapshot is a working copy hashed into objects.
type Snapshot struct {
	Root    core.Hash
	Objects map[core.Hash]source
	Files   int
}

var alwaysIgnore = map[string]bool{StateDir: true, ".git": true, "node_modules": true}

type ignorer struct{ patterns []string }

func loadIgnore(root string) ignorer {
	var ig ignorer
	f, err := os.Open(filepath.Join(root, ".strataignore"))
	if err != nil {
		return ig
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l != "" && !strings.HasPrefix(l, "#") {
			ig.patterns = append(ig.patterns, l)
		}
	}
	return ig
}

// match: a pattern with a slash matches the relative path, otherwise the
// base name. A trailing slash limits the pattern to directories.
func (ig ignorer) match(rel string, dir bool) bool {
	base := path.Base(rel)
	if alwaysIgnore[base] {
		return true
	}
	for _, p := range ig.patterns {
		if strings.HasSuffix(p, "/") {
			if !dir {
				continue
			}
			p = strings.TrimSuffix(p, "/")
		}
		target := base
		if strings.Contains(p, "/") {
			target, p = rel, strings.TrimPrefix(p, "/")
		}
		if ok, _ := path.Match(p, target); ok {
			return true
		}
	}
	return false
}

// TakeSnapshot hashes the working copy at root.
func TakeSnapshot(root string) (*Snapshot, error) {
	s := &Snapshot{Objects: map[core.Hash]source{}}
	ig := loadIgnore(root)
	h, _, err := s.dir(root, "", ig)
	if err != nil {
		return nil, err
	}
	s.Root = h
	return s, nil
}

func (s *Snapshot) dir(abs, rel string, ig ignorer) (core.Hash, int, error) {
	des, err := os.ReadDir(abs)
	if err != nil {
		return "", 0, err
	}
	var t core.Tree
	for _, de := range des {
		r := path.Join(rel, de.Name())
		p := filepath.Join(abs, de.Name())
		if ig.match(r, de.IsDir()) {
			continue
		}
		switch {
		case de.IsDir():
			h, n, err := s.dir(p, r, ig)
			if err != nil {
				return "", 0, err
			}
			if n > 0 { // like Git, empty directories are not stored
				t.Entries = append(t.Entries, core.TreeEntry{Name: de.Name(), Kind: core.KindDir, Hash: h})
			}
		case de.Type().IsRegular():
			info, err := de.Info()
			if err != nil {
				return "", 0, err
			}
			h, err := hashFile(p)
			if err != nil {
				return "", 0, err
			}
			kind := core.KindFile
			if info.Mode()&0o111 != 0 {
				kind = core.KindExec
			}
			s.Objects[h] = source{path: p, size: info.Size()}
			s.Files++
			t.Entries = append(t.Entries, core.TreeEntry{Name: de.Name(), Kind: kind, Hash: h, Size: info.Size()})
		default:
			fmt.Fprintf(os.Stderr, "skipping %s: symlinks and special files are not supported yet\n", r)
		}
	}
	b, h, err := t.Encode()
	if err != nil {
		return "", 0, err
	}
	s.Objects[h] = source{data: b, size: int64(len(b))}
	return h, len(t.Entries), nil
}

func hashFile(p string) (core.Hash, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return core.Hash(hex.EncodeToString(sum.Sum(nil))), nil
}

// Upload sends every object the server is missing. It returns how many were sent.
func (s *Snapshot) Upload(ctx context.Context, c *Client, repo string) (int, error) {
	var all []ObjectSize
	for h, src := range s.Objects {
		all = append(all, ObjectSize{Hash: h, Size: src.size})
	}
	sent := 0
	for i := 0; i < len(all); i += 1000 {
		batch := all[i:min(i+1000, len(all))]
		ups, err := c.Negotiate(ctx, repo, batch)
		if err != nil {
			return sent, err
		}
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			first error
			sem   = make(chan struct{}, 8)
		)
		for _, u := range ups {
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				src := s.Objects[u.Hash]
				var body io.Reader = bytes.NewReader(src.data)
				if src.path != "" {
					f, err := os.Open(src.path)
					if err != nil {
						mu.Lock()
						first = errors.Join(first, err)
						mu.Unlock()
						return
					}
					defer f.Close()
					body = f
				}
				err := c.Upload(ctx, u, body, src.size)
				mu.Lock()
				if err != nil {
					first = errors.Join(first, err)
				} else {
					sent++
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		if first != nil {
			return sent, first
		}
	}
	return sent, nil
}

// Checkout writes the tree h into dir.
func Checkout(ctx context.Context, c *Client, repo string, h core.Hash, dir string) (int, error) {
	rc, err := c.Download(ctx, repo, h)
	if err != nil {
		return 0, err
	}
	b, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	rc.Close()
	if err != nil {
		return 0, err
	}
	t, err := core.DecodeTree(b, h)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	n := 0
	for _, e := range t.Entries {
		p := filepath.Join(dir, e.Name)
		if e.Kind == core.KindDir {
			m, err := Checkout(ctx, c, repo, e.Hash, p)
			n += m
			if err != nil {
				return n, err
			}
			continue
		}
		if err := downloadFile(ctx, c, repo, e, p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func downloadFile(ctx context.Context, c *Client, repo string, e core.TreeEntry, p string) error {
	rc, err := c.Download(ctx, repo, e.Hash)
	if err != nil {
		return err
	}
	defer rc.Close()
	mode := fs.FileMode(0o644)
	if e.Kind == core.KindExec {
		mode = 0o755
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, sum), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if core.Hash(hex.EncodeToString(sum.Sum(nil))) != e.Hash {
		return fmt.Errorf("%s: content does not match its hash", p)
	}
	return nil
}
