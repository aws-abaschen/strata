package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicyAsCode(t *testing.T) {
	h := newHarness(t)
	const (
		root = "root@example.com;strata-admins"
		pat  = "pat@example.com;platform"
		dan  = "dan@example.com;payments-devs"
		eve  = "eve@example.com;marketing-devs"
		kim  = "kim@example.com;crypto"
	)
	policy, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "policies", "strata.cedar"))
	if err != nil {
		t.Fatal(err)
	}

	// An admin installs the policies (bootstrap: no policies yet).
	pol := t.TempDir()
	h.ok(pol, root, "repo", "create", "strata-policies")
	h.ok(pol, root, "init", "--repo", "strata-policies")
	write(t, pol, map[string]string{"strata.cedar": string(policy)})
	h.ok(pol, root, "push", "-m", "SEC-1 initial policies")

	// A broken policy file is refused, so nobody gets locked out.
	write(t, pol, map[string]string{"broken.cedar": "permit ("})
	if out, code := h.run(pol, pat, "push", "-m", "SEC-2 oops"); code == 0 || !strings.Contains(out, "broken.cedar") {
		t.Fatalf("invalid policy should be rejected, got %d:\n%s", code, out)
	}
	os.Remove(filepath.Join(pol, "broken.cedar"))

	// Only the platform team creates Layers.
	h.ok(t.TempDir(), pat, "repo", "create", "payments-rules", "--layer")
	if out, code := h.run(t.TempDir(), dan, "repo", "create", "rogue-rules", "--layer"); code == 0 || !strings.Contains(out, "may not create rogue-rules") {
		t.Fatalf("dev creating a layer: %d\n%s", code, out)
	}

	// Payments devs write repos under payments-rules; others may not.
	h.ok(t.TempDir(), dan, "repo", "create", "cards", "--extends", "payments-rules")
	cards := t.TempDir()
	h.ok(cards, dan, "init", "--repo", "cards")
	write(t, cards, map[string]string{"main.go": "package main"})
	h.ok(cards, dan, "push", "-m", "CARD-1 skeleton")
	write(t, cards, map[string]string{"main.go": "package main // eve"})
	if out, code := h.run(cards, eve, "push", "-m", "MKT-1 drive-by"); code == 0 || !strings.Contains(out, "may not write cards") {
		t.Fatalf("eve writing cards: %d\n%s", code, out)
	}
	// ...but anyone may annotate (link tickets) and read.
	h.ok(cards, eve, "annotate", "--work-item", "MKT-9")
	h.ok(cards, eve, "log")
	// ...and only owners change settings.
	if out, code := h.run(cards, eve, "layers", "unset"); code == 0 || !strings.Contains(out, "may not admin cards") {
		t.Fatalf("eve detaching cards: %d\n%s", code, out)
	}
	// Admins have no standing access: an emergency fix needs a grant.
	if out, code := h.run(cards, root, "push", "-m", "OPS-1 emergency fix"); code == 0 || !strings.Contains(out, "may not write cards") {
		t.Fatalf("admin without a grant: %d\n%s", code, out)
	}
	h.ok(cards, root, "breakglass", "start", "--reason", "cards outage, hotfix", "--work-item", "INC-1", "--repo", "cards", "--for", "30m")
	h.ok(cards, root, "push", "-m", "OPS-1 emergency fix")
	h.ok(cards, root, "breakglass", "end")

	// hsm-keys is invisible to everyone outside the crypto team.
	h.ok(t.TempDir(), kim, "repo", "create", "hsm-keys")
	hsm := t.TempDir()
	h.ok(hsm, kim, "init", "--repo", "hsm-keys")
	write(t, hsm, map[string]string{"keys.txt": "secret"})
	h.ok(hsm, kim, "push", "-m", "SEC-5 keys", "--label", "secret")
	if strings.Contains(h.ok(t.TempDir(), eve, "repo", "list"), "hsm-keys") {
		t.Fatal("eve should not see hsm-keys in the list")
	}
	if strings.Contains(h.ok(t.TempDir(), root, "repo", "list"), "hsm-keys") {
		t.Fatal("the forbid applies to admins too")
	}
	if out, code := h.run(t.TempDir(), eve, "clone", "hsm-keys"); code == 0 || !strings.Contains(out, "not found") {
		t.Fatalf("eve cloning hsm-keys: %d\n%s", code, out)
	}
	if strings.Contains(h.ok(t.TempDir(), eve, "find", "--label", "secret"), "hsm-keys") {
		t.Fatal("search must not leak hsm-keys")
	}
	if !strings.Contains(h.ok(t.TempDir(), kim, "find", "--label", "secret"), "hsm-keys") {
		t.Fatal("kim should find her own change")
	}
}
