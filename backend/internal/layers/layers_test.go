package layers

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// commit writes files (path -> content) as a new change on main.
func commit(t *testing.T, mem *store.Memory, repo string, files map[string]string) core.Hash {
	t.Helper()
	var build func(prefix string) core.Hash
	build = func(prefix string) core.Hash {
		var tr core.Tree
		dirs := map[string]bool{}
		for p, c := range files {
			if prefix != "" && !strings.HasPrefix(p, prefix+"/") {
				continue
			}
			rest := strings.TrimPrefix(p, prefix+"/")
			if prefix == "" {
				rest = p
			}
			if name, _, nested := strings.Cut(rest, "/"); nested {
				if !dirs[name] {
					dirs[name] = true
					tr.Entries = append(tr.Entries, core.TreeEntry{Name: name, Kind: core.KindDir, Hash: build(path.Join(prefix, name))})
				}
				continue
			}
			h := mem.Put(repo, []byte(c))
			tr.Entries = append(tr.Entries, core.TreeEntry{Name: rest, Kind: core.KindFile, Hash: h, Size: int64(len(c))})
		}
		b, _, err := tr.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return mem.Put(repo, b)
	}
	ctx := context.Background()
	var parents []core.Hash
	expected := core.Hash("")
	if ref, err := mem.GetRef(ctx, repo, "main"); err == nil {
		parents, expected = []core.Hash{ref.Target}, ref.Target
	}
	c := core.Change{ChangeID: core.NewChangeID(), Repo: repo, Tree: build(""), Parents: parents, Author: "t", CreatedAt: time.Now(), Message: "x"}
	if _, err := c.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := mem.PutChange(ctx, c, &store.RefUpdate{Name: "main", Expected: expected}, core.AuditEntry{Action: "ChangeCreated"}); err != nil {
		t.Fatal(err)
	}
	return c.Revision
}

func repo(t *testing.T, mem *store.Memory, name string, kind core.RepoKind, extends string) core.Repo {
	t.Helper()
	r := core.Repo{Name: name, Kind: kind}
	if extends != "" {
		l, _ := core.ParseLayerRef(extends)
		r.Extends = &l
	}
	if err := mem.CreateRepo(context.Background(), r, core.AuditEntry{Action: "RepoCreated"}); err != nil {
		t.Fatal(err)
	}
	return r
}

func setup(t *testing.T) (*store.Memory, *Resolver, core.Repo) {
	mem := store.NewMemory()
	repo(t, mem, "org-rules", core.KindLayer, "")
	repo(t, mem, "team-rules", core.KindLayer, "org-rules")
	cards := repo(t, mem, "cards", core.KindCode, "team-rules")
	commit(t, mem, "org-rules", map[string]string{
		ManifestPath:      `{"rules":[{"path":"ci/**","mode":"locked"},{"path":"AGENTS.md","mode":"append"}]}`,
		"ci/pipeline.yml": "org pipeline",
		"AGENTS.md":       "org rules",
		".editorconfig":   "org",
	})
	commit(t, mem, "team-rules", map[string]string{
		"AGENTS.md":       "team rules",
		".editorconfig":   "team",
		"ci/pipeline.yml": "team hack",
		"lint/rules.json": "{}",
	})
	commit(t, mem, "cards", map[string]string{
		"AGENTS.md":     "repo notes",
		".editorconfig": "repo",
		"main.go":       "package main",
	})
	return mem, &Resolver{Meta: mem, Objects: mem}, cards
}

