package cli

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

// auditCmd prints a repository's ledger and verifies the whole hash chain
// on this machine, so the check does not depend on trusting the server.
func auditCmd(ctx context.Context, c *Client, repo string, n int, verifyOnly bool, trust anchor.Trust, out io.Writer) error {
	recs, err := c.Audit(ctx, repo)
	if err != nil {
		return err
	}
	last, verr := core.VerifyAudit(recs, "")
	if !verifyOnly {
		from := max(0, len(recs)-n)
		for _, r := range recs[from:] {
			fmt.Fprintf(out, "%6d  %s  %-22s %-20s %s\n", r.Seq, r.At.Format(time.RFC3339), r.Actor, r.Action, summary(r))
		}
	}
	if verr != nil {
		return fmt.Errorf("%w: ledger of %s FAILED verification: %v", core.ErrPolicy, repo, verr)
	}
	fmt.Fprintf(out, "verified %d records of %s, head %s\n", len(recs), repo, last.Short())
	return verifyAnchors(ctx, c, repo, recs, trust, out)
}

// verifyAnchors checks the chain against every signed anchor that covered
// it: an anchored head that changed means history was rewritten after the
// fact, even if the rewritten chain is internally consistent.
func verifyAnchors(ctx context.Context, c *Client, repo string, recs []core.AuditRecord, trust anchor.Trust, out io.Writer) error {
	ps, err := c.Anchors(ctx, repo)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		fmt.Fprintf(out, "not anchored yet: the anchor job runs every 15 minutes\n")
		return nil
	}
	checked, err := anchor.VerifyChain(recs, ps, trust)
	if err != nil {
		return fmt.Errorf("%w: ledger of %s FAILED anchor verification: %v", core.ErrPolicy, repo, err)
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
	fmt.Fprintf(out, "anchored %d times, through seq %d (anchor %d)\n", len(checked), last.Leaf.Seq, last.Seq)
	if trust.Key != nil {
		fmt.Fprintf(out, "  signatures: verified with the pinned anchor key\n")
	} else {
		fmt.Fprintf(out, "  signatures: NOT checked, no anchor key pinned (--anchor-key or STRATA_ANCHOR_KEY)\n")
	}
	switch {
	case stamped == 0:
		fmt.Fprintf(out, "  timestamps: none (%s)\n", last.TimestampErr)
	case trust.TSARoot != nil:
		fmt.Fprintf(out, "  timestamps: %d of %d, RFC 3161, authority verified against the pinned root, latest %s\n", stamped, len(checked), lastTS.UTC().Format(time.RFC3339))
	default:
		fmt.Fprintf(out, "  timestamps: %d of %d, RFC 3161, latest %s; authority NOT checked (--tsa-ca or STRATA_TSA_CA)\n", stamped, len(checked), lastTS.UTC().Format(time.RFC3339))
	}
	return nil
}

// loadTrust reads the pinned anchor key and timestamp roots (either may be empty).
func loadTrust(keyFile, caFile string) (anchor.Trust, error) {
	var t anchor.Trust
	if keyFile != "" {
		b, err := os.ReadFile(keyFile)
		if err != nil {
			return t, err
		}
		if t.Key, err = anchor.ParsePublicKey(b); err != nil {
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

// summary renders the most useful field of a record's detail.
func summary(r core.AuditRecord) string {
	var d struct {
		Subject string `json:"subject"`
		Ref     any    `json:"ref"`
		Note    string `json:"note"`
	}
	json.Unmarshal(r.Detail, &d)
	switch {
	case d.Note != "":
		return d.Note
	case d.Subject != "":
		return d.Subject
	}
	if m, ok := d.Ref.(map[string]any); ok {
		return fmt.Sprintf("%v -> %v", m["name"], m["target"])
	}
	return ""
}
