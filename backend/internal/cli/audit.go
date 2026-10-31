package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// auditCmd prints a repository's ledger and verifies the whole hash chain
// on this machine, so the check does not depend on trusting the server.
func auditCmd(ctx context.Context, c *Client, repo string, n int, verifyOnly bool, ac anchorCheck, out io.Writer) error {
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
	ps, err := c.Anchors(ctx, repo)
	if err != nil {
		return err
	}
	return ac.verify(repo, c.API+" "+repo, recs, ps, out)
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
