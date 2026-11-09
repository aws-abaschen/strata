package cli_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/cli"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Server-side merges: fast-forward, a clean three-way merge with a merge
// change, up to date, conflicts that write nothing, and a dry run. The
// merge change exports to Git with two parents and verifies offline.
func TestServerMerge(t *testing.T) {
	h := newHarness(t)
	const ana, bob = "ana@example.com", "bob@example.com"
	ctx := context.Background()
	c := &cli.Client{API: h.srv.URL, Token: ana}

	main := t.TempDir()
	h.ok(main, ana, "repo", "create", "app")
	h.ok(main, ana, "init", "--repo", "app")
	write(t, main, map[string]string{"notes.txt": "one\ntwo\nthree\nfour\nfive\n", "keep.txt": "k\n"})
	h.ok(main, ana, "push", "-m", "APP-1 start")

	// bob's branch starts from main.
	start, _, err := c.GetRef(ctx, "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.MoveRef(ctx, "app", "feat", start.Target, ""); err != nil {
		t.Fatal(err)
	}
	fdir := t.TempDir()
	h.ok(fdir, bob, "clone", "app", "f", "--ref", "feat")
	fd := filepath.Join(fdir, "f")

	// Fast-forward: feat is ahead, main has not moved.
	write(t, fd, map[string]string{"feature.txt": "new\n"})
	h.ok(fd, bob, "push", "-m", "APP-2 feature file")
	if out := h.ok(main, ana, "merge", "feat", "--dry-run"); !strings.Contains(out, "would be fast-forwarded") {
		t.Fatalf("dry-run fast-forward:\n%s", out)
	}
	if out := h.ok(main, ana, "merge", "feat"); !strings.Contains(out, "main fast-forwarded") {
		t.Fatalf("fast-forward:\n%s", out)
	}
	if out := h.ok(main, ana, "merge", "feat"); !strings.Contains(out, "already contains") {
		t.Fatalf("up to date:\n%s", out)
	}
	h.ok(main, ana, "pull")

	// Both sides move, on different lines of the same file: a merge change.
	write(t, main, map[string]string{"notes.txt": "ONE\ntwo\nthree\nfour\nfive\n"})
	h.ok(main, ana, "push", "-m", "APP-3 shout one")
	write(t, fd, map[string]string{"notes.txt": "one\ntwo\nthree\nfour\nFIVE\n"})
	h.ok(fd, bob, "push", "-m", "APP-4 shout five")
	if out := h.ok(main, ana, "merge", "feat", "--dry-run"); !strings.Contains(out, "merges cleanly") {
		t.Fatalf("dry-run merge:\n%s", out)
	}
	out := h.ok(main, ana, "merge", "feat")
	if !strings.Contains(out, "merged feat into main") {
		t.Fatalf("merge:\n%s", out)
	}
	tip, _, _ := c.GetRef(ctx, "app", "main")
	d, err := c.GetChange(ctx, "app", tip.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Change.Parents) != 2 || d.Change.Author != ana || d.Change.Message != "Merge feat into main" {
		t.Fatalf("merge change: %+v", d.Change)
	}
	if got := fmt.Sprint(d.Change.Metadata.WorkItems); !strings.Contains(got, "APP-4") {
		t.Fatalf("merge should link the branch's work items, got %s", got)
	}
	h.ok(main, ana, "pull")
	if s := readFile(t, main, "notes.txt"); s != "ONE\ntwo\nthree\nfour\nFIVE\n" {
		t.Fatalf("merged notes.txt = %q", s)
	}
	if s := readFile(t, main, "feature.txt"); s != "new\n" {
		t.Fatalf("feature.txt = %q", s)
	}
	audit := h.ok(main, ana, "audit", "-n", "100")
	if !strings.Contains(audit, "ChangeCreated") {
		t.Fatalf("audit:\n%s", audit)
	}

	// The same line on both sides: conflicts, nothing written.
	write(t, main, map[string]string{"keep.txt": "main's\n"})
	h.ok(main, ana, "push", "-m", "APP-5 main side")
	h.ok(fd, bob, "pull")
	write(t, fd, map[string]string{"keep.txt": "feat's\n"})
	h.ok(fd, bob, "push", "-m", "APP-6 feat side")
	before, _, _ := c.GetRef(ctx, "app", "main")
	out, code := h.run(main, ana, "merge", "feat")
	if code == 0 || !strings.Contains(out, "CONFLICT keep.txt") || !strings.Contains(out, "nothing was merged") {
		t.Fatalf("conflict (exit %d):\n%s", code, out)
	}
	after, _, _ := c.GetRef(ctx, "app", "main")
	if after.Target != before.Target {
		t.Fatal("a conflicting merge moved main")
	}
	if out := h.ok(main, ana, "merge", "feat", "--dry-run"); !strings.Contains(out, "CONFLICT keep.txt") {
		t.Fatalf("dry-run conflict:\n%s", out)
	}
	if _, code := h.run(main, ana, "merge", "nope"); code == 0 {
		t.Fatal("merging a missing ref should fail")
	}

	// The merge change exports with two Git parents and verifies offline.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	job := &anchor.Job{Meta: h.mem, Archive: tableArchive{h.mem}, Signer: anchor.LocalSigner{Key: key, ID: "test"}}
	if _, err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	keyFile := filepath.Join(t.TempDir(), "anchor.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)
	exp := filepath.Join(t.TempDir(), "export")
	h.ok(main, ana, "export", "--out", exp)
	if res, code := h.run(t.TempDir(), bob, "export", "verify", exp, "--anchor-key", keyFile); code != 0 {
		t.Fatalf("verify (exit %d):\n%s", code, res)
	}
	if py := pythonVerifier(t, keyFile); py != nil {
		if o, code := py(exp); code != 0 {
			t.Fatalf("Python verifier (exit %d):\n%s", code, o)
		}
	}
	if git, err := exec.LookPath("git"); err == nil {
		o, err := exec.Command(git, "-C", filepath.Join(exp, "git"), "log", "--merges", "--format=%P", "main").CombinedOutput()
		if err != nil || len(strings.Fields(string(o))) != 2 {
			t.Fatalf("git merge commit parents: %v\n%s", err, o)
		}
	}
}

// With four eyes on main, a merge goes to a review ref, is approved by
// someone who wrote none of it, then promoted.
func TestServerMergeFourEyes(t *testing.T) {
	h := newHarness(t)
	const (
		root = "root@example.com;strata-admins"
		ann  = "ann@example.com"
		bob  = "bob@example.com"
		cy   = "cy@example.com"
	)
	ctx := context.Background()
	installPolicy(t, h, root, fourEyesPolicy)
	h.ok(t.TempDir(), ann, "repo", "create", "app")
	dir := t.TempDir()
	h.ok(dir, ann, "init", "--repo", "app")
	write(t, dir, map[string]string{"a.txt": "1\n2\n3\n4\n5\n"})
	r1 := reviewRev(t, h.ok(dir, ann, "push", "--review", "first", "-m", "APP-1 first"))
	h.ok(dir, bob, "approve", r1)
	h.ok(dir, ann, "promote", r1, "--ref", "main")

	// ann's branch, and main moves on under it.
	c := &cli.Client{API: h.srv.URL, Token: ann}
	if _, err := c.MoveRef(ctx, "app", "feat", core.Hash(r1), ""); err != nil {
		t.Fatal(err)
	}
	fdir := t.TempDir()
	h.ok(fdir, ann, "clone", "app", "f", "--ref", "feat")
	fd := filepath.Join(fdir, "f")
	write(t, fd, map[string]string{"a.txt": "1\n2\n3\n4\nfive\n"})
	h.ok(fd, ann, "push", "-m", "APP-2 feature")
	write(t, dir, map[string]string{"a.txt": "one\n2\n3\n4\n5\n"})
	r2 := reviewRev(t, h.ok(dir, ann, "push", "--review", "second", "-m", "APP-3 main moves"))
	h.ok(dir, cy, "approve", r2)
	h.ok(dir, ann, "promote", r2, "--ref", "main")

	// Straight into main: a new change has no approvals.
	if out, code := h.run(dir, bob, "merge", "feat"); code == 0 || !strings.Contains(out, "forbidden by") {
		t.Fatalf("direct merge into protected main (exit %d):\n%s", code, out)
	}
	out := h.ok(dir, bob, "merge", "feat", "--to", "review/merge-feat")
	m := reviewRev(t, out)
	h.ok(dir, ann, "approve", m) // ann wrote the branch: does not count
	if out, code := h.run(dir, bob, "promote", m, "--ref", "main"); code == 0 {
		t.Fatalf("the branch author's approval counted:\n%s", out)
	}
	h.ok(dir, cy, "approve", m)
	h.ok(dir, bob, "promote", m, "--ref", "main")
	check := t.TempDir()
	h.ok(check, cy, "clone", "app", "c")
	if s := readFile(t, filepath.Join(check, "c"), "a.txt"); s != "one\n2\n3\n4\nfive\n" {
		t.Fatalf("a.txt = %q", s)
	}
}
