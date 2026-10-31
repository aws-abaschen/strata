package anchor

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/digitorus/timestamp"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// HTTPTSA asks an RFC 3161 timestamp authority over HTTP.
type HTTPTSA struct {
	URL    string
	Client *http.Client
}

// Stamp implements TSA.
func (t HTTPTSA) Stamp(ctx context.Context, digest []byte) ([]byte, error) {
	nonce, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, err
	}
	body, err := (&timestamp.Request{HashAlgorithm: crypto.SHA256, HashedMessage: digest, Certificates: true, Nonce: nonce}).Marshal()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	c := t.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("timestamp authority: %w", err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("timestamp authority: HTTP %d", res.StatusCode)
	}
	ts, err := timestamp.ParseResponse(b)
	if err != nil {
		return nil, fmt.Errorf("timestamp authority: %w", err)
	}
	if !bytes.Equal(ts.HashedMessage, digest) || ts.Nonce == nil || ts.Nonce.Cmp(nonce) != 0 {
		return nil, fmt.Errorf("timestamp authority answered for another request")
	}
	return ts.RawToken, nil
}

// Trust is what a verifier pins out of band. Nil fields are not checked,
// and the result says so.
type Trust struct {
	Key     *ecdsa.PublicKey // the anchor signing key
	TSARoot *x509.CertPool   // roots of the timestamp authority
}

// ParsePublicKey reads a PEM or DER SubjectPublicKeyInfo (as KMS GetPublicKey returns).
func ParsePublicKey(b []byte) (*ecdsa.PublicKey, error) {
	if p, _ := pem.Decode(b); p != nil {
		b = p.Bytes
	}
	k, err := x509.ParsePKIXPublicKey(b)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("anchor key is %T, want ECDSA", k)
	}
	return ec, nil
}

// Checked is what Verify could establish about one proof.
type Checked struct {
	Seq          int64
	Leaf         core.AnchorLeaf
	SignatureOK  bool      // signature valid under the pinned key
	TimestampAt  time.Time // zero if no timestamp
	TimestampOK  bool      // token valid and over this statement
	TimestampCA  bool      // TSA certificate chains to a pinned root
	TimestampErr string    // why the job recorded no timestamp
}

// Verify checks one proof: the Merkle path, the signature (if a key is
// pinned) and the timestamp token (always parsed; chained to roots if
// pinned). It does not check the leaf against the chain: callers do.
func Verify(p core.AnchorProof, trust Trust) (Checked, error) {
	c := Checked{Seq: p.Statement.Seq, Leaf: p.Leaf, TimestampErr: p.Seal.TimestampError}
	if err := p.Verify(); err != nil {
		return c, err
	}
	digest := p.Statement.Digest()
	if trust.Key != nil {
		if !ecdsa.VerifyASN1(trust.Key, digest, p.Seal.Signature) {
			return c, fmt.Errorf("anchor %d: signature does not match the pinned key", p.Statement.Seq)
		}
		c.SignatureOK = true
	}
	if len(p.Seal.Timestamp) > 0 {
		ts, err := timestamp.Parse(p.Seal.Timestamp)
		if err != nil {
			return c, fmt.Errorf("anchor %d: timestamp: %w", p.Statement.Seq, err)
		}
		if !bytes.Equal(ts.HashedMessage, digest) {
			return c, fmt.Errorf("anchor %d: timestamp is for another statement", p.Statement.Seq)
		}
		c.TimestampAt, c.TimestampOK = ts.Time, len(ts.Certificates) > 0
		if trust.TSARoot != nil && len(ts.Certificates) > 0 {
			inter := x509.NewCertPool()
			for _, cert := range ts.Certificates[1:] {
				inter.AddCert(cert)
			}
			if _, err := ts.Certificates[0].Verify(x509.VerifyOptions{
				Roots: trust.TSARoot, Intermediates: inter, CurrentTime: ts.Time,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
			}); err != nil {
				return c, fmt.Errorf("anchor %d: timestamp authority not trusted: %w", p.Statement.Seq, err)
			}
			c.TimestampCA = true
		}
	}
	return c, nil
}

// VerifyChain checks every proof for a chain against its records (all of
// them, from seq 1). Each anchored head must be the record the reader holds.
func VerifyChain(recs []core.AuditRecord, ps []core.AnchorProof, trust Trust) ([]Checked, error) {
	bySeq := map[int64]core.Hash{}
	for _, r := range recs {
		bySeq[r.Seq] = r.Hash
	}
	var out []Checked
	for _, p := range ps {
		c, err := Verify(p, trust)
		if err != nil {
			return out, err
		}
		h, ok := bySeq[p.Leaf.Seq]
		switch {
		case !ok:
			return out, fmt.Errorf("anchor %d covers %s#%d, which the ledger no longer has", p.Statement.Seq, p.Leaf.Repo, p.Leaf.Seq)
		case h != p.Leaf.Hash:
			return out, fmt.Errorf("anchor %d: %s#%d was rewritten after it was anchored", p.Statement.Seq, p.Leaf.Repo, p.Leaf.Seq)
		}
		out = append(out, c)
	}
	return out, nil
}
