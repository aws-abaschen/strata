package cli_test

import (
	"strings"
	"testing"
)

func TestAuditLedger(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.ok(dir, "ana@example.com", "repo", "create", "payments")
	h.ok(dir, "ana@example.com", "init", "--repo", "payments")
	write(t, dir, map[string]string{"a.txt": "1"})
	h.ok(dir, "ana@example.com", "push", "-m", "PAY-1 first")
	write(t, dir, map[string]string{"a.txt": "2"})
	h.ok(dir, "bo@example.com", "push", "-m", "PAY-2 second")
	h.ok(dir, "lead@example.com", "annotate", "--label", "reviewed")

	out := h.ok(dir, "auditor@example.com", "audit")
	for _, want := range []string{"RepoCreated", "PAY-1 first", "bo@example.com", "ChangeAnnotated", "verified 4 records of payments"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}

	// Someone with database access rewrites who made change #3: detected.
	h.mem.TamperAudit("payments", 3, "ana@example.com")
	out, code := h.run(dir, "auditor@example.com", "audit", "--verify")
	if code == 0 || !strings.Contains(out, "FAILED verification") || !strings.Contains(out, "seq 3 was altered") {
		t.Fatalf("tampering not detected (exit %d):\n%s", code, out)
	}
}
