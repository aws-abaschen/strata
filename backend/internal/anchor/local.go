package anchor

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
)

// LocalSigner signs with an in-process key (tests and local development; in
// AWS the key lives in KMS and never leaves it).
type LocalSigner struct {
	Key *ecdsa.PrivateKey
	ID  string
}

// KeyID implements Signer.
func (s LocalSigner) KeyID() string { return s.ID }

// Sign implements Signer.
func (s LocalSigner) Sign(_ context.Context, digest []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, s.Key, digest)
}
