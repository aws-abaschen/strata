package cli_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/cli"
)

// A repository with amends, annotations, approvals, a review ref that was
// deleted and a Layer setting exports to Git plus evidence, verifies
// offline, and every kind of tampering is caught with its own message.
func TestExportRoundTrip(t *testing.T) {
	h := newHarness(t)
	const ana, bob = "ana@example.com", "bob@example.com"
	const root = "root@example.com;strata-admins"

	base := t.TempDir()
	h.ok(base, ana, "repo", "create", "base", "--layer")
	h.ok(base, ana, "init", "--repo", "base")
	write(t, base, map[string]string{"policy.yaml": "x: 1\n"})
	h.ok(base, ana, "push", "-m", "OPS-1 base")

	dir := t.TempDir()
	h.ok(dir, ana, "repo", "create", "payments", "--extends", "base")
	h.ok(dir, ana, "init", "--repo", "payments")
	write(t, dir, map[string]string{"a.txt": "v1\n", "docs/R&D notes.md": "# notes\n", "empty/.keep": ""})
	h.ok(dir, ana, "push", "-m", "PAY-1 first: R&D <draft> café   done")
	write(t, dir, map[string]string{"a.txt": "v2\n"})
	h.ok(dir, ana, "push", "-m", "PAY-2 second")
	write(t, dir, map[string]string{"a.txt": "v2 amended\n"})
	h.ok(dir, ana, "push", "--amend", "-m", "PAY-2 second, amended")
	h.ok(dir, bob, "annotate", "--note", "reviewed <ok> & signé", "--label", "risk:low")
	h.ok(dir, bob, "approve", "--note", "fine")
	write(t, dir, map[string]string{"b.txt": "review me\n"})
	h.ok(dir, ana, "push", "--review", "feature", "-m", "PAY-3 on review")
	c := &cli.Client{API: h.srv.URL, Token: ana}
	if err := c.DeleteRef(context.Background(), "payments", "review/feature"); err != nil {
		t.Fatal(err)
	}
	h.ok(dir, ana, "layers", "unset")

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	job := &anchor.Job{Meta: h.mem, Archive: tableArchive{h.mem}, Signer: anchor.LocalSigner{Key: key, ID: "test"}}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A second anchor, so verifiers walk the anchor chain too.
	h.ok(dir, bob, "annotate", "--label", "audited")
	if rep, err := job.Run(context.Background()); err != nil || rep.Anchored == 0 {
		t.Fatalf("second anchor: %+v %v", rep, err)
	}
	// Break-glass activity moves only that chain's head: anchor 3 carries no
	// payments proof, so payments' anchors are 1 and 2.
	h.ok(t.TempDir(), root, "breakglass", "start", "--reason", "export test incident", "--work-item", "INC-7", "--repo", "payments", "--for", "10m")
	h.ok(t.TempDir(), root, "breakglass", "end")
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	keyFile := filepath.Join(t.TempDir(), "anchor.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)

	out := filepath.Join(t.TempDir(), "export")
	got := h.ok(dir, root, "export", "--out", out, "--break-glass")
	// 4 revisions: first, second, its amend, and the deleted review change.
	if !strings.Contains(got, "exported payments") || !strings.Contains(got, "4 changes") || !strings.Contains(got, "1 refs") {
		t.Fatalf("export:\n%s", got)
	}
	verify := func(d string) (string, int) {
		return h.run(t.TempDir(), "nobody@example.com", "export", "verify", d, "--anchor-key", keyFile)
	}
	res, code := verify(out)
	if code != 0 || !strings.Contains(res, "4 changes") || !strings.Contains(res, "2 annotations, 1 approvals") ||
		!strings.Contains(res, "signatures: verified with the pinned anchor key") || !strings.Contains(res, "anchored 2 times") || !strings.Contains(res, "verified 2 break-glass records") {
		t.Fatalf("verify (exit %d):\n%s", code, res)
	}

	// The independent verifier, written from the spec in Python, agrees.
	py := pythonVerifier(t, keyFile)
	if py != nil {
		if o, code := py(out); code != 0 || !strings.Contains(o, "4 changes") || !strings.Contains(o, "2 anchors") || !strings.Contains(o, "2 break-glass records") || !strings.Contains(o, "verified with the pinned key") {
			t.Fatalf("Python verifier (exit %d):\n%s", code, o)
		}
	}

	// Exports are reproducible: a second run gives the same Git commits.
	out2 := filepath.Join(t.TempDir(), "export")
	h.ok(dir, root, "export", "--out", out2, "--break-glass")
	if a, b := gitRef(t, out, "git/refs/heads/main"), gitRef(t, out2, "git/refs/heads/main"); a != b {
		t.Fatalf("not reproducible: %s vs %s", a, b)
	}

	// Plain Git can read it: fsck passes and a clone has the amended content.
	if git, err := exec.LookPath("git"); err == nil {
		if o, err := exec.Command(git, "-C", filepath.Join(out, "git"), "fsck", "--strict", "--no-dangling").CombinedOutput(); err != nil {
			t.Fatalf("git fsck: %v\n%s", err, o)
		}
		clone := filepath.Join(t.TempDir(), "clone")
		if o, err := exec.Command(git, "clone", "-q", filepath.Join(out, "git"), clone).CombinedOutput(); err != nil {
			t.Fatalf("git clone: %v\n%s", err, o)
		}
		if s := readFile(t, clone, "a.txt"); s != "v2 amended\n" {
			t.Fatalf("clone a.txt = %q", s)
		}
		if o, _ := exec.Command(git, "-C", clone, "log", "-1", "--format=%an%n%B").CombinedOutput(); !strings.Contains(string(o), "ana@example.com") || !strings.Contains(string(o), "Strata-Revision: ") {
			t.Fatalf("git log:\n%s", o)
		}
	}

	// Each tampering is caught, with a message naming what is wrong.
	revs := func(d string) []string {
		ents, _ := os.ReadDir(filepath.Join(d, "strata", "changes"))
		var out []string
		for _, e := range ents {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
		return out
	}
	for _, tc := range []struct {
		name, want string
		tamper     func(d string)
	}{
		{"file content in Git", "content changed", func(d string) {
			replaceObject(t, d, "blob", "v1\n", "v9\n")
		}},
		{"superseded revision dropped", "is missing from strata/changes", func(d string) {
			r := revs(d)[0]
			os.Remove(filepath.Join(d, "strata", "changes", r+".json"))
			os.Remove(filepath.Join(d, "git", "refs", "strata", "revisions", r))
		}},
		{"byte in a change", "does not hash to its revision", func(d string) {
			p := filepath.Join(d, "strata", "changes", revs(d)[1]+".json")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(b, []byte("ana@"), []byte("eve@"), 1), 0o644)
		}},
		{"branch moved", "does not point at", func(d string) {
			r := revs(d)[0]
			sha := gitRef(t, d, "git/refs/strata/revisions/"+r)
			if sha == gitRef(t, d, "git/refs/heads/main") {
				sha = gitRef(t, d, "git/refs/strata/revisions/"+revs(d)[1])
			}
			os.WriteFile(filepath.Join(d, "git", "refs", "heads", "main"), []byte(sha), 0o644)
		}},
		{"deleted ref resurrected", "does not leave standing", func(d string) {
			os.MkdirAll(filepath.Join(d, "git", "refs", "heads", "review"), 0o755)
			os.WriteFile(filepath.Join(d, "git", "refs", "heads", "review", "feature"), []byte(gitRef(t, d, "git/refs/heads/main")), 0o644)
		}},
		{"approval removed", "approval", func(d string) {
			os.WriteFile(filepath.Join(d, "strata", "approvals.jsonl"), nil, 0o644)
		}},
		{"ledger record edited", "ledger", func(d string) {
			p := filepath.Join(d, "strata", "audit.jsonl")
			b, _ := os.ReadFile(p)
			os.WriteFile(p, bytes.Replace(b, []byte(`"actor":"bob@example.com"`), []byte(`"actor":"eve@example.com"`), 1), 0o644)
		}},
		{"anchors stripped", "carries no anchors", func(d string) {
			os.WriteFile(filepath.Join(d, "strata", "anchors.json"), []byte("[]"), 0o644)
		}},
		{"ref hidden as omitted", "Git can hold", func(d string) {
			p := filepath.Join(d, "strata", "manifest.json")
			var m map[string]any
			json.Unmarshal(mustRead(t, p), &m)
			m["omittedRefs"] = []string{"main"}
			b, _ := json.Marshal(m)
			os.WriteFile(p, b, 0o644)
			os.Remove(filepath.Join(d, "git", "refs", "heads", "main"))
		}},
		{"break-glass ledger removed", "break-glass ledger is included, but it is missing", func(d string) {
			os.RemoveAll(filepath.Join(d, "strata", "break-glass"))
		}},
		{"ledger tail cut", "anchors", func(d string) {
			p := filepath.Join(d, "strata", "audit.jsonl")
			lines := strings.SplitAfter(strings.TrimSpace(string(mustRead(t, p))), "\n")
			os.WriteFile(p, []byte(strings.Join(lines[:len(lines)-1], "")), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "x")
			if err := os.CopyFS(d, os.DirFS(out)); err != nil {
				t.Fatal(err)
			}
			tc.tamper(d)
			res, code := verify(d)
			t.Log(strings.TrimSpace(res))
			if code == 0 || !strings.Contains(res, "FAILED") || !strings.Contains(res, tc.want) {
				t.Fatalf("tampering not caught (exit %d), want %q:\n%s", code, tc.want, res)
			}
			if py != nil {
				if o, code := py(d); code != 1 || !strings.Contains(o, "FAILED") {
					t.Fatalf("Python verifier missed it (exit %d):\n%s", code, o)
				} else {
					t.Log("python: " + strings.TrimSpace(o))
				}
			}
		})
	}
}

// replaceObject overwrites the loose object for content old with new bytes,
// keeping its name, as someone editing the Git repository by hand would.
func replaceObject(t *testing.T, d, kind, old, new string) {
	t.Helper()
	raw := func(s string) []byte { return []byte(fmt.Sprintf("%s %d\x00%s", kind, len(s), s)) }
	sum := sha1.Sum(raw(old))
	sha := hex.EncodeToString(sum[:])
	p := filepath.Join(d, "git", "objects", sha[:2], sha[2:])
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write(raw(new))
	zw.Close()
	os.Chmod(p, 0o644)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	return string(mustRead(t, filepath.Join(root, filepath.FromSlash(rel))))
}

// gitRef reads a loose ref file.
func gitRef(t *testing.T, root, rel string) string {
	t.Helper()
	return strings.TrimSpace(readFile(t, root, rel))
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pythonVerifier runs tools/verify_export.py, or returns nil without Python.
// STRATA_REQUIRE_PYTHON=1 (CI) makes a missing interpreter a failure.
func pythonVerifier(t *testing.T, keyFile string) func(dir string) (string, int) {
	t.Helper()
	var py string
	for _, n := range []string{"python3", "python"} {
		if p, err := exec.LookPath(n); err == nil && exec.Command(p, "-c", "import sys; assert sys.version_info >= (3, 8)").Run() == nil {
			py = p
			break
		}
	}
	if py == "" {
		if os.Getenv("STRATA_REQUIRE_PYTHON") != "" {
			t.Fatal("python 3 not found")
		}
		t.Log("python 3 not found: skipping the independent verifier")
		return nil
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "..", "tools", "verify_export.py"))
	args := []string{"-I", script}
	if _, err := exec.LookPath("openssl"); err == nil {
		args = append(args, "--anchor-key", keyFile)
	}
	return func(dir string) (string, int) {
		o, err := exec.Command(py, append(append([]string{}, args...), dir)...).CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(o), code
	}
}
