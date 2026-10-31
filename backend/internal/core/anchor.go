package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Anchoring: a scheduled job takes the head (seq, hash) of every audit chain,
// builds a Merkle tree over them and signs a statement that commits to the
// root and to the previous anchor. The statement also gets an RFC 3161
// timestamp from an outside authority. Once an anchor exists, rewriting the
// tail of any chain it covers is detectable: the anchored (seq, hash) no
// longer matches, and forging a new anchor does not erase the old one.

// AnchorVersion is the statement format.
const AnchorVersion = 1

// AnchorLeaf is one chain's head at anchor time.
type AnchorLeaf struct {
	Repo string `json:"repo"`
	Seq  int64  `json:"seq"`
	Hash Hash   `json:"hash"`
}

// AnchorStatement is what is signed and timestamped. Prev is the digest of
// the previous anchor's statement, so anchors form their own chain.
type AnchorStatement struct {
	Version   int       `json:"v"`
	Seq       int64     `json:"seq"`
	Prev      string    `json:"prev"`
	Root      string    `json:"root"`
	Leaves    int       `json:"leaves"`
	CreatedAt time.Time `json:"createdAt"`
}

// Bytes is the canonical encoding (fixed field order, compact JSON).
func (s AnchorStatement) Bytes() []byte {
	s.CreatedAt = s.CreatedAt.UTC().Truncate(time.Millisecond)
	b, _ := json.Marshal(s)
	return b
}

// Digest is SHA-256 of Bytes: the message both signed and timestamped.
func (s AnchorStatement) Digest() []byte {
	d := sha256.Sum256(s.Bytes())
	return d[:]
}

// DigestHex is Digest in hex, the anchor's identity.
func (s AnchorStatement) DigestHex() string { return hex.EncodeToString(s.Digest()) }

// AnchorSeal is the proof material over a statement.
type AnchorSeal struct {
	KeyID     string `json:"keyId"`
	Signature []byte `json:"signature"` // ECDSA P-256, ASN.1 DER, over Digest
	// Timestamp is an RFC 3161 TimeStampToken (DER) whose message imprint is
	// SHA-256(statement bytes) = Digest. Empty if the authority failed.
	Timestamp      []byte `json:"timestamp,omitempty"`
	TimestampError string `json:"timestampError,omitempty"`
}

// Anchor is the full record, kept by the operator. It names every chain, so
// readers only ever get an AnchorProof for their own repository.
type Anchor struct {
	Statement AnchorStatement `json:"statement"`
	Seal      AnchorSeal      `json:"seal"`
	Heads     []AnchorLeaf    `json:"heads"` // sorted by repo, the Merkle leaves
}

// AnchorProof shows one chain's head is under an anchor, without revealing
// the other chains.
type AnchorProof struct {
	Statement AnchorStatement `json:"statement"`
	Seal      AnchorSeal      `json:"seal"`
	Leaf      AnchorLeaf      `json:"leaf"`
	Index     int             `json:"index"`
	Proof     []string        `json:"proof"` // sibling hashes, hex, leaf to root
}

// Verify checks the leaf is under the statement's root. Signature and
// timestamp are checked by the caller against keys it trusts.
func (p AnchorProof) Verify() error {
	if p.Statement.Version != AnchorVersion {
		return fmt.Errorf("anchor %d: unknown version %d", p.Statement.Seq, p.Statement.Version)
	}
	proof := make([][]byte, len(p.Proof))
	for i, h := range p.Proof {
		b, err := hex.DecodeString(h)
		if err != nil {
			return fmt.Errorf("anchor %d: bad proof", p.Statement.Seq)
		}
		proof[i] = b
	}
	root, err := RootFromProof(LeafHash(p.Leaf), p.Index, p.Statement.Leaves, proof)
	if err != nil {
		return fmt.Errorf("anchor %d: %w", p.Statement.Seq, err)
	}
	if hex.EncodeToString(root) != p.Statement.Root {
		return fmt.Errorf("anchor %d: %s#%d is not under the anchored root", p.Statement.Seq, p.Leaf.Repo, p.Leaf.Seq)
	}
	return nil
}

// Merkle tree as in RFC 9162 (Certificate Transparency v2): domain-separated
// leaf (0x00) and node (0x01) hashes, left subtree = largest power of two
// smaller than n.

// LeafHash hashes one chain head. JSON keeps repo names unambiguous.
func LeafHash(l AnchorLeaf) []byte {
	b, _ := json.Marshal(l)
	h := sha256.New()
	h.Write([]byte{0})
	h.Write(b)
	return h.Sum(nil)
}

func nodeHash(l, r []byte) []byte {
	h := sha256.New()
	h.Write([]byte{1})
	h.Write(l)
	h.Write(r)
	return h.Sum(nil)
}

func split(n int) int {
	k := 1
	for k<<1 < n {
		k <<= 1
	}
	return k
}

// MerkleRoot is the root over leaf hashes (SHA-256 of nothing if empty).
func MerkleRoot(leaves [][]byte) []byte {
	switch len(leaves) {
	case 0:
		h := sha256.Sum256(nil)
		return h[:]
	case 1:
		return leaves[0]
	}
	k := split(len(leaves))
	return nodeHash(MerkleRoot(leaves[:k]), MerkleRoot(leaves[k:]))
}

// MerkleProof is the audit path of leaf i.
func MerkleProof(leaves [][]byte, i int) [][]byte {
	if len(leaves) <= 1 {
		return nil
	}
	k := split(len(leaves))
	if i < k {
		return append(MerkleProof(leaves[:k], i), MerkleRoot(leaves[k:]))
	}
	return append(MerkleProof(leaves[k:], i-k), MerkleRoot(leaves[:k]))
}

// RootFromProof recomputes the root from a leaf and its audit path
// (RFC 9162 section 2.1.3.2).
func RootFromProof(leaf []byte, index, size int, proof [][]byte) ([]byte, error) {
	if index < 0 || index >= size {
		return nil, fmt.Errorf("leaf index %d outside tree of %d", index, size)
	}
	fn, sn := index, size-1
	r := leaf
	for _, p := range proof {
		if sn == 0 {
			return nil, fmt.Errorf("proof too long")
		}
		if fn&1 == 1 || fn == sn {
			r = nodeHash(p, r)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			r = nodeHash(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return nil, fmt.Errorf("proof too short")
	}
	return r, nil
}

// SameLeaves reports whether two head lists are identical.
func SameLeaves(a, b []AnchorLeaf) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// HexEqual compares a digest with its hex form.
func HexEqual(b []byte, h string) bool { return bytes.Equal([]byte(hex.EncodeToString(b)), []byte(h)) }
