package cli_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/store"
)

// A standby region serves reads and answers writes with 421 and the
// writer's URL; the CLI follows it, so a client pointed at either region
// works and its writes land in the writer only.
func TestCLIFollowsTheWriter(t *testing.T) {
	west, central := newHarness(t), newHarness(t)
	west.mem.Region, west.mem.Primary = "eu-west-1", "eu-west-1"
	central.mem.Region, central.mem.Primary = "eu-central-1", "eu-west-1"
	w := store.Writer{Region: "eu-west-1", Epoch: 1, API: west.srv.URL}
	for _, m := range []*store.Memory{west.mem, central.mem} {
		if err := m.SetWriter(context.Background(), w, 0); err != nil {
			t.Fatal(err)
		}
	}
	const ana = "ana@example.com"

	// The writer URL comes from a table item: it is followed only to a
	// known peer, never wherever the item says.
	out, code := central.run(t.TempDir(), ana, "repo", "create", "payments")
	if code == 0 || !strings.Contains(out, "STRATA_PEER_APIS") || !strings.Contains(out, west.srv.URL) {
		t.Fatalf("followed an unlisted writer URL (exit %d):\n%s", code, out)
	}
	central.env = map[string]string{"STRATA_PEER_APIS": west.srv.URL}

	// Writes sent to the standby end up in the writer, uploads included.
	dir := t.TempDir()
	central.ok(dir, ana, "repo", "create", "payments")
	if _, err := central.mem.GetRepo(context.Background(), "payments"); err == nil {
		t.Fatal("the standby wrote")
	}
	west.mem.ReplicateTo(central.mem) // replication catches up (seconds in AWS)
	central.ok(dir, ana, "init", "--repo", "payments")
	write(t, dir, map[string]string{"a.txt": "hello\n"})
	central.ok(dir, ana, "push", "-m", "PAY-1 first")
	if recs, _ := west.mem.ListAudit(context.Background(), "payments", 0, 10); len(recs) != 2 {
		t.Fatalf("writer has %d records, want 2", len(recs))
	}
	if recs, _ := central.mem.ListAudit(context.Background(), "payments", 0, 10); len(recs) != 1 {
		t.Fatalf("standby has %d records before replication, want 1", len(recs))
	}

	// Once replicated, the standby serves reads itself.
	west.mem.ReplicateTo(central.mem)
	if out := central.ok(dir, "auditor@example.com", "audit", "--verify"); !strings.Contains(out, "verified 2 records") {
		t.Fatalf("standby read:\n%s", out)
	}

	// Without a writer URL to follow, the standby's refusal is clear.
	central.mem.SetWriter(context.Background(), store.Writer{Region: "eu-west-1", Epoch: 2}, 1)
	out, code = central.run(dir, ana, "annotate", "--note", "x")
	if code == 0 || !strings.Contains(out, "eu-central-1 is not the writer region") {
		t.Fatalf("standby without writer URL (exit %d):\n%s", code, out)
	}
}

// GET /health answers without sign-in: 200 in the writer region, 503
// elsewhere, so DNS failover on the shared API domain follows the writer.
func TestHealthFollowsTheWriter(t *testing.T) {
	west, central := newHarness(t), newHarness(t)
	west.mem.Region, west.mem.Primary = "eu-west-1", "eu-west-1"
	central.mem.Region, central.mem.Primary = "eu-central-1", "eu-west-1"
	status := func(h *harness) int {
		res, err := http.Get(h.srv.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if status(west) != 200 || status(central) != 503 {
		t.Fatalf("before any writer item: west %d, central %d", status(west), status(central))
	}
	w := store.Writer{Region: "eu-central-1", Epoch: 2}
	west.mem.SetWriter(context.Background(), w, 0)
	central.mem.SetWriter(context.Background(), w, 0)
	if status(west) != 503 || status(central) != 200 {
		t.Fatalf("after switchover: west %d, central %d", status(west), status(central))
	}
}
