package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
	"github.com/aws-abaschen/strata/backend/internal/export"
)

const exportUsage = `usage:
  strata export --repo NAME --out DIR [--break-glass]   Git repository plus evidence sidecar
  strata export verify DIR [--anchor-key PEM] [--tsa-ca PEM]   check an export offline`

// clientSource reads an export over the API.
type clientSource struct {
	c    *Client
	repo string
}

func (s clientSource) Repo(ctx context.Context) (core.Repo, error)  { return s.c.GetRepo(ctx, s.repo) }
func (s clientSource) Refs(ctx context.Context) ([]core.Ref, error) { return s.c.Refs(ctx, s.repo) }
func (s clientSource) Change(ctx context.Context, rev core.Hash) (core.Change, []core.Annotation, error) {
	d, err := s.c.GetChange(ctx, s.repo, rev)
	return d.Change, d.Annotations, err
}
func (s clientSource) Approvals(ctx context.Context, rev core.Hash) ([]core.Approval, error) {
	return s.c.Approvals(ctx, s.repo, rev)
}
func (s clientSource) Object(ctx context.Context, h core.Hash) ([]byte, error) {
	return download(ctx, s.c, s.repo, h)
}
func (s clientSource) Audit(ctx context.Context) ([]core.AuditRecord, error) {
	return s.c.Audit(ctx, s.repo)
}
func (s clientSource) Anchors(ctx context.Context) ([]core.AnchorProof, error) {
	return s.c.Anchors(ctx, s.repo)
}
func (s clientSource) BreakGlass(ctx context.Context) ([]core.AuditRecord, []core.AnchorProof, error) {
	recs, err := s.c.BreakGlassLedger(ctx)
	if err != nil {
		return nil, nil, err
	}
	ps, err := s.c.BreakGlassAnchors(ctx)
	return recs, ps, err
}

// Refs lists a repository's refs.
func (c *Client) Refs(ctx context.Context, repo string) ([]core.Ref, error) {
	var out struct{ Refs []core.Ref }
	err := c.do(ctx, http.MethodGet, "/repos/"+esc(repo)+"/refs", nil, &out)
	return out.Refs, err
}

func exportCmd(ctx context.Context, args []string, env Env) error {
	if len(args) > 0 && args[0] == "verify" {
		return exportVerify(args[1:], env)
	}
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(env.Out)
	api := fs.String("api", "", "API URL")
	repo := fs.String("repo", "", "repository to export (default: this working copy's)")
	out := fs.String("out", "", "directory to write (must be empty)")
	bg := fs.Bool("break-glass", false, "also export the break-glass ledger (strata-admins and auditors)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, _, _ := LoadState(env.Dir)
	name := firstNonEmpty(*repo, st.Repo)
	if name == "" || *out == "" {
		return errors.New(exportUsage)
	}
	c, err := env.client(firstNonEmpty(*api, st.API))
	if err != nil {
		return err
	}
	me, err := c.Me(ctx)
	if err != nil {
		return err
	}
	m, err := export.Write(ctx, clientSource{c, name}, abs(env.Dir, *out), export.Options{BreakGlass: *bg, API: c.API, By: me.Identity, Now: time.Now()})
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Out, "exported %s to %s: %d changes, %d trees, %d files, %d refs, %d ledger records, %d anchors\n",
		m.Repo, *out, m.Changes, m.Trees, m.Blobs, m.Refs, m.Records, m.Anchors)
	if len(m.Omitted) > 0 || len(m.OmittedRefs) > 0 {
		fmt.Fprintf(env.Out, "  %d entries and %d refs Git cannot hold are kept in the sidecar only\n", len(m.Omitted), len(m.OmittedRefs))
	}
	fmt.Fprintf(env.Out, "  git clone %s/git    then verify with: strata export verify %s --anchor-key anchor.pem\n", *out, *out)
	return nil
}

func exportVerify(args []string, env Env) error {
	fs := flag.NewFlagSet("export verify", flag.ContinueOnError)
	fs.SetOutput(env.Out)
	anchors := anchorFlags(fs, env)
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(exportUsage)
	}
	ac, err := anchors("")
	if err != nil {
		return err
	}
	dir := abs(env.Dir, fs.Arg(0))
	rep, err := export.Verify(dir, ac.trust)
	if err != nil {
		return fmt.Errorf("%w: %v", core.ErrPolicy, err)
	}
	fmt.Fprintf(env.Out, "verified export of %s: %d ledger records, %d changes, %d trees, %d files, %d refs, %d annotations, %d approvals\n",
		rep.Repo, rep.Records, rep.Changes, rep.Trees, rep.Blobs, rep.Refs, rep.Annotations, rep.Approvals)
	if rep.Omitted > 0 {
		fmt.Fprintf(env.Out, "  %d tree entries Git cannot hold (.git) are checked in the sidecar only\n", rep.Omitted)
	}
	printAnchors(env.Out, "ledger", rep.Anchors, ac)
	printUnanchored(env.Out, rep.Unanchored)
	if rep.BreakGlass != nil {
		fmt.Fprintf(env.Out, "verified %d break-glass records\n", rep.BreakGlass.Records)
		printAnchors(env.Out, "break-glass ledger", rep.BreakGlass.Anchors, ac)
		printUnanchored(env.Out, rep.BreakGlass.Unanchored)
	}
	return nil
}

func printUnanchored(out io.Writer, n int) {
	if n > 0 {
		fmt.Fprintf(out, "  %d records after the last anchor rest on the hash chain alone\n", n)
	}
}
