package cli_test

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

const fourEyesPolicy = `
permit (principal, action, resource);
forbid (principal, action == Action::"write", resource)
when { context.ref == "main" &&
       (context.approvals < 1 || !context.fastForward || !context.hasWorkItem) };
`

var revRe = regexp.MustCompile(`strata approve ([0-9a-f]{64})`)

func reviewRev(t *testing.T, out string) string {
	t.Helper()
	m := revRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no revision in review output:\n%s", out)
	}
	return m[1]
}

func installPolicy(t *testing.T, h *harness, user, policy string) {
	t.Helper()
	pol := t.TempDir()
	h.ok(pol, user, "repo", "create", "strata-policies")
	h.ok(pol, user, "init", "--repo", "strata-policies")
	write(t, pol, map[string]string{"rules.cedar": policy})
	h.ok(pol, user, "push", "-m", "SEC-1 policies")
}

func TestFourEyes(t *testing.T) {
	h := newHarness(t)
	const (
		root = "root@example.com;strata-admins"
		ann  = "ann@example.com"
		bob  = "bob@example.com"
		cy   = "cy@example.com"
	)
	installPolicy(t, h, root, fourEyesPolicy)

	h.ok(t.TempDir(), ann, "repo", "create", "app")
	annDir := t.TempDir()
	h.ok(annDir, ann, "init", "--repo", "app")
	write(t, annDir, map[string]string{"main.go": "package main"})

	// A direct push to main has no approvals.
	if out, code := h.run(annDir, ann, "push", "-m", "APP-1 first"); code == 0 || !strings.Contains(out, "forbidden by") {
		t.Fatalf("direct push to main: %d\n%s", code, out)
	}

	// Review flow: push to a review ref, a colleague approves, promote.
	r1 := reviewRev(t, h.ok(annDir, ann, "push", "--review", "first", "-m", "APP-1 first"))
	if out, code := h.run(annDir, ann, "approve", r1); code == 0 || !strings.Contains(out, "cannot approve") {
		t.Fatalf("self-approval: %d\n%s", code, out)
	}
	if out, code := h.run(annDir, ann, "promote", r1, "--ref", "main"); code == 0 {
		t.Fatalf("promote without approval: %d\n%s", code, out)
	}
	h.ok(annDir, bob, "approve", r1, "--note", "looks right")
	if out, code := h.run(annDir, bob, "approve", r1); code == 0 || !strings.Contains(out, "already approved") {
		t.Fatalf("double approval: %d\n%s", code, out)
	}
	h.ok(annDir, ann, "promote", r1, "--ref", "main")

	// The ledger record of the promotion names the approvers.
	audit := h.ok(annDir, ann, "audit", "-n", "50")
	if !strings.Contains(audit, "ApprovalGranted") || !strings.Contains(audit, "RefUpdated") {
		t.Fatalf("audit missing approval records:\n%s", audit)
	}

	// An admin cannot approve their own change either.
	rootDir := t.TempDir()
	h.ok(rootDir, root, "clone", "app", "rootapp")
	rd := rootDir + "/rootapp"
	write(t, rd, map[string]string{"main.go": "package main // root"})
	rr := reviewRev(t, h.ok(rd, root, "push", "--review", "ops", "-m", "APP-2 ops"))
	if out, code := h.run(rd, root, "approve", rr); code == 0 || !strings.Contains(out, "cannot approve") {
		t.Fatalf("admin self-approval: %d\n%s", code, out)
	}

	// A/B/A: ann writes c1, bob stacks c2 on it, ann's approval of c2 does
	// not count because she authored part of the range.
	write(t, annDir, map[string]string{"main.go": "package main // v2"})
	c1 := reviewRev(t, h.ok(annDir, ann, "push", "--review", "x", "-m", "APP-3 part one"))
	bobDir := t.TempDir()
	h.ok(bobDir, bob, "clone", "app", "bobapp", "--ref", "review/x")
	bd := bobDir + "/bobapp"
	write(t, bd, map[string]string{"main.go": "package main // v3"})
	c2 := reviewRev(t, h.ok(bd, bob, "push", "--review", "y", "-m", "APP-3 part two"))
	_ = c1
	h.ok(annDir, ann, "approve", c2)
	if out, code := h.run(bd, bob, "promote", c2, "--ref", "main"); code == 0 {
		t.Fatalf("A/B/A should not count ann's approval: %d\n%s", code, out)
	}
	h.ok(annDir, cy, "approve", c2)
	h.ok(bd, bob, "promote", c2, "--ref", "main")

	// Rollback to an older approved revision is not a fast-forward.
	if out, code := h.run(annDir, ann, "promote", r1, "--ref", "main"); code == 0 || !strings.Contains(out, "forbidden by") {
		t.Fatalf("rollback: %d\n%s", code, out)
	}

	// Deleting main is a move that is not a fast-forward.
	req, _ := http.NewRequest(http.MethodPut, h.srv.URL+"/repos/app/refs/main", strings.NewReader(`{"target":"","expected":"`+c2+`"}`))
	req.Header.Set("authorization", "Bearer "+root)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("deleting main as admin: status %d", resp.StatusCode)
	}
}

