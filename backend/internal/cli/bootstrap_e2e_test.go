package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/cli"
)

// Production starts closed: until policies exist only strata-admins can do
// anything, and the policy repository can never be left without a main.
func TestClosedBootstrap(t *testing.T) {
	h := newHarnessMode(t, false)
	const (
		root = "root@example.com;strata-admins"
		dan  = "dan@example.com;payments-devs"
	)
	policy, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "policies", "strata.cedar"))
	if err != nil {
		t.Fatal(err)
	}
	c := &cli.Client{API: h.srv.URL, Token: dan}

	// A signed-in non-admin can do nothing, and is told why.
	if out, code := h.run(t.TempDir(), dan, "repo", "create", "payments"); code == 0 || !strings.Contains(out, "may not create") {
		t.Fatalf("non-admin create: %d\n%s", code, out)
	}
	me, err := c.Me(context.Background())
	if err != nil || me.Authorization != "not-configured" {
		t.Fatalf("/me = %+v %v", me, err)
	}

	// An admin bootstraps the policy repository.
	pol := t.TempDir()
	h.ok(pol, root, "repo", "create", "strata-policies")
	h.ok(pol, root, "init", "--repo", "strata-policies")
	write(t, pol, map[string]string{"strata.cedar": string(policy)})
	h.ok(pol, root, "push", "-m", "SEC-1 initial policies")
	if me, _ := c.Me(context.Background()); me.Authorization != "policies" {
		t.Fatalf("/me after bootstrap = %+v", me)
	}

	// Now the policies decide: anyone may create code repos.
	h.ok(t.TempDir(), dan, "repo", "create", "payments")

	// Main of the policy repo cannot be deleted, not even by an admin: that
	// would reopen the bootstrap window.
	admin := &cli.Client{API: h.srv.URL, Token: root}
	if err := admin.DeleteRef(context.Background(), "strata-policies", "main"); err == nil || !strings.Contains(err.Error(), "cannot be deleted") {
		t.Fatalf("deleting policy main: %v", err)
	}
}
