package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPullMergesConcurrentWork(t *testing.T) {
	h := newHarness(t)
	const ana, bo = "ana@example.com", "bo@example.com"
	a := t.TempDir()
	h.ok(a, ana, "repo", "create", "payments")
	h.ok(a, ana, "init", "--repo", "payments")
	write(t, a, map[string]string{
		"round.go":  "package pay\n\nfunc Round() {}\n\nfunc Fee() {}\n",
		"README.md": "# Payments\n",
		"old.txt":   "to delete",
		"logo.bin":  "\x00v1",
	})
	h.ok(a, ana, "push", "-m", "PAY-1 base")

	b := filepath.Join(t.TempDir(), "b")
	h.ok(t.TempDir(), bo, "clone", "payments", b)

	// Bo changes the top of round.go, deletes old.txt, adds new.txt, changes logo.
	write(t, b, map[string]string{
		"round.go": "package pay\n\n// Round uses half-even.\nfunc Round() {}\n\nfunc Fee() {}\n",
		"new.txt":  "from bo",
		"logo.bin": "\x00bo",
	})
	os.Remove(filepath.Join(b, "old.txt"))
	h.ok(b, bo, "push", "-m", "PAY-2 bo's work")

	// Meanwhile Ana edits the bottom of round.go and the README, and the logo.
	write(t, a, map[string]string{
		"round.go":  "package pay\n\nfunc Round() {}\n\nfunc Fee() int { return 1 }\n",
		"README.md": "# Payments\nOwned by ana.\n",
		"logo.bin":  "\x00ana",
	})
	if out, code := h.run(a, ana, "push", "-m", "PAY-3 ana's work"); code == 0 || !strings.Contains(out, "run strata pull") {
		t.Fatalf("stale push: %d\n%s", code, out)
	}

	out := h.ok(a, ana, "pull")
	if !strings.Contains(out, "2 updated, 1 merged, 1 conflicts") || !strings.Contains(out, "CONFLICT logo.bin") {
		t.Fatalf("pull output:\n%s", out)
	}
	if got := read(t, filepath.Join(a, "round.go")); got != "package pay\n\n// Round uses half-even.\nfunc Round() {}\n\nfunc Fee() int { return 1 }\n" {
		t.Fatalf("merged round.go = %q", got)
	}
	if read(t, filepath.Join(a, "new.txt")) != "from bo" || read(t, filepath.Join(a, "README.md")) != "# Payments\nOwned by ana.\n" {
		t.Fatal("pull should bring bo's new file and keep ana's README")
	}
	if _, err := os.Stat(filepath.Join(a, "old.txt")); err == nil {
		t.Fatal("old.txt was deleted on the server")
	}

	// The binary conflict blocks the push until resolved.
	if out, code := h.run(a, ana, "push", "-m", "PAY-3 ana's work"); code == 0 || !strings.Contains(out, "unresolved conflicts in [logo.bin]") {
		t.Fatalf("push with conflicts: %d\n%s", code, out)
	}
	os.Remove(filepath.Join(a, "logo.bin.theirs")) // keep ana's logo
	h.ok(a, ana, "push", "-m", "PAY-3 ana's work")

	// Bo pulls: a text clash this time.
	write(t, b, map[string]string{"README.md": "# Payments\nOwned by bo.\n"})
	out = h.ok(b, bo, "pull")
	if !strings.Contains(out, "CONFLICT README.md") {
		t.Fatalf("bo pull:\n%s", out)
	}
	readme := read(t, filepath.Join(b, "README.md"))
	if !strings.Contains(readme, "<<<<<<< yours\nOwned by bo.\n=======\nOwned by ana.\n>>>>>>> server") {
		t.Fatalf("conflict markers:\n%s", readme)
	}
	write(t, b, map[string]string{"README.md": "# Payments\nOwned by ana and bo.\n"})
	h.ok(b, bo, "push", "-m", "PAY-4 shared ownership")

	// History is linear: each push built on the previous one.
	log := h.ok(b, bo, "log")
	for _, want := range []string{"PAY-4", "PAY-3", "PAY-2", "PAY-1"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %s:\n%s", want, log)
		}
	}
	if !strings.Contains(h.ok(a, ana, "pull"), "1 updated") || read(t, filepath.Join(a, "README.md")) != "# Payments\nOwned by ana and bo.\n" {
		t.Fatal("ana should get bo's resolution")
	}
	if !strings.Contains(h.ok(a, ana, "pull"), "already up to date") {
		t.Fatal("second pull should be a no-op")
	}
}
