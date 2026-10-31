package core

import (
	"encoding/hex"
	"fmt"
	"testing"
)

func TestMerkleProofsVerifyForEveryTreeSize(t *testing.T) {
	for n := 1; n <= 17; n++ {
		var leaves []AnchorLeaf
		var hashes [][]byte
		for i := 0; i < n; i++ {
			l := AnchorLeaf{Repo: fmt.Sprintf("r%02d", i), Seq: int64(i + 1), Hash: Hash(fmt.Sprintf("%064x", i))}
			leaves = append(leaves, l)
			hashes = append(hashes, LeafHash(l))
		}
		root := hex.EncodeToString(MerkleRoot(hashes))
		for i := range leaves {
			var proof []string
			for _, p := range MerkleProof(hashes, i) {
				proof = append(proof, hex.EncodeToString(p))
			}
			p := AnchorProof{Statement: AnchorStatement{Version: AnchorVersion, Root: root, Leaves: n}, Leaf: leaves[i], Index: i, Proof: proof}
			if err := p.Verify(); err != nil {
				t.Fatalf("n=%d i=%d: %v", n, i, err)
			}
			// A rewritten head no longer verifies.
			p.Leaf.Hash = Hash(fmt.Sprintf("%064x", 999))
			if p.Verify() == nil {
				t.Fatalf("n=%d i=%d: forged leaf verified", n, i)
			}
			// Nor does a proof replayed at another index.
			if n > 1 {
				p.Leaf, p.Index = leaves[i], (i+1)%n
				if p.Verify() == nil {
					t.Fatalf("n=%d i=%d: proof verified at the wrong index", n, i)
				}
			}
		}
	}
}

func TestStatementDigestCoversPreviousAnchor(t *testing.T) {
	a := AnchorStatement{Version: 1, Seq: 2, Prev: "aa", Root: "bb", Leaves: 3}
	b := a
	b.Prev = "ab"
	if a.DigestHex() == b.DigestHex() {
		t.Fatal("digest must change with the previous anchor")
	}
}
