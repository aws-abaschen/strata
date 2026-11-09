package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/cli"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

const (
	mAna = "ana@example.com"
	mBob = "bob@example.com"
)

// mergeRepo is a repository with main (ana's working copy) and feat (bob's),
// both starting from the same first change.
type mergeRepo struct {
	h        *harness
	c        *cli.Client
	main, fd string
}

func newMergeRepo(t *testing.T, files map[string]string) *mergeRepo {
	t.Helper()
	h := newHarness(t)
	m := &mergeRepo{h: h, c: &cli.Client{API: h.srv.URL, Token: mAna}, main: t.TempDir()}
	h.ok(m.main, mAna, "repo", "create", "app")
	h.ok(m.main, mAna, "init", "--repo", "app")
	write(t, m.main, files)
	h.ok(m.main, mAna, "push", "-m", "APP-1 start")
	m.fd = m.branch(t, "feat")
	return m
}

// branch creates ref name at main's tip and returns a working copy of it.
func (m *mergeRepo) branch(t *testing.T, name string) string {
	t.Helper()
	tip := m.tip(t, "main")
	if _, err := m.c.MoveRef(context.Background(), "app", name, tip, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m.h.ok(dir, mBob, "clone", "app", "wc", "--ref", name)
	return filepath.Join(dir, "wc")
}

func (m *mergeRepo) tip(t *testing.T, ref string) core.Hash {
	t.Helper()
	r, _, err := m.c.GetRef(context.Background(), "app", ref)
	if err != nil {
		t.Fatal(err)
	}
	return r.Target
}

// conflict merges feat into main and expects a refusal that names want and
// leaves main where it was.
func (m *mergeRepo) conflict(t *testing.T, want string) string {
	t.Helper()
	before := m.tip(t, "main")
	out, code := m.h.run(m.main, mAna, "merge", "feat")
	if code == 0 || !strings.Contains(out, want) {
		t.Fatalf("want a refusal with %q (exit %d):\n%s", want, code, out)
	}
	if m.tip(t, "main") != before {
		t.Fatal("a refused merge moved main")
	}
	return out
}

// Each side deletes a different file of one directory: the merge leaves no
// empty directory behind, exactly like a push of the same files.
func TestServerMergeDropsEmptyDirectories(t *testing.T) {
	m := newMergeRepo(t, map[string]string{"d/x.txt": "x\n", "d/y.txt": "y\n", "keep.txt": "k\n"})
	ctx := context.Background()
	os.Remove(filepath.Join(m.main, "d", "x.txt"))
	m.h.ok(m.main, mAna, "push", "-m", "APP-2 drop x")
	os.Remove(filepath.Join(m.fd, "d", "y.txt"))
	m.h.ok(m.fd, mBob, "push", "-m", "APP-3 drop y")
	m.h.ok(m.main, mAna, "merge", "feat")

	d, err := m.c.GetChange(ctx, "app", m.tip(t, "main"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := m.h.mem.Get(ctx, "app", d.Change.Tree, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var tree core.Tree
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree.Entries) != 1 || tree.Entries[0].Name != "keep.txt" {
		t.Fatalf("merged tree should hold only keep.txt, got %s", raw)
	}
	// The same files pushed from a working copy give the same tree.
	other := t.TempDir()
	m.h.ok(other, mAna, "repo", "create", "twin")
	m.h.ok(other, mAna, "init", "--repo", "twin")
	write(t, other, map[string]string{"keep.txt": "k\n"})
	m.h.ok(other, mAna, "push", "-m", "APP-9 twin")
	tw, _, err := m.c.GetRef(ctx, "twin", "main")
	if err != nil {
		t.Fatal(err)
	}
	td, err := m.c.GetChange(ctx, "twin", tw.Target)
	if err != nil {
		t.Fatal(err)
	}
	if td.Change.Tree != d.Change.Tree {
		t.Fatalf("merge tree %s differs from a push of the same files %s", d.Change.Tree.Short(), td.Change.Tree.Short())
	}
}

func TestServerMergeConflictKinds(t *testing.T) {
	t.Run("deleted on one side, changed on the other", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"a.txt": "a\n", "keep.txt": "k\n"})
		os.Remove(filepath.Join(m.main, "a.txt"))
		m.h.ok(m.main, mAna, "push", "-m", "APP-2 drop a")
		write(t, m.fd, map[string]string{"a.txt": "changed\n"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-3 change a")
		m.conflict(t, "CONFLICT a.txt: deleted on one side, changed on the other")
	})
	t.Run("a file on one side, a directory on the other", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"keep.txt": "k\n"})
		write(t, m.main, map[string]string{"thing": "a file\n"})
		m.h.ok(m.main, mAna, "push", "-m", "APP-2 thing is a file")
		write(t, m.fd, map[string]string{"thing/inner.txt": "a directory\n"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-3 thing is a directory")
		m.conflict(t, "CONFLICT thing: a file on one side, a directory on the other")
	})
	t.Run("binary changed on both sides", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"blob.bin": "\x00\x01base", "keep.txt": "k\n"})
		write(t, m.main, map[string]string{"blob.bin": "\x00\x01main"})
		m.h.ok(m.main, mAna, "push", "-m", "APP-2 main blob")
		write(t, m.fd, map[string]string{"blob.bin": "\x00\x01feat"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-3 feat blob")
		m.conflict(t, "CONFLICT blob.bin: binary file changed on both sides")
	})
	t.Run("added on both sides", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"keep.txt": "k\n"})
		write(t, m.main, map[string]string{"new.txt": "main\n"})
		m.h.ok(m.main, mAna, "push", "-m", "APP-2 main adds")
		write(t, m.fd, map[string]string{"new.txt": "feat\n"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-3 feat adds")
		m.conflict(t, "CONFLICT new.txt: added on both sides with different content")
	})
	t.Run("the executable bit and the content merge separately", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no executable bit on Windows")
		}
		m := newMergeRepo(t, map[string]string{"run.sh": "one\ntwo\nthree\n", "keep.txt": "k\n"})
		if err := os.Chmod(filepath.Join(m.main, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		m.h.ok(m.main, mAna, "push", "-m", "APP-2 make it executable")
		write(t, m.fd, map[string]string{"run.sh": "one\ntwo\nTHREE\n"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-3 edit it")
		m.h.ok(m.main, mAna, "merge", "feat")
		m.h.ok(m.main, mAna, "pull")
		if s := readFile(t, m.main, "run.sh"); s != "one\ntwo\nTHREE\n" {
			t.Fatalf("run.sh = %q", s)
		}
		st, err := os.Stat(filepath.Join(m.main, "run.sh"))
		if err != nil || st.Mode()&0o100 == 0 {
			t.Fatalf("run.sh lost its executable bit: %v %v", st.Mode(), err)
		}
	})
}

// A file past the server's size limit is reported as a limit, not as a
// conflict: editing the branch would not help.
func TestServerMergeReportsLimits(t *testing.T) {
	big := func(first string) string {
		var b bytes.Buffer
		b.WriteString(first + "\n")
		for b.Len() < 1<<20+1024 {
			b.WriteString("the same line on every side\n")
		}
		return b.String()
	}
	m := newMergeRepo(t, map[string]string{"big.txt": big("base"), "keep.txt": "k\n"})
	write(t, m.main, map[string]string{"big.txt": big("main")})
	m.h.ok(m.main, mAna, "push", "-m", "APP-2 main edits big")
	write(t, m.fd, map[string]string{"big.txt": big("feat") + "tail\n"})
	m.h.ok(m.fd, mBob, "push", "-m", "APP-3 feat edits big")
	out := m.conflict(t, "LIMIT big.txt: changed on both sides and too large to merge on the server")
	if strings.Contains(out, "CONFLICT") || !strings.Contains(out, "1 file past what the server merges") {
		t.Fatalf("a limit was reported as a conflict:\n%s", out)
	}
	res, err := m.c.Merge(context.Background(), "app", cli.MergeRequest{Source: "feat", Target: "main", DryRun: true})
	if err != nil || len(res.Conflicts) != 1 || !res.Conflicts[0].Limit {
		t.Fatalf("dry run: %+v, %v", res, err)
	}
}

// Histories the server does not merge are refused with the reason.
func TestServerMergeRefusedHistories(t *testing.T) {
	t.Run("no common history", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"keep.txt": "k\n"})
		other := t.TempDir()
		m.h.ok(other, mBob, "init", "--repo", "app")
		write(t, other, map[string]string{"elsewhere.txt": "e\n"})
		m.h.ok(other, mBob, "push", "--review", "island", "-m", "APP-2 unrelated")
		out, code := m.h.run(m.main, mAna, "merge", "review/island")
		if code == 0 || !strings.Contains(out, "share no history") {
			t.Fatalf("unrelated histories (exit %d):\n%s", code, out)
		}
	})
	t.Run("criss-cross", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"keep.txt": "k\n"})
		write(t, m.main, map[string]string{"m.txt": "m\n"})
		m.h.ok(m.main, mAna, "push", "-m", "APP-2 main side")
		write(t, m.fd, map[string]string{"f.txt": "f\n"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-3 feat side")
		ctx := context.Background()
		// Keep both tips, then merge each into the other.
		if _, err := m.c.MoveRef(ctx, "app", "main0", m.tip(t, "main"), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := m.c.MoveRef(ctx, "app", "feat0", m.tip(t, "feat"), ""); err != nil {
			t.Fatal(err)
		}
		m.h.ok(m.main, mAna, "merge", "feat0")
		m.h.ok(m.main, mAna, "merge", "main0", "--into", "feat")
		before := m.tip(t, "main")
		out, code := m.h.run(m.main, mAna, "merge", "feat")
		if code == 0 || !strings.Contains(out, "criss-cross") {
			t.Fatalf("criss-cross (exit %d):\n%s", code, out)
		}
		if m.tip(t, "main") != before {
			t.Fatal("a refused merge moved main")
		}
	})
	t.Run("too far apart", func(t *testing.T) {
		m := newMergeRepo(t, map[string]string{"keep.txt": "k\n"})
		write(t, m.fd, map[string]string{"f.txt": "f\n"})
		m.h.ok(m.fd, mBob, "push", "-m", "APP-2 feat side")
		for i := 0; i < 205; i++ {
			write(t, m.main, map[string]string{"n.txt": fmt.Sprintf("%d\n", i)})
			m.h.ok(m.main, mAna, "push", "-m", fmt.Sprintf("APP-3 step %d", i))
		}
		// feat into main reads feat's short history first and finds the base.
		// main into feat has to read main's 205 changes: past the limit.
		before := m.tip(t, "feat")
		out, code := m.h.run(m.main, mAna, "merge", "main", "--into", "feat")
		if code == 0 || !strings.Contains(out, "changes apart") {
			t.Fatalf("too far apart (exit %d):\n%s", code, out)
		}
		if m.tip(t, "feat") != before {
			t.Fatal("a refused merge moved feat")
		}
	})
}

