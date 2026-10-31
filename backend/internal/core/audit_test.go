package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func chain(t *testing.T, n int) []AuditRecord {
	t.Helper()
	var out []AuditRecord
	var seq int64
	var prev Hash
	for i := range n {
		e, _ := NewAuditEntry("ChangeCreated", "ana@example.com", time.Date(2026, 10, 31, 10, i, 0, 0, time.UTC), map[string]any{"i": i, "html": "<b>&"})
		r, err := NextAuditRecord("payments", seq, prev, e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
		seq, prev = r.Seq, r.Hash
	}
	return out
}

func TestAuditChainVerifies(t *testing.T) {
	recs := chain(t, 5)
	last, err := VerifyAudit(recs, "")
	if err != nil || last != recs[4].Hash {
		t.Fatalf("%v %v", last, err)
	}
	// A JSON round trip (store, API, CLI) keeps it verifiable.
	b, _ := json.Marshal(recs)
	var back []AuditRecord
	json.Unmarshal(b, &back)
	if _, err := VerifyAudit(back, ""); err != nil {
		t.Fatal(err)
	}
	// A page in the middle verifies against the previous hash.
	if _, err := VerifyAudit(recs[2:], recs[1].Hash); err != nil {
		t.Fatal(err)
	}
}

func TestAuditDetectsTampering(t *testing.T) {
	cases := map[string]func([]AuditRecord) []AuditRecord{
		"altered": func(r []AuditRecord) []AuditRecord { r[2].Actor = "mallory"; return r },
		"detail":  func(r []AuditRecord) []AuditRecord { r[2].Detail = json.RawMessage(`{"i":99}`); return r },
		"removed": func(r []AuditRecord) []AuditRecord { return append(r[:2], r[3:]...) },
		"swapped": func(r []AuditRecord) []AuditRecord { r[1], r[2] = r[2], r[1]; return r },
	}
	for name, mutate := range cases {
		if _, err := VerifyAudit(mutate(chain(t, 5)), ""); err == nil || !strings.HasPrefix(err.Error(), "audit:") {
			t.Errorf("%s: want audit error, got %v", name, err)
		}
	}
}
