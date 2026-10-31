package cli_test

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/cli"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Everyone may work, main needs four eyes, admins included, and auditors
// see break-glass grants.
const breakGlassPolicy = `
permit (principal, action in [Action::"read", Action::"write", Action::"create", Action::"annotate", Action::"approve", Action::"admin"], resource);
forbid (principal, action == Action::"write", resource)
when { context.ref == "main" &&
       (context.approvals < 1 || !context.fastForward || !context.hasWorkItem) };
permit (principal in Group::"auditors", action == Action::"audit", resource == Repo::"_breakglass");
`

var grantRe = regexp.MustCompile(`break-glass (bg-\S+) active`)

func grantID(t *testing.T, out string) string {
	t.Helper()
	m := grantRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no grant id in:\n%s", out)
	}
	return m[1]
}

func (h *harness) fails(dir, user, want string, args ...string) {
	h.t.Helper()
	out, code := h.run(dir, user, args...)
	if code == 0 || !strings.Contains(out, want) {
		h.t.Fatalf("strata %v as %s: want failure containing %q, got %d:\n%s", args, user, want, code, out)
	}
}

func TestBreakGlass(t *testing.T) {
	h := newHarnessMode(t, false)
	const (
		root = "root@example.com;strata-admins"
		sec  = "sec@example.com;strata-admins"
		dan  = "dan@example.com;payments-devs"
		aud  = "aud@example.com;auditors"
	)
	installPolicy(t, h, root, breakGlassPolicy)
	for _, repo := range []string{"app", "other"} {
		h.ok(t.TempDir(), dan, "repo", "create", repo)
	}
	app := t.TempDir()
	h.ok(app, root, "init", "--repo", "app")
	write(t, app, map[string]string{"fix.txt": "hotfix"})
	other := t.TempDir()
	h.ok(other, root, "init", "--repo", "other")
	write(t, other, map[string]string{"fix.txt": "hotfix"})

	// Without a grant, policy binds admins like everyone.
	h.fails(app, root, "forbidden by", "push", "-m", "INC-7 hotfix")

	// Only admins activate, and only with a reason, a ticket and a sane duration.
	start := []string{"breakglass", "start", "--reason", "payments down, hotfix needed", "--work-item", "INC-7", "--repo", "app", "--for", "30m"}
	h.fails(t.TempDir(), dan, "only members of strata-admins", start...)
	h.fails(t.TempDir(), root, "reason", "breakglass", "start", "--work-item", "INC-7", "--repo", "app")
	h.fails(t.TempDir(), root, "workItem", "breakglass", "start", "--reason", "payments down, hotfix needed", "--repo", "app")
	h.fails(t.TempDir(), root, "repos", "breakglass", "start", "--reason", "payments down, hotfix needed", "--work-item", "INC-7")
	h.fails(t.TempDir(), root, "minutes", "breakglass", "start", "--reason", "payments down, hotfix needed", "--work-item", "INC-7", "--repo", "app", "--for", "5h")

	id := grantID(t, h.ok(t.TempDir(), root, start...))
	h.fails(t.TempDir(), root, "already hold an active grant", start...)

	// The grant lets the emergency push through, on the listed repo only.
	h.ok(app, root, "push", "-m", "INC-7 hotfix")
	h.fails(other, root, "forbidden by", "push", "-m", "INC-7 hotfix")

	// The write is tagged with the grant in the ledger, and indexed under it.
	recs, err := h.mem.ListAudit(context.Background(), "app", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var tag struct {
		BreakGlass string `json:"breakGlass"`
	}
	json.Unmarshal(recs[len(recs)-1].Detail, &tag)
	if tag.BreakGlass != id {
		t.Fatalf("last app record %s not tagged with %s: %s", recs[len(recs)-1].Action, id, recs[len(recs)-1].Detail)
	}
	if out := h.ok(t.TempDir(), sec, "breakglass", "show", id); !strings.Contains(out, "1 actions") || !strings.Contains(out, "ChangeCreated") {
		t.Fatalf("show:\n%s", out)
	}

	// No review while the grant is active; ending it closes the door.
	h.fails(t.TempDir(), sec, "still active", "breakglass", "review", id, "--outcome", "accepted", "--note", "hotfix matches INC-7")
	h.ok(t.TempDir(), root, "breakglass", "end")
	write(t, app, map[string]string{"fix.txt": "hotfix 2"})
	h.fails(app, root, "forbidden by", "push", "-m", "INC-7 second hotfix")

	// Review: another admin, once, with a note; never the holder.
	h.fails(t.TempDir(), root, "cannot review", "breakglass", "review", id, "--outcome", "accepted", "--note", "hotfix matches INC-7")
	h.fails(t.TempDir(), sec, "note", "breakglass", "review", id, "--outcome", "accepted")
	h.fails(t.TempDir(), sec, "outcome", "breakglass", "review", id, "--note", "hotfix matches INC-7")
	h.ok(t.TempDir(), sec, "breakglass", "review", id, "--outcome", "accepted", "--note", "hotfix matches INC-7, diff checked")
	h.fails(t.TempDir(), sec, "already reviewed", "breakglass", "review", id, "--outcome", "rejected", "--note", "changed my mind")

	// A grant expires on its own, on the server clock.
	id2 := grantID(t, h.ok(t.TempDir(), root, "breakglass", "start", "--reason", "second incident here", "--work-item", "INC-8", "--repo", "*", "--for", "10m"))
	me, err := (&cli.Client{API: h.srv.URL, Token: root}).Me(context.Background())
	if err != nil || me.BreakGlass == nil || me.BreakGlass.ID != id2 {
		t.Fatalf("/me during grant: %+v %v", me, err)
	}
	h.skew.Store(int64(11 * time.Minute))
	h.fails(app, root, "forbidden by", "push", "-m", "INC-8 too late")
	if me, _ := (&cli.Client{API: h.srv.URL, Token: root}).Me(context.Background()); me.BreakGlass != nil {
		t.Fatalf("/me after expiry still shows %s", me.BreakGlass.ID)
	}
	// ...and becomes overdue for review a day later.
	h.skew.Store(int64(25 * time.Hour))
	if out := h.ok(t.TempDir(), sec, "breakglass", "list", "--open"); !strings.Contains(out, id2) || !strings.Contains(out, "OVERDUE") || strings.Contains(out, id) {
		t.Fatalf("open grants:\n%s", out)
	}

	// Auditors see grants and verify the ledger; others see nothing.
	h.ok(t.TempDir(), aud, "breakglass", "list")
	if out := h.ok(t.TempDir(), aud, "breakglass", "audit", "--verify"); !strings.Contains(out, "verified 4 break-glass records") {
		t.Fatalf("break-glass audit:\n%s", out)
	}
	h.fails(t.TempDir(), dan, "visible to strata-admins", "breakglass", "list")

	if got := strings.Join(h.events.types, ","); !strings.Contains(got, "BreakGlassActivated,ChangeCreated") || !strings.Contains(got, "BreakGlassReviewed") {
		t.Fatalf("events: %s", got)
	}
}

// A policy set that forbids everything, admins included, cannot lock the
// organisation out: break-glass is outside Cedar, and the fix is pushed under
// a grant.
func TestBreakGlassRecoversFromLockout(t *testing.T) {
	h := newHarnessMode(t, false)
	const (
		root = "root@example.com;strata-admins"
		dan  = "dan@example.com"
	)
	installPolicy(t, h, root, `forbid (principal, action, resource);`)
	h.fails(t.TempDir(), dan, "may not create", "repo", "create", "app")

	// Admins are locked out too: they cannot even read the policies.
	pol := t.TempDir()
	h.fails(t.TempDir(), root, "not found", "clone", "strata-policies", pol)

	h.ok(t.TempDir(), root, "breakglass", "start", "--reason", "policy push locked everyone out", "--work-item", "SEC-2", "--repo", "strata-policies")
	h.ok(t.TempDir(), root, "clone", "strata-policies", pol)
	write(t, pol, map[string]string{"rules.cedar": `permit (principal, action, resource);`})
	h.ok(pol, root, "push", "-m", "SEC-2 restore access")
	h.ok(t.TempDir(), root, "breakglass", "end")

	h.ok(t.TempDir(), dan, "repo", "create", "app")
	recs, _ := h.mem.ListAudit(context.Background(), core.BreakGlassChain, 0, 10)
	if len(recs) != 2 || recs[0].Action != "BreakGlassActivated" || recs[1].Action != "BreakGlassEnded" {
		t.Fatalf("break-glass ledger: %+v", recs)
	}
}