// A review ref that holds an earlier merge result is not replaced: its
// approvals would be stranded. A ref the merge contains still moves forward.
func TestServerMergeKeepsReviewRef(t *testing.T) {
	m := newMergeRepo(t, map[string]string{"a.txt": "1\n2\n3\n4\n5\n"})
	write(t, m.main, map[string]string{"a.txt": "one\n2\n3\n4\n5\n"})
	m.h.ok(m.main, mAna, "push", "-m", "APP-2 main side")
	write(t, m.fd, map[string]string{"a.txt": "1\n2\n3\n4\nfive\n"})
	m.h.ok(m.fd, mBob, "push", "-m", "APP-3 feat side")

	m.h.ok(m.main, mAna, "merge", "feat", "--to", "review/merge-feat")
	first := m.tip(t, "review/merge-feat")
	out, code := m.h.run(m.main, mAna, "merge", "feat", "--to", "review/merge-feat")
	if code == 0 || !strings.Contains(out, "already holds") {
		t.Fatalf("second merge into the same review ref (exit %d):\n%s", code, out)
	}
	if m.tip(t, "review/merge-feat") != first {
		t.Fatal("the review ref was replaced")
	}
	// A dry run still answers, and another name works.
	m.h.ok(m.main, mAna, "merge", "feat", "--to", "review/merge-feat", "--dry-run")
	m.h.ok(m.main, mAna, "merge", "feat", "--to", "review/merge-feat-2")

	// A result ref that is behind the merge moves forward.
	if _, err := m.c.MoveRef(context.Background(), "app", "staging", m.tip(t, "main"), ""); err != nil {
		t.Fatal(err)
	}
	m.h.ok(m.main, mAna, "merge", "feat", "--to", "staging")
	if d, err := m.c.GetChange(context.Background(), "app", m.tip(t, "staging")); err != nil || len(d.Change.Parents) != 2 {
		t.Fatalf("staging should hold the merge change: %+v %v", d.Change, err)
	}
}
