package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Grant is a break-glass grant as the API returns it.
type Grant struct {
	core.Grant
	Active   bool               `json:"active"`
	Overdue  bool               `json:"overdue"`
	Activity []core.AuditRecord `json:"activity,omitempty"`
}

// BreakGlassRequest asks for a grant.
type BreakGlassRequest struct {
	Reason   string   `json:"reason"`
	WorkItem string   `json:"workItem"`
	Repos    []string `json:"repos"`
	Minutes  int      `json:"minutes,omitempty"`
}

// StartBreakGlass activates a grant for the caller.
func (c *Client) StartBreakGlass(ctx context.Context, in BreakGlassRequest) (Grant, error) {
	var out Grant
	err := c.do(ctx, http.MethodPost, "/break-glass", in, &out)
	return out, err
}

// Grants lists grants, newest first; open lists only unreviewed ones.
func (c *Client) Grants(ctx context.Context, open bool) ([]Grant, error) {
	var out []Grant
	p := "/break-glass?limit=200"
	if open {
		p += "&open=1"
	}
	err := c.do(ctx, http.MethodGet, p, nil, &out)
	return out, err
}

// Grant returns one grant with what was done under it.
func (c *Client) Grant(ctx context.Context, id string) (Grant, error) {
	var out Grant
	err := c.do(ctx, http.MethodGet, "/break-glass/"+url.PathEscape(id), nil, &out)
	return out, err
}

// EndBreakGlass ends a grant early.
func (c *Client) EndBreakGlass(ctx context.Context, id string) (Grant, error) {
	var out Grant
	err := c.do(ctx, http.MethodPost, "/break-glass/"+url.PathEscape(id)+"/end", map[string]any{}, &out)
	return out, err
}

// ReviewBreakGlass records the after-the-fact review of a grant.
func (c *Client) ReviewBreakGlass(ctx context.Context, id, outcome, note string) (Grant, error) {
	var out Grant
	err := c.do(ctx, http.MethodPost, "/break-glass/"+url.PathEscape(id)+"/review", map[string]any{"outcome": outcome, "note": note}, &out)
	return out, err
}

// BreakGlassLedger returns the break-glass audit chain.
func (c *Client) BreakGlassLedger(ctx context.Context) ([]core.AuditRecord, error) {
	var out []core.AuditRecord
	err := c.do(ctx, http.MethodGet, "/break-glass/ledger", nil, &out)
	return out, err
}

const breakglassUsage = `usage:
  strata breakglass start --reason TEXT --work-item INC-123 --repo NAME [--repo NAME|*] [--for 1h]
  strata breakglass end [ID]                 end your grant (or any, as an admin) now
  strata breakglass list [--open]            grants, newest first; --open: unreviewed
  strata breakglass show ID                  a grant and everything done under it
  strata breakglass review ID --outcome accepted|rejected --note TEXT
  strata breakglass audit [--verify]         the break-glass ledger, verified here`