func TestResolveCascade(t *testing.T) {
	_, r, cards := setup(t)
	ctx := context.Background()
	eff, err := r.Resolve(ctx, cards, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(eff.Chain) != 2 || eff.Chain[0].Layer != "org-rules" || eff.Chain[1].Layer != "team-rules" {
		t.Fatalf("chain %+v", eff.Chain)
	}
	byPath := map[string]File{}
	for _, f := range eff.Files {
		byPath[f.Path] = f
	}
	if _, ok := byPath[ManifestPath]; ok {
		t.Error("the manifest itself must not be inherited")
	}
	if _, ok := byPath["main.go"]; ok {
		t.Error("the repo's own non-layer files are not part of the effective config")
	}

	render := func(p string) string {
		b, err := r.Render(ctx, byPath[p])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if f := byPath["ci/pipeline.yml"]; f.Mode != Locked || f.ModeSetBy != "org-rules" || render("ci/pipeline.yml") != "org pipeline" {
		t.Errorf("locked file: %+v", f)
	}
	if got := render("AGENTS.md"); got != "org rules\nteam rules\nrepo notes" {
		t.Errorf("append: %q", got)
	}
	if got := render(".editorconfig"); got != "repo" {
		t.Errorf("override: repo should win, got %q", got)
	}
	if f := byPath["lint/rules.json"]; f.ModeSetBy != "team-rules" || f.Mode != Override {
		t.Errorf("team file: %+v", f)
	}
	if len(eff.Violations) != 1 || eff.Violations[0] != (Violation{Path: "ci/pipeline.yml", Source: "team-rules", LockedBy: "org-rules"}) {
		t.Errorf("violations %+v", eff.Violations)
	}

	// Without the repo level, the inherited view.
	inh, _ := r.Resolve(ctx, cards, "")
	for _, f := range inh.Files {
		if f.Path == ".editorconfig" {
			b, _ := r.Render(ctx, f)
			if string(b) != "team" {
				t.Errorf("inherited .editorconfig = %q", b)
			}
		}
	}
}

func TestCollisions(t *testing.T) {
	mem, r, cards := setup(t)
	ctx := context.Background()
	eff, _ := r.Resolve(ctx, cards, "")
	rev := commit(t, mem, "cards", map[string]string{"ci/pipeline.yml": "mine", "x": "y"})
	c, _ := mem.GetChange(ctx, "cards", rev)
	v, err := r.Collisions(ctx, "cards", c.Tree, eff.Locked())
	if err != nil || len(v) != 1 || v[0].Path != "ci/pipeline.yml" {
		t.Fatalf("collisions %v %v", v, err)
	}
}

func TestChainErrors(t *testing.T) {
	mem := store.NewMemory()
	ctx := context.Background()
	r := &Resolver{Meta: mem, Objects: mem}
	a := repo(t, mem, "a-layer", core.KindLayer, "b-layer")
	repo(t, mem, "b-layer", core.KindLayer, "a-layer")
	if _, err := r.Chain(ctx, a); !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
	code := repo(t, mem, "app", core.KindCode, "")
	bad := repo(t, mem, "bad", core.KindCode, "app")
	_ = code
	if _, err := r.Chain(ctx, bad); err == nil || !strings.Contains(err.Error(), "not a layer") {
		t.Fatalf("want not-a-layer error, got %v", err)
	}
	ghost := repo(t, mem, "ghost", core.KindCode, "nope-layer")
	if _, err := r.Chain(ctx, ghost); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want missing error, got %v", err)
	}
}

func TestMissingRefAddsNothing(t *testing.T) {
	mem := store.NewMemory()
	repo(t, mem, "empty-layer", core.KindLayer, "")
	app := repo(t, mem, "app", core.KindCode, "empty-layer@canary")
	eff, err := (&Resolver{Meta: mem, Objects: mem}).Resolve(context.Background(), app, "main")
	if err != nil || len(eff.Files) != 0 || !eff.Chain[0].Missing {
		t.Fatalf("%+v %v", eff, err)
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, p string
		want   bool
	}{
		{"ci/**", "ci/a.yml", true},
		{"ci/**", "ci/x/y/z.yml", true},
		{"ci/**", "cix/a.yml", false},
		{"**/*.md", "README.md", true},
		{"**/*.md", "docs/a/b.md", true},
		{"*.md", "docs/b.md", false},
		{".github/workflows/*", ".github/workflows/ci.yml", true},
		{"AGENTS.md", "AGENTS.md", true},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.p); got != c.want {
			t.Errorf("Match(%q, %q) = %v", c.pat, c.p, got)
		}
	}
}

func TestManifestValidation(t *testing.T) {
	for _, bad := range []string{`{"default":"maybe"}`, `{"rules":[{"path":"","mode":"locked"}]}`, `{"rules":[{"path":"x","mode":"nope"}]}`, `{"extra":1}`, `[`} {
		if _, err := ParseManifest([]byte(bad)); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: want invalid, got %v", bad, err)
		}
	}
	m, err := ParseManifest([]byte(`{"rules":[{"path":"ci/**","mode":"locked"}]}`))
	if err != nil || m.ModeFor("ci/a") != Locked || m.ModeFor("README.md") != Override {
		t.Fatalf("%+v %v", m, err)
	}
}
