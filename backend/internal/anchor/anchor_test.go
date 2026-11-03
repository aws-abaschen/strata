package anchor

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitorus/timestamp"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/store"
)

// fakeTSA issues real RFC 3161 tokens from a throwaway CA.
type fakeTSA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	down bool
}

func newFakeTSA(t *testing.T) (*fakeTSA, *x509.CertPool) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test TSA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &fakeTSA{cert: cert, key: key}, pool
}

func (f *fakeTSA) Stamp(_ context.Context, digest []byte) ([]byte, error) {
	if f.down {
		return nil, errors.New("timestamp authority: HTTP 503")
	}
	ts := timestamp.Timestamp{HashAlgorithm: crypto.SHA256, HashedMessage: digest, Time: time.Now(), Policy: []int{1, 2, 3}, AddTSACertificate: true}
	resp, err := ts.CreateResponseWithOpts(f.cert, f.key, crypto.SHA256)
	if err != nil {
		return nil, err
	}
	parsed, err := timestamp.ParseResponse(resp)
	if err != nil {
		return nil, err
	}
	return parsed.RawToken, nil
}

// memArchive is the Object Lock bucket; drop loses records like a broken ledger Lambda.
type memArchive struct {
	mu      sync.Mutex
	recs    map[string]core.AuditRecord
	anchors map[int64]core.Anchor
	drop    map[string]bool
}

func k(repo string, seq int64) string { return fmt.Sprintf("%s#%d", repo, seq) }

func (a *memArchive) add(r core.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.drop[k(r.Repo, r.Seq)] {
		a.recs[k(r.Repo, r.Seq)] = r
	}
}

func (a *memArchive) Get(_ context.Context, repo string, seq int64) (core.AuditRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.recs[k(repo, seq)]
	if !ok {
		return r, store.ErrNotFound
	}
	return r, nil
}

func (a *memArchive) PutAnchor(_ context.Context, x core.Anchor) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.anchors[x.Statement.Seq]; ok {
		return errors.New("anchor exists")
	}
	a.anchors[x.Statement.Seq] = x
	return nil
}

type fixture struct {
	t       *testing.T
	ctx     context.Context
	mem     *store.Memory
	arch    *memArchive
	tsa     *fakeTSA
	trust   Trust
	job     *Job
	now     time.Time
	counter int
}

