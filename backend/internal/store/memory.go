package store

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Memory implements Objects and Meta in memory, for tests and local runs.
// Mount Memory.UploadHandler at BaseURL so presigned uploads work.
type Memory struct {
	// BaseURL is where UploadHandler is served, e.g. an httptest server URL.
	BaseURL string

	mu          sync.Mutex
	objects     map[string][]byte
	changeDocs  map[string][]byte
	repos       map[string]core.Repo
	refs        map[string]map[string]core.Ref
	changes     map[string]core.Change
	pointers    map[string]core.Hash
	annotations map[string][]core.Annotation
	index       map[string][]core.ChangeSummary
}

// NewMemory returns an empty store.
func NewMemory() *Memory {
	return &Memory{
		objects: map[string][]byte{}, changeDocs: map[string][]byte{}, repos: map[string]core.Repo{},
		refs: map[string]map[string]core.Ref{}, changes: map[string]core.Change{}, pointers: map[string]core.Hash{},
		annotations: map[string][]core.Annotation{}, index: map[string][]core.ChangeSummary{},
	}
}

func okey(repo string, h core.Hash) string { return repo + "/" + string(h) }

// Exists implements Objects.
func (m *Memory) Exists(_ context.Context, repo string, h core.Hash) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[okey(repo, h)]
	return ok, nil
}

// Get implements Objects.
func (m *Memory) Get(_ context.Context, repo string, h core.Hash, max int64) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[okey(repo, h)]
	if !ok {
		return nil, ErrNotFound
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("object %s larger than %d bytes", h.Short(), max)
	}
	return b, nil
}

// Put stores bytes directly (test helper). It verifies the hash.
func (m *Memory) Put(repo string, b []byte) core.Hash {
	h := core.HashBytes(b)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[okey(repo, h)] = slices.Clone(b)
	return h
}

// PresignPut implements Objects.
func (m *Memory) PresignPut(_ context.Context, repo string, h core.Hash, _ int64) (Upload, error) {
	return Upload{Hash: h, Method: http.MethodPut, URL: m.BaseURL + "/_objects/" + repo + "/" + string(h), Headers: map[string]string{}}, nil
}

// PresignGet implements Objects.
func (m *Memory) PresignGet(_ context.Context, repo string, h core.Hash) (string, error) {
	return m.BaseURL + "/_objects/" + repo + "/" + string(h), nil
}

// UploadHandler serves the presigned URLs: PUT verifies the hash like S3's
// checksum check, GET returns the bytes.
func (m *Memory) UploadHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, h, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/_objects/"), "/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			if core.HashBytes(b) != core.Hash(h) {
				http.Error(w, "checksum mismatch", http.StatusBadRequest)
				return
			}
			m.Put(repo, b)
		case http.MethodGet:
			b, err := m.Get(r.Context(), repo, core.Hash(h), 1<<40)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(b)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// PutChangeDoc implements Objects.
func (m *Memory) PutChangeDoc(_ context.Context, c core.Change, doc []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := okey(c.Repo, c.Revision)
	if _, ok := m.changeDocs[k]; ok {
		return ErrExists
	}
	m.changeDocs[k] = slices.Clone(doc)
	return nil
}

// CreateRepo implements Meta.
func (m *Memory) CreateRepo(_ context.Context, r core.Repo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.repos[r.Name]; ok {
		return ErrExists
	}
	m.repos[r.Name] = r
	m.refs[r.Name] = map[string]core.Ref{}
	return nil
}

// GetRepo implements Meta.
func (m *Memory) GetRepo(_ context.Context, name string) (core.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.repos[name]
	if !ok {
		return core.Repo{}, ErrNotFound
	}
	return r, nil
}