func breakglassCmd(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 {
		return errors.New(breakglassUsage)
	}
	fs := flag.NewFlagSet("breakglass", flag.ContinueOnError)
	fs.SetOutput(env.Out)
	api := fs.String("api", "", "API URL")
	reason := fs.String("reason", "", "what is broken and why break-glass is needed")
	workItem := fs.String("work-item", "", "incident or emergency-change ticket")
	var repos multi
	fs.Var(&repos, "repo", `repository the grant covers (repeatable; "*" for all)`)
	dur := fs.Duration("for", time.Hour, "how long the grant lasts (max 4h)")
	open := fs.Bool("open", false, "only unreviewed grants")
	outcome := fs.String("outcome", "", "accepted or rejected")
	note := fs.String("note", "", "what you checked and found")
	verify := fs.Bool("verify", false, "only verify the chain")
	anchors := anchorFlags(fs, env)
	if err := fs.Parse(reorder(args[1:])); err != nil {
		return err
	}
	st, root, _ := LoadState(env.Dir)
	c, err := env.client(firstNonEmpty(*api, st.API))
	if err != nil {
		return err
	}
	id := fs.Arg(0)
	switch args[0] {
	case "start":
		g, err := c.StartBreakGlass(ctx, BreakGlassRequest{Reason: *reason, WorkItem: *workItem, Repos: repos, Minutes: int(dur.Minutes())})
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "break-glass %s active until %s on %s\n", g.ID, g.ExpiresAt.Local().Format(time.Kitchen), strings.Join(g.Repos, ", "))
		fmt.Fprintln(env.Out, "everything you do is tagged with this grant; another admin reviews it afterwards")
		return nil
	case "end":
		if id == "" {
			me, err := c.Me(ctx)
			if err != nil {
				return err
			}
			if me.BreakGlass == nil {
				return errors.New("you hold no active grant")
			}
			id = me.BreakGlass.ID
		}
		if _, err := c.EndBreakGlass(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "ended %s; it now awaits review by another admin\n", id)
		return nil
	case "list":
		gs, err := c.Grants(ctx, *open)
		if err != nil {
			return err
		}
		for _, g := range gs {
			fmt.Fprintf(env.Out, "%s  %-24s %-10s %-12s %s\n", g.ID, g.User, grantState(g), g.WorkItem, g.Reason)
		}
		return nil
	case "show":
		if id == "" {
			return errors.New("usage: strata breakglass show ID")
		}
		g, err := c.Grant(ctx, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "grant    %s (%s)\nholder   %s\nticket   %s\nreason   %s\nrepos    %s\nfrom     %s\nuntil    %s\n",
			g.ID, grantState(g), g.User, g.WorkItem, g.Reason, strings.Join(g.Repos, ", "),
			g.CreatedAt.Format(time.RFC3339), g.Until().Format(time.RFC3339))
		if r := g.Review; r != nil {
			fmt.Fprintf(env.Out, "review   %s by %s at %s: %s\n", r.Outcome, r.Reviewer, r.At.Format(time.RFC3339), r.Note)
		}
		fmt.Fprintf(env.Out, "\n%d actions under this grant:\n", len(g.Activity))
		for _, r := range g.Activity {
			fmt.Fprintf(env.Out, "  %s  %-16s %-20s %s\n", r.At.Format(time.RFC3339), r.Repo, r.Action, summary(r))
		}
		return nil
	case "review":
		if id == "" {
			return errors.New("usage: strata breakglass review ID --outcome accepted|rejected --note TEXT")
		}
		g, err := c.ReviewBreakGlass(ctx, id, *outcome, *note)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "reviewed %s: %s\n", g.ID, g.Review.Outcome)
		return nil
	case "audit":
		recs, err := c.BreakGlassLedger(ctx)
		if err != nil {
			return err
		}
		last, verr := core.VerifyAudit(recs, "")
		if !*verify {
			for _, r := range recs {
				fmt.Fprintf(env.Out, "%6d  %s  %-22s %s\n", r.Seq, r.At.Format(time.RFC3339), r.Actor, r.Action)
			}
		}
		if verr != nil {
			return fmt.Errorf("%w: break-glass ledger FAILED verification: %v", core.ErrPolicy, verr)
		}
		fmt.Fprintf(env.Out, "verified %d break-glass records, head %s\n", len(recs), last.Short())
		ac, err := anchors(workspaceWitness(root))
		if err != nil {
			return err
		}
		ps, err := c.BreakGlassAnchors(ctx)
		if err != nil {
			return err
		}
		return ac.verify("break-glass", c.API+" "+core.BreakGlassChain, recs, ps, env.Out)
	}
	return errors.New(breakglassUsage)
}

func grantState(g Grant) string {
	switch {
	case g.Active:
		return "ACTIVE"
	case g.Review != nil:
		return g.Review.Outcome
	case g.Overdue:
		return "OVERDUE"
	}
	return "to-review"
}
