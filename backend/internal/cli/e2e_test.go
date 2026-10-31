package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/api"
	"github.com/aws-abaschen/strata/backend/internal/cli"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// recorder keeps the actions of committed audit records, which the ledger
// Lambda publishes as events in AWS.
type recorder struct{ types []string }

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	mem    *store.Memory
	events *recorder
}

// newHarness serves the API and the memory store's upload URLs. The bearer
// token stands in for the verified identity.
func newHarness(t *testing.T) *harness {
	mem := store.NewMemory()
	ev := &recorder{}
	mem.OnAudit = func(r core.AuditRecord) { ev.types = append(ev.types, r.Action) }
	a := &api.API{Objects: mem, Meta: mem, WebURL: "https://strata.example.com"}
	h := a.Handler()
	mux := http.NewServeMux()
	mux.Handle("/_objects/", mem.UploadHandler())
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.Header.Get("authorization"), "Bearer ")
		h.ServeHTTP(w, r.WithContext(api.WithPrincipal(r.Context(), api.ParseDevToken(id))))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mem.BaseURL = srv.URL
	return &harness{t: t, srv: srv, mem: mem, events: ev}
}

// run executes a strata command in dir as user and returns its output.
func (h *harness) run(dir, user string, args ...string) (string, int) {
	var out bytes.Buffer
	env := cli.Env{Dir: dir, Out: &out, Getenv: func(k string) string {
		switch k {
		case "STRATA_API":
			return h.srv.URL
		case "STRATA_TOKEN":
			return user
		}
		return ""
	}}
	code := cli.Run(context.Background(), args, env)
	return out.String(), code
}

func (h *harness) ok(dir, user string, args ...string) string {
	h.t.Helper()
	out, code := h.run(dir, user, args...)
	if code != 0 {
		h.t.Fatalf("strata %v: exit %d\n%s", args, code, out)
	}
	return out
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	a := t.TempDir()
	h.ok(a, "ana@example.com", "repo", "create", "payments", "-d", "Payments service")
	h.ok(a, "ana@example.com", "init", "--repo", "payments")
	write(t, a, map[string]string{
		"README.md":         "# payments\n",
		"src/round.go":      "package src\n",
		"src/deep/x.txt":    "x",
		"node_modules/a.js": "ignored",
		"build/out.bin":     "ignored by .strataignore",
		".strataignore":     "build/\n*.log\n",
		"debug.log":         "ignored",
	})

	out := h.ok(a, "ana@example.com", "push", "-m", "PAY-42 first cut", "--label", "Risk:Low", "--attr", "approver=bob")
	if !strings.Contains(out, "jira:PAY-42") || !strings.Contains(out, "#risk:low") || !strings.Contains(out, "approver=bob") {
		t.Fatalf("push output missing metadata:\n%s", out)
	}
	st1, _, _ := cli.LoadState(a)
	if !strings.Contains(h.ok(a, "ana@example.com", "status"), "clean") {
		t.Fatal("status should be clean after push")
	}

	write(t, a, map[string]string{"src/round.go": "package src\n// v2\n"})
	h.ok(a, "ana@example.com", "push", "-m", "OPS-7 second", "--work-item", "servicenow:CHG0031337")
	st2, _, _ := cli.LoadState(a)

	// Amend keeps the change id and replaces HEAD on the ref.
	write(t, a, map[string]string{"src/round.go": "package src\n// v3\n"})
	h.ok(a, "ana@example.com", "push", "-m", "OPS-7 second, fixed", "--amend")
	st3, _, _ := cli.LoadState(a)
	if st3.ChangeID != st2.ChangeID || st3.Head == st2.Head {
		t.Fatalf("amend: change id %s -> %s, head %s -> %s", st2.ChangeID, st3.ChangeID, st2.Head, st3.Head)
	}
	log := h.ok(a, "ana@example.com", "log")
	if strings.Count(log, "change c") != 2 || !strings.Contains(log, "OPS-7 second, fixed") || strings.Contains(log, "OPS-7 second\n") {
		t.Fatalf("log after amend:\n%s", log)
	}

	// Clone reproduces the tree exactly, ignored files excluded.
	b := filepath.Join(t.TempDir(), "copy")
	h.ok(t.TempDir(), "bo@example.com", "clone", "payments", b)
	got, _ := os.ReadFile(filepath.Join(b, "src", "round.go"))
	if string(got) != "package src\n// v3\n" {
		t.Fatalf("cloned content %q", got)
	}
	for _, p := range []string{"node_modules", "build", "debug.log"} {
		if _, err := os.Stat(filepath.Join(b, p)); err == nil {
			t.Errorf("%s should not have been pushed", p)
		}
	}

	// Work tracking after the fact: annotate the first change.
	h.ok(a, "lead@example.com", "annotate", string(st1.Head), "--work-item", "SEC-9", "--note", "reviewed for SEC-9")
	find := h.ok(a, "x@example.com", "find", "--work-item", "SEC-9")
	if !strings.Contains(find, st1.Head.Short()) || !strings.Contains(find, "via annotation") {
		t.Fatalf("find SEC-9:\n%s", find)
	}
	if !strings.Contains(h.ok(a, "x@example.com", "find", "--work-item", "PAY-42"), st1.Head.Short()) {
		t.Fatal("PAY-42 should find the first change")
	}
	if !strings.Contains(h.ok(a, "x@example.com", "find", "--label", "risk:low"), "payments") {
		t.Fatal("label search")
	}
	show := h.ok(a, "x@example.com", "show", string(st1.Head))
	if !strings.Contains(show, "jira:SEC-9") || !strings.Contains(show, "reviewed for SEC-9") {
		t.Fatalf("show should merge annotation metadata:\n%s", show)
	}

	// Concurrent writers: B is now behind A, so its push must be rejected.
	h.ok(a, "ana@example.com", "push", "-m", "PAY-43 third")
	write(t, b, map[string]string{"new.txt": "from bo"})
	if out, code := h.run(b, "bo@example.com", "push", "-m", "PAY-44 stale"); code == 0 || !strings.Contains(out, "moved on the server") {
		t.Fatalf("stale push should fail, got %d:\n%s", code, out)
	}

	// Author comes from the verified identity, never from the client.
	d, _ := (&cli.Client{API: h.srv.URL, Token: "x@example.com"}).GetChange(context.Background(), "payments", st1.Head)
	if d.Change.Author != "ana@example.com" {
		t.Fatalf("author %q", d.Change.Author)
	}
	want := []string{"RepoCreated", "ChangeCreated", "ChangeCreated", "ChangeCreated", "ChangeAnnotated", "ChangeCreated"}
	if strings.Join(h.events.types, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v", h.events.types)
	}
}

