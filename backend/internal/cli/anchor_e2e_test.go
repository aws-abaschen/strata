package cli_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// tableArchive pretends the Object Lock archive matches the table.
type tableArchive struct{ mem *store.Memory }

func (a tableArchive) Get(ctx context.Context, repo string, seq int64) (core.AuditRecord, error) {
	recs, err := a.mem.ListAudit(ctx, repo, seq-1, 1)
	if err != nil || len(recs) == 0 {
		return core.AuditRecord{}, store.ErrNotFound
	}
	return recs[0], nil
}

func (tableArchive) PutAnchor(context.Context, core.Anchor) error { return nil }

// The CLI checks the ledger against signed anchors with a key pinned on the
// reader's side, and catches a rewrite that leaves the chain itself consistent.
func TestAuditVerifiesAnchors(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.ok(dir, "ana@example.com", "repo", "create", "payments")
	h.ok(dir, "ana@example.com", "init", "--repo", "payments")
	write(t, dir, map[string]string{"a.txt": "1"})
	h.ok(dir, "ana@example.com", "push", "-m", "PAY-1 first")

	if out := h.ok(dir, "auditor@example.com", "audit", "--verify"); !strings.Contains(out, "not anchored yet") {
		t.Fatalf("before any anchor:\n%s", out)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	job := &anchor.Job{Meta: h.mem, Archive: tableArchive{h.mem}, Signer: anchor.LocalSigner{Key: key, ID: "test"}}
	if rep, err := job.Run(context.Background()); err != nil || rep.Anchored != 1 {
		t.Fatalf("anchor: %+v %v", rep, err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	keyFile := filepath.Join(t.TempDir(), "anchor.pem")
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644)

	out := h.ok(dir, "auditor@example.com", "audit", "--verify")
	if !strings.Contains(out, "anchored 1 times, through seq 2") || !strings.Contains(out, "signatures: NOT checked") {
		t.Fatalf("unpinned:\n%s", out)
	}
	out = h.ok(dir, "auditor@example.com", "audit", "--verify", "--anchor-key", keyFile)
	if !strings.Contains(out, "signatures: verified with the pinned anchor key") {
		t.Fatalf("pinned:\n%s", out)
	}

	// Rewrite history and re-chain it: the chain verifies, the anchor does not.
	h.mem.RewriteAuditTail("payments", 2, "mallory@example.com")
	out, code := h.run(dir, "auditor@example.com", "audit", "--verify", "--anchor-key", keyFile)
	if code == 0 || !strings.Contains(out, "verified 2 records") || !strings.Contains(out, "FAILED anchor verification") {
		t.Fatalf("rewrite not caught (exit %d):\n%s", code, out)
	}
}