func newFixture(t *testing.T) *fixture {
	mem := store.NewMemory()
	arch := &memArchive{recs: map[string]core.AuditRecord{}, anchors: map[int64]core.Anchor{}, drop: map[string]bool{}}
	mem.OnAudit = arch.add
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tsa, roots := newFakeTSA(t)
	f := &fixture{t: t, ctx: context.Background(), mem: mem, arch: arch, tsa: tsa,
		trust: Trust{Keys: []*ecdsa.PublicKey{&key.PublicKey}, TSARoot: roots}, now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	f.job = &Job{Meta: mem, Archive: arch, Signer: LocalSigner{Key: key, ID: "test"}, TSA: tsa, Now: func() time.Time { return f.now }}
	return f
}

// write appends one record to a chain (creating the repo first if needed).
func (f *fixture) write(repo string) {
	f.t.Helper()
	f.counter++
	e, _ := core.NewAuditEntry("RepoUpdated", "ana@example.com", f.now, map[string]int{"n": f.counter})
	if _, err := f.mem.GetRepo(f.ctx, repo); errors.Is(err, store.ErrNotFound) {
		e.Action = "RepoCreated"
		if err := f.mem.CreateRepo(f.ctx, core.Repo{Name: repo}, e); err != nil {
			f.t.Fatal(err)
		}
		return
	}
	r, _ := f.mem.GetRepo(f.ctx, repo)
	if err := f.mem.UpdateRepo(f.ctx, r, nil, e); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) run() Report {
	f.t.Helper()
	f.now = f.now.Add(15 * time.Minute)
	rep, err := f.job.Run(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return rep
}

func (f *fixture) verify(repo string) ([]Checked, error) {
	recs, _ := f.mem.ListAudit(f.ctx, repo, 0, 1000)
	ps, _ := f.mem.ListAnchorProofs(f.ctx, repo, 0, 1000)
	return VerifyChain(recs, ps, f.trust)
}

func TestAnchorsVerifyEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.write("payments")
	f.write("payments")
	f.write("cards")

	if rep := f.run(); rep.Anchored != 1 || rep.Chains != 2 || rep.TimestampFailed {
		t.Fatalf("first run: %+v", rep)
	}
	// Nothing moved: no new anchor, but the archive is reconciled.
	if rep := f.run(); rep.Anchored != 0 || rep.Checked != 3 || len(rep.Gaps)+len(rep.Mismatches)+len(rep.Rewrites) != 0 {
		t.Fatalf("idle run: %+v", rep)
	}
	f.write("payments")
	if rep := f.run(); rep.Anchored != 2 {
		t.Fatalf("after a write: %+v", rep)
	}

	got, err := f.verify("payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[1].SignatureOK || !got[1].TimestampOK || !got[1].TimestampCA || got[1].Leaf.Seq != 3 {
		t.Fatalf("payments proofs: %+v", got)
	}
	// cards did not move, so only anchor 1 covers it.
	if got, err := f.verify("cards"); err != nil || len(got) != 1 {
		t.Fatalf("cards: %+v %v", got, err)
	}

	// Anchors chain: each statement commits to the one before.
	if f.arch.anchors[2].Statement.Prev != f.arch.anchors[1].Statement.DigestHex() {
		t.Fatal("anchor 2 does not link to anchor 1")
	}
	// A daily heartbeat anchors even when nothing moved.
	f.now = f.now.Add(Heartbeat)
	if rep := f.run(); rep.Anchored != 3 {
		t.Fatalf("heartbeat: %+v", rep)
	}
}

func TestRewrittenTailIsCaught(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 4; i++ {
		f.write("payments")
	}
	f.run()

	// Someone with table access rewrites history from seq 2 and re-chains it.
	f.mem.RewriteAuditTail("payments", 2, "mallory@example.com")
	recs, _ := f.mem.ListAudit(f.ctx, "payments", 0, 100)
	if _, err := core.VerifyAudit(recs, ""); err != nil {
		t.Fatalf("the rewrite should be internally consistent: %v", err)
	}

	// The job notices on every run, and never signs over the rewrite, even
	// once more writes land on the forged chain or a heartbeat is due.
	f.write("payments")
	for i := 0; i < 2; i++ {
		f.now = f.now.Add(Heartbeat)
		if rep := f.run(); len(rep.Rewrites) != 1 || rep.Rewrites[0].Seq != 4 || rep.Anchored != 0 {
			t.Fatalf("run %d after rewrite: %+v", i, rep)
		}
	}
	if a, _, _ := f.mem.LatestAnchor(f.ctx); a.Statement.Seq != 1 {
		t.Fatalf("anchored over the rewrite: latest is %d", a.Statement.Seq)
	}
	// ...and any reader verifying against the anchors does too.
	if _, err := f.verify("payments"); err == nil || !strings.Contains(err.Error(), "rewritten") {
		t.Fatalf("verify after rewrite: %v", err)
	}
}

func TestArchiveGapsAndMismatchesAreReported(t *testing.T) {
	f := newFixture(t)
	f.arch.drop[k("payments", 2)] = true
	for i := 0; i < 3; i++ {
		f.write("payments")
	}
	f.write("cards")
	f.run() // anchor 1
	rep := f.run()
	if len(rep.Gaps) != 1 || rep.Gaps[0].Repo != "payments" || rep.Gaps[0].Seq != 2 {
		t.Fatalf("gaps: %+v", rep)
	}
	// Still reported on the next run, until someone fixes it.
	if rep := f.run(); len(rep.Gaps) != 1 {
		t.Fatalf("gap forgotten: %+v", rep)
	}

	// An archive copy that differs from the table.
	f.arch.mu.Lock()
	r := f.arch.recs[k("cards", 1)]
	r.Hash = core.Hash(strings.Repeat("0", 64))
	f.arch.recs[k("cards", 1)] = r
	f.arch.mu.Unlock()
	f.mem.SetAnchorProgress(f.ctx, map[string]int64{})
	if rep := f.run(); len(rep.Mismatches) != 1 || rep.Mismatches[0].Repo != "cards" {
		t.Fatalf("mismatches: %+v", rep)
	}
}

func TestTrustIsPinnedNotServed(t *testing.T) {
	f := newFixture(t)
	f.write("payments")
	f.run()

	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.trust.Keys = []*ecdsa.PublicKey{&other.PublicKey}
	if _, err := f.verify("payments"); err == nil || !strings.Contains(err.Error(), "pinned key") {
		t.Fatalf("wrong key: %v", err)
	}
	_, otherRoots := newFakeTSA(t)
	f.trust = Trust{TSARoot: otherRoots}
	if _, err := f.verify("payments"); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("wrong TSA root: %v", err)
	}
	// With nothing pinned the proof still checks, but says what it could not.
	f.trust = Trust{}
	got, err := f.verify("payments")
	if err != nil || got[0].SignatureOK || got[0].TimestampCA {
		t.Fatalf("unpinned: %+v %v", got, err)
	}
}