// ListRepos implements Meta.
func (m *Memory) ListRepos(context.Context) ([]core.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []core.Repo{}
	for _, r := range m.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListRefs implements Meta.
func (m *Memory) ListRefs(_ context.Context, repo string) ([]core.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []core.Ref{}
	for _, r := range m.refs[repo] {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetRef implements Meta.
func (m *Memory) GetRef(_ context.Context, repo, name string) (core.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.refs[repo][name]
	if !ok {
		return core.Ref{}, ErrNotFound
	}
	return r, nil
}

func (m *Memory) casRef(repo string, ref core.Ref, expected core.Hash) error {
	cur, ok := m.refs[repo][ref.Name]
	if (expected == "" && ok) || (expected != "" && (!ok || cur.Target != expected)) {
		return ErrConflict
	}
	return nil
}

// UpdateRef implements Meta.
func (m *Memory) UpdateRef(_ context.Context, repo string, ref core.Ref, expected core.Hash) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.casRef(repo, ref, expected); err != nil {
		return err
	}
	if ref.Target == "" {
		delete(m.refs[repo], ref.Name)
	} else {
		m.refs[repo][ref.Name] = ref
	}
	return nil
}

// PutChange implements Meta.
func (m *Memory) PutChange(_ context.Context, c core.Change, ref *RefUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pk := c.Repo + "/" + c.ChangeID
	if cur := m.pointers[pk]; cur != c.Predecessor {
		return ErrConflict
	}
	if _, ok := m.changes[okey(c.Repo, c.Revision)]; ok {
		return ErrExists
	}
	if ref != nil {
		if err := m.casRef(c.Repo, core.Ref{Name: ref.Name}, ref.Expected); err != nil {
			return err
		}
		m.refs[c.Repo][ref.Name] = core.Ref{Name: ref.Name, Target: c.Revision, UpdatedBy: c.Author, UpdatedAt: c.CreatedAt}
	}
	m.changes[okey(c.Repo, c.Revision)] = c
	m.pointers[pk] = c.Revision
	m.addIndex(c.Metadata, c.Summary())
	return nil
}

func (m *Memory) addIndex(md core.Metadata, s core.ChangeSummary) {
	for _, w := range md.WorkItems {
		m.index["wi:"+w.String()] = append(m.index["wi:"+w.String()], s)
	}
	for _, l := range md.Labels {
		m.index["l:"+l] = append(m.index["l:"+l], s)
	}
}

// GetChange implements Meta.
func (m *Memory) GetChange(_ context.Context, repo string, rev core.Hash) (core.Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.changes[okey(repo, rev)]
	if !ok {
		return core.Change{}, ErrNotFound
	}
	return c, nil
}

// LatestRevision implements Meta.
func (m *Memory) LatestRevision(_ context.Context, repo, changeID string) (core.Hash, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.pointers[repo+"/"+changeID]
	if !ok {
		return "", ErrNotFound
	}
	return h, nil
}

// PutAnnotation implements Meta.
func (m *Memory) PutAnnotation(_ context.Context, a core.Annotation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.changes[okey(a.Repo, a.Revision)]
	if !ok {
		return ErrNotFound
	}
	k := okey(a.Repo, a.Revision)
	m.annotations[k] = append(m.annotations[k], a)
	s := c.Summary()
	s.Via = "annotation"
	m.addIndex(a.Metadata, s)
	return nil
}

// ListAnnotations implements Meta.
func (m *Memory) ListAnnotations(_ context.Context, repo string, rev core.Hash) ([]core.Annotation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.Annotation{}, m.annotations[okey(repo, rev)]...), nil
}

func (m *Memory) find(k string, limit int) []core.ChangeSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]core.ChangeSummary{}, m.index[k]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// FindByWorkItem implements Meta.
func (m *Memory) FindByWorkItem(_ context.Context, system, key string, limit int) ([]core.ChangeSummary, error) {
	return m.find("wi:"+core.WorkItem{System: system, Key: key}.String(), limit), nil
}

// FindByLabel implements Meta.
func (m *Memory) FindByLabel(_ context.Context, label string, limit int) ([]core.ChangeSummary, error) {
	return m.find("l:"+label, limit), nil
}