func TestServerRejectsIncompleteSnapshot(t *testing.T) {
	h := newHarness(t)
	c := &cli.Client{API: h.srv.URL, Token: "ana@example.com"}
	ctx := context.Background()
	if _, err := c.CreateRepo(ctx, "infra", ""); err != nil {
		t.Fatal(err)
	}
	// A tree that points at a file nobody uploaded.
	tree := core.Tree{Entries: []core.TreeEntry{{Name: "a", Kind: core.KindFile, Hash: core.HashBytes([]byte("never uploaded"))}}}
	b, th, _ := tree.Encode()
	h.mem.Put("infra", b)
	_, err := c.CreateChange(ctx, "infra", cli.NewChange{Tree: th, Message: "broken", Parents: []core.Hash{}, Ref: "main"})
	if ae, ok := err.(*cli.APIError); !ok || ae.Status != 400 || !strings.Contains(ae.Message, "missing") {
		t.Fatalf("want 400 missing objects, got %v", err)
	}
}

func TestUploadMustMatchHash(t *testing.T) {
	h := newHarness(t)
	c := &cli.Client{API: h.srv.URL, Token: "ana@example.com"}
	ctx := context.Background()
	c.CreateRepo(ctx, "docs", "")
	good := []byte("hello")
	ups, err := c.Negotiate(ctx, "docs", []cli.ObjectSize{{Hash: core.HashBytes(good), Size: 5}})
	if err != nil || len(ups) != 1 {
		t.Fatalf("negotiate: %v %v", ups, err)
	}
	if err := c.Upload(ctx, ups[0], strings.NewReader("evil!"), 5); err == nil {
		t.Fatal("tampered upload must be rejected")
	}
	if err := c.Upload(ctx, ups[0], bytes.NewReader(good), 5); err != nil {
		t.Fatal(err)
	}
	if ups, _ := c.Negotiate(ctx, "docs", []cli.ObjectSize{{Hash: core.HashBytes(good), Size: 5}}); len(ups) != 0 {
		t.Fatal("stored objects must not be asked for again")
	}
}

func TestUnauthenticated(t *testing.T) {
	h := newHarness(t)
	_, err := (&cli.Client{API: h.srv.URL}).ListRepos(context.Background())
	if ae, ok := err.(*cli.APIError); !ok || ae.Status != 401 {
		t.Fatalf("want 401, got %v", err)
	}
}