func TestTimestampOutageStillAnchors(t *testing.T) {
	f := newFixture(t)
	f.write("payments")
	f.tsa.down = true
	rep := f.run()
	if rep.Anchored != 1 || !rep.TimestampFailed {
		t.Fatalf("outage: %+v", rep)
	}
	got, err := f.verify("payments")
	if err != nil || !got[0].SignatureOK || got[0].TimestampOK || got[0].TimestampErr == "" {
		t.Fatalf("outage proof: %+v %v", got, err)
	}
}

func TestAnchorsNeverFork(t *testing.T) {
	f := newFixture(t)
	f.write("payments")
	f.run()
	// A second writer that read the same latest anchor loses the CAS.
	a, _, _ := f.mem.LatestAnchor(f.ctx)
	if err := f.mem.PutAnchor(f.ctx, a); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("forked anchor: %v", err)
	}
}

// The DR chain (writer changes, drills) is anchored like the others, and its
// name cannot be taken by a repository.
func TestDRChainIsAnchored(t *testing.T) {
	f := newFixture(t)
	e, _ := core.NewAuditEntry("WriterInitialized", "ops", f.now, map[string]string{"region": "eu-west-1"})
	if err := f.mem.AppendAudit(f.ctx, core.DRChain, e); err != nil {
		t.Fatal(err)
	}
	if _, err := f.job.Run(f.ctx); err != nil {
		t.Fatal(err)
	}
	ps, _ := f.mem.ListAnchorProofs(f.ctx, core.DRChain, 0, 10)
	if len(ps) != 1 || ps[0].Leaf.Repo != core.DRChain {
		t.Fatalf("DR chain proofs: %+v", ps)
	}
	if err := core.ValidateRepoName(core.DRChain); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("a repository may be named %s: %v", core.DRChain, err)
	}
}

// When the signing key changes (a new multi-Region key, a move to another
// account), the chain verifies with every key it was signed with pinned, and
// not with the new key alone.
func TestKeyRotation(t *testing.T) {
	f := newFixture(t)
	old := f.trust.Keys[0]
	f.write("payments")
	f.run()
	next, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f.job.Signer = LocalSigner{Key: next, ID: "next"}
	f.write("payments")
	if rep := f.run(); rep.Anchored != 2 {
		t.Fatalf("anchor with the new key: %+v", rep)
	}
	f.trust.Keys = []*ecdsa.PublicKey{old, &next.PublicKey}
	got, err := f.verify("payments")
	if err != nil || len(got) != 2 || got[0].SignedBy == got[1].SignedBy || got[1].SignedBy != KeyFingerprint(&next.PublicKey) {
		t.Fatalf("both keys pinned: %+v %v", got, err)
	}
	f.trust.Keys = []*ecdsa.PublicKey{&next.PublicKey}
	if _, err := f.verify("payments"); err == nil {
		t.Fatal("verified anchor 1 without its key")
	}
	// Several keys in one PEM file, as auditors pin them.
	var pemFile []byte
	for _, k := range []*ecdsa.PublicKey{old, &next.PublicKey} {
		der, _ := x509.MarshalPKIXPublicKey(k)
		pemFile = append(pemFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...)
	}
	if keys, err := ParsePublicKeys(pemFile); err != nil || len(keys) != 2 {
		t.Fatalf("parse two keys: %d %v", len(keys), err)
	}
}
