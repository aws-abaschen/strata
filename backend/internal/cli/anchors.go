package cli

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

// anchorCheck is how a verifier checks a chain against signed anchors: the
// keys it pinned and the anchors it remembers from earlier runs.
type anchorCheck struct {
	trust anchor.Trust
	// witness is the file of anchors this machine already verified ("" = none).
	// The server keeps proofs in the same table as the ledger, so someone who
	// can rewrite one can delete the other; a remembered anchor cannot be.
	witness string
}

// anchorFlags registers --anchor-key, --tsa-ca and --witness.
func anchorFlags(fs *flag.FlagSet, env Env) func(defaultWitness string) (anchorCheck, error) {
	key := fs.String("anchor-key", env.Getenv("STRATA_ANCHOR_KEY"), "PEM public key of the anchor signer, obtained out of band")
	tsaCA := fs.String("tsa-ca", env.Getenv("STRATA_TSA_CA"), "PEM root certificate(s) of the timestamp authority")
	wit := fs.String("witness", env.Getenv("STRATA_WITNESS"), "file that remembers verified anchors (default .strata/anchors.json in a workspace)")
	return func(defaultWitness string) (anchorCheck, error) {
		t, err := loadTrust(*key, *tsaCA)
		return anchorCheck{trust: t, witness: firstNonEmpty(*wit, defaultWitness)}, err
	}
}

// witnessEntry is one anchor verified on this machine.
type witnessEntry struct {
	Anchor     int64           `json:"anchor"`
	Digest     string          `json:"digest"`
	Leaf       core.AnchorLeaf `json:"leaf"`
	VerifiedAt time.Time       `json:"verifiedAt"`
}

type witnessFile struct {
	Chains map[string][]witnessEntry `json:"chains"` // key: API URL + chain
}

func readWitness(path string) (witnessFile, error) {
	w := witnessFile{Chains: map[string][]witnessEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return w, err
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return w, fmt.Errorf("witness %s: %w", path, err)
	}
	if w.Chains == nil {
		w.Chains = map[string][]witnessEntry{}
	}
	return w, nil
}

// verify checks recs against the served proofs and the remembered anchors,
// prints what it established, and remembers the anchors it verified.
func (ac anchorCheck) verify(label, key string, recs []core.AuditRecord, ps []core.AnchorProof, out io.Writer) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w: ledger of %s FAILED anchor verification: %s", core.ErrPolicy, label, fmt.Sprintf(format, a...))
	}
	var w witnessFile
	if ac.witness != "" {
		var err error
		if w, err = readWitness(ac.witness); err != nil {
			return err
		}
		served := map[int64]core.AnchorProof{}
		for _, p := range ps {
			served[p.Statement.Seq] = p
		}
		for _, e := range w.Chains[key] {
			p, ok := served[e.Anchor]
			switch {
			case !ok:
				return fail("anchor %d (verified here %s) is no longer served: evidence was removed", e.Anchor, e.VerifiedAt.Format(time.RFC3339))
			case p.Statement.DigestHex() != e.Digest || p.Leaf != e.Leaf:
				return fail("anchor %d changed since it was verified here %s", e.Anchor, e.VerifiedAt.Format(time.RFC3339))
			}
		}
	}
	if len(ps) == 0 {
		fmt.Fprintf(out, "not anchored yet: the anchor job runs every 15 minutes\n")
		return nil
	}
	checked, err := anchor.VerifyChain(recs, ps, ac.trust)
	if err != nil {
		return fail("%v", err)
	}
	printAnchors(out, "", checked, ac)
	if ac.witness == "" {
		fmt.Fprintf(out, "  witness: anchors not remembered (run in a workspace or pass --witness)\n")
		return nil
	}
	known := map[int64]bool{}
	for _, e := range w.Chains[key] {
		known[e.Anchor] = true
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, p := range ps {
		if !known[p.Statement.Seq] {
			w.Chains[key] = append(w.Chains[key], witnessEntry{Anchor: p.Statement.Seq, Digest: p.Statement.DigestHex(), Leaf: p.Leaf, VerifiedAt: now})
		}
	}
	b, _ := json.MarshalIndent(w, "", "  ")
	if err := os.MkdirAll(filepath.Dir(ac.witness), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(ac.witness, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "  witness: %d anchors remembered in %s\n", len(w.Chains[key]), ac.witness)
	return nil
}

// printAnchors summarizes what checking a chain's anchors established.
func printAnchors(out io.Writer, label string, checked []anchor.Checked, ac anchorCheck) {
	if label != "" {
		label += " "
	}
	if len(checked) == 0 {
		fmt.Fprintf(out, "%snot anchored yet: the anchor job runs every 15 minutes\n", label)
		return
	}
	var lastTS time.Time
	stamped := 0
	for _, ch := range checked {
		if ch.TimestampOK {
			stamped++
			lastTS = ch.TimestampAt
		}
	}
	last := checked[len(checked)-1]
	fmt.Fprintf(out, "%sanchored %d times, through seq %d (anchor %d)\n", label, len(checked), last.Leaf.Seq, last.Seq)
	if len(ac.trust.Keys) > 0 {
		signers := map[string]bool{}
		for _, ch := range checked {
			signers[ch.SignedBy] = true
		}
		if len(signers) > 1 {
			fmt.Fprintf(out, "  signatures: verified with the pinned anchor keys (%d keys signed this chain)\n", len(signers))
		} else {
			fmt.Fprintf(out, "  signatures: verified with the pinned anchor key\n")
		}
	} else {
		fmt.Fprintf(out, "  signatures: NOT checked, no anchor key pinned (--anchor-key or STRATA_ANCHOR_KEY)\n")
	}
	switch {
	case stamped == 0:
		fmt.Fprintf(out, "  timestamps: none (%s)\n", last.TimestampErr)
	case ac.trust.TSARoot != nil:
		fmt.Fprintf(out, "  timestamps: %d of %d, RFC 3161, authority verified against the pinned root, latest %s\n", stamped, len(checked), lastTS.UTC().Format(time.RFC3339))
	default:
		fmt.Fprintf(out, "  timestamps: %d of %d, RFC 3161, latest %s; authority NOT checked (--tsa-ca or STRATA_TSA_CA)\n", stamped, len(checked), lastTS.UTC().Format(time.RFC3339))
	}
}

// abs resolves p against the command's directory.
func abs(dir, p string) string {
	if filepath.IsAbs(p) || dir == "" {
		return p
	}
	return filepath.Join(dir, p)
}

// loadTrust reads the pinned anchor key and timestamp roots (either may be empty).
func loadTrust(keyFile, caFile string) (anchor.Trust, error) {
	var t anchor.Trust
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return t, err
		}
		if t.Keys, err = anchor.ParsePublicKeys(b); err != nil {
			return t, fmt.Errorf("anchor key %s: %w", keyFile, err)
		}
	}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return t, err
		}
		t.TSARoot = x509.NewCertPool()
		if !t.TSARoot.AppendCertsFromPEM(b) {
			return t, fmt.Errorf("no PEM certificates in %s", caFile)
		}
	}
	return t, nil
}

// workspaceWitness is the default witness file inside a working copy.
func workspaceWitness(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, StateDir, "anchors.json")
}