func TestPolicyEvaluationErrorDenies(t *testing.T) {
	h := newHarness(t)
	const root = "root@example.com;strata-admins"
	// `context.nope` does not exist: Cedar skips an erroring forbid, which
	// would fail open. Strata denies instead.
	installPolicy(t, h, root, `permit (principal, action, resource);
forbid (principal, action, resource) when { context.nope == 1 };`)
	out, code := h.run(t.TempDir(), "pat@example.com", "repo", "create", "xx")
	if code == 0 || !strings.Contains(out, "evaluation error") {
		t.Fatalf("erroring forbid must deny: %d\n%s", code, out)
	}
}

// postJSON calls the API as user and decodes the reply into out.
func (h *harness) postJSON(user, method, path, body string, out any) int {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("authorization", "Bearer "+user)
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// A merge brings in the other branch's changes: their authors cannot be
// the second pair of eyes on it, even though its first parent is main.
func TestFourEyesMergeCountsBranchAuthors(t *testing.T) {
	h := newHarness(t)
	const (
		root = "root@example.com;strata-admins"
		ann  = "ann@example.com"
		bob  = "bob@example.com"
		cy   = "cy@example.com"
	)
	installPolicy(t, h, root, fourEyesPolicy)
	h.ok(t.TempDir(), ann, "repo", "create", "app")
	annDir := t.TempDir()
	h.ok(annDir, ann, "init", "--repo", "app")
	write(t, annDir, map[string]string{"main.go": "package main"})
	r1 := reviewRev(t, h.ok(annDir, ann, "push", "--review", "first", "-m", "APP-1 first"))
	h.ok(annDir, bob, "approve", r1)
	h.ok(annDir, ann, "promote", r1, "--ref", "main")

	write(t, annDir, map[string]string{"main.go": "package main // feature"})
	feat := reviewRev(t, h.ok(annDir, ann, "push", "--review", "feat", "-m", "APP-2 feature"))
	var fc struct {
		Change struct{ Tree string } `json:"change"`
	}
	h.postJSON(ann, http.MethodGet, "/repos/app/changes/"+feat, "", &fc)

	var merge struct{ Revision string }
	body := `{"tree":"` + fc.Change.Tree + `","parents":["` + r1 + `","` + feat + `"],"message":"APP-2 merge feat","ref":"review/m","expectedRef":""}`
	if code := h.postJSON(bob, http.MethodPost, "/repos/app/changes", body, &merge); code != http.StatusCreated {
		t.Fatalf("create merge change: %d", code)
	}
	h.ok(annDir, ann, "approve", merge.Revision)
	if out, code := h.run(annDir, bob, "promote", merge.Revision, "--ref", "main"); code == 0 {
		t.Fatalf("ann approved a merge of her own branch and it counted:\n%s", out)
	}
	h.ok(annDir, cy, "approve", merge.Revision)
	h.ok(annDir, bob, "promote", merge.Revision, "--ref", "main")
}
