package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

const usage = `strata: serverless version control on S3

Usage:
  strata repo create NAME [-d DESCRIPTION]
  strata repo list
  strata init --repo NAME [--ref main]          start a working copy here
  strata clone REPO [DIR] [--ref main]          download a repository
  strata status                                 show whether files changed since HEAD
  strata push -m MESSAGE [metadata] [--amend]   snapshot and record a change
  strata pull                                   merge the server's latest into your edits
  strata log [-n 20]                            history of the current ref
  strata show [REV]                             one change with its annotations
  strata annotate [REV] [--note TEXT] [metadata]
  strata push --review NAME -m MESSAGE          push to review/NAME, for approval
  strata approve [REV] [--note TEXT]            sign off on a colleague's change
  strata promote [REV] [--ref main]             move a ref to an approved revision
  strata find --work-item KEY | --label LABEL   search every repository
  strata audit [-n 20] [--verify]               the ledger, hash chain verified here

Break-glass (strata-admins; time-boxed, reviewed afterwards by another admin):
  strata breakglass start --reason TEXT --work-item INC-1 --repo NAME [--for 1h]
  strata breakglass end | list [--open] | show ID | audit [--verify]
  strata breakglass review ID --outcome accepted|rejected --note TEXT

Layers (configuration inherited top-down):
  strata repo create NAME --layer               a Layer repository
  strata repo create NAME --extends LAYER[@REF] a repository inheriting a Layer
  strata layers [show]                          the resolved chain and files
  strata layers set LAYER[@REF] | unset         change what this repo extends
  strata layers pull                            write inherited files here
  strata layers fleet [LAYER]                   repositories inheriting a Layer

Metadata flags (repeatable):
  --work-item KEY        PAY-42 (Jira) or system:KEY, e.g. servicenow:CHG0031337
  --label NAME           e.g. risk:low, release/2026.11
  --attr NAME=VALUE      any attribute, e.g. approver=ana

Jira keys in the message (PAY-42 ...) are linked automatically.

Environment:
  STRATA_API     API URL (else taken from the working copy, or --api)
  STRATA_TOKEN   bearer token (an ID token from the web console)
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

type metaFlags struct{ items, labels, attrs multi }

func (f *metaFlags) register(fs *flag.FlagSet) {
	fs.Var(&f.items, "work-item", "link a work item (repeatable)")
	fs.Var(&f.labels, "label", "add a label (repeatable)")
	fs.Var(&f.attrs, "attr", "add NAME=VALUE (repeatable)")
}

func (f *metaFlags) metadata() (core.Metadata, error) {
	var md core.Metadata
	for _, s := range f.items {
		w, err := core.ParseWorkItem(s)
		if err != nil {
			return md, err
		}
		md.WorkItems = append(md.WorkItems, w)
	}
	md.Labels = f.labels
	for _, a := range f.attrs {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return md, fmt.Errorf("--attr %q: want NAME=VALUE", a)
		}
		if md.Attributes == nil {
			md.Attributes = map[string]string{}
		}
		md.Attributes[k] = v
	}
	return md, md.Normalize()
}

// Env is what commands read from the outside world.
type Env struct {
	Dir    string
	Out    io.Writer
	Getenv func(string) string
	// Client overrides the HTTP client (tests).
	Client *Client
}

func (e Env) client(api string) (*Client, error) {
	if v := e.Getenv("STRATA_API"); v != "" {
		api = v
	}
	if api == "" {
		return nil, errors.New("no API URL: set STRATA_API or pass --api")
	}
	c := &Client{API: api, Token: e.Getenv("STRATA_TOKEN")}
	if e.Client != nil {
		c.HTTP = e.Client.HTTP
	}
	return c, nil
}

// Run executes one command and returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if err := run(ctx, args, env); err != nil {
		fmt.Fprintln(env.Out, "error:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(env.Out, usage)
		return nil
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(env.Out)
	api := fs.String("api", "", "API URL")
	switch cmd {
	case "repo":
		return repoCmd(ctx, args, env)
	case "breakglass":
		return breakglassCmd(ctx, args, env)
	case "init":
		repo := fs.String("repo", "", "repository name")
		ref := fs.String("ref", "main", "ref to push to")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if err := core.ValidateRepoName(*repo); err != nil {
			return err
		}
		if err := SaveState(env.Dir, State{API: *api, Repo: *repo, Ref: *ref}); err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "initialized %s (ref %s)\n", *repo, *ref)
		return nil
	case "clone":
		ref := fs.String("ref", "main", "ref to check out")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New("usage: strata clone REPO [DIR]")
		}
		repo := fs.Arg(0)
		dir := filepath.Join(env.Dir, repo)
		if fs.NArg() > 1 {
			dir = fs.Arg(1)
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(env.Dir, dir)
			}
		}
		c, err := env.client(*api)
		if err != nil {
			return err
		}
		return clone(ctx, c, repo, *ref, dir, env.Out)
	}

	st, root, err := LoadState(env.Dir)
	if err != nil && cmd != "find" { // search works from anywhere
		return err
	}
	c, err := env.client(firstNonEmpty(*api, st.API))
	if err != nil {
		return err
	}
	switch cmd {
	case "pull":
		if err := fs.Parse(args); err != nil {
			return err
		}
		res, err := Pull(ctx, c, &st, root)
		if err != nil {
			return err
		}
		if res.To == res.From {
			fmt.Fprintf(env.Out, "already up to date with %s\n", st.Ref)
		} else {
			fmt.Fprintf(env.Out, "pulled %s: %s -> %s, %d updated, %d merged, %d conflicts\n",
				st.Ref, res.From.Short(), res.To.Short(), res.Updated, res.Merged, len(res.Conflicts))
		}
		for _, p := range res.Kept {
			fmt.Fprintf(env.Out, "  kept %s: you changed it, the server deleted it\n", p)
		}
		for _, p := range res.Conflicts {
			fmt.Fprintf(env.Out, "  CONFLICT %s: resolve it, then push\n", p)
		}
		if ls, err := LoadLayerState(root); err == nil && len(ls.Files) > 0 {
			return PullLayers(ctx, c, st, root, env.Out)
		}
		return nil
	case "status":
		if left, err := unresolved(root); err == nil && len(left) > 0 {
			fmt.Fprintf(env.Out, "unresolved conflicts: %v\n", left)
		}
		snap, err := TakeSnapshot(root)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "repo %s, ref %s\n", st.Repo, st.Ref)
		if st.Head == "" {
			fmt.Fprintf(env.Out, "no changes yet; %d files ready to push\n", snap.Files)
			return nil
		}
		d, err := c.GetChange(ctx, st.Repo, st.Head)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "HEAD %s %s\n", st.Head.Short(), d.Change.Subject())
		if d.Change.Tree == snap.Root {
			fmt.Fprintln(env.Out, "working copy is clean")
		} else {
			fmt.Fprintln(env.Out, "working copy has changes")
		}
		return nil
	case "push":
		msg := fs.String("m", "", "message")
		amend := fs.Bool("amend", false, "replace HEAD with a new revision of the same change")
		review := fs.String("review", "", "push to review/NAME instead of the ref, for approval and promotion")
		var mf metaFlags
		mf.register(fs)
		if err := fs.Parse(args); err != nil {
			return err
		}
		md, err := mf.metadata()
		if err != nil {
			return err
		}
		return push(ctx, c, &st, root, *msg, md, *amend, *review, env.Out)
	case "approve":
		note := fs.String("note", "", "why you approve")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		rev, err := revArg(fs, st)
		if err != nil {
			return err
		}
		if _, err := c.Approve(ctx, st.Repo, rev, *note); err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "approved %s\n", rev.Short())
		return nil
	case "promote":
		ref := fs.String("ref", "", "ref to move (default: the working copy's ref)")
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		rev, err := revArg(fs, st)
		if err != nil {
			return err
		}
		name := *ref
		if name == "" {
			name = st.Ref
		}
		cur, _, err := c.GetRef(ctx, st.Repo, name)
		if err != nil {
			return err
		}
		if _, err := c.MoveRef(ctx, st.Repo, name, rev, cur.Target); err != nil {
			return err
		}
		if name == st.Ref {
			// The working copy's ref moved: continue from the promoted change.
			if d, err := c.GetChange(ctx, st.Repo, rev); err == nil {
				st.Head, st.ChangeID = rev, d.Change.ChangeID
				if err := SaveState(root, st); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(env.Out, "%s now at %s\n", name, rev.Short())
		return nil
	case "log":
		n := fs.Int("n", 20, "number of changes")
		if err := fs.Parse(args); err != nil {
			return err
		}
		changes, err := c.Log(ctx, st.Repo, st.Ref, *n)
		if err != nil {
			return err
		}
		for _, ch := range changes {
			printChange(env.Out, ch, ch.Metadata)
		}
		return nil
	case "show":
		if err := fs.Parse(args); err != nil {
			return err
		}
		rev, err := revArg(fs, st)
		if err != nil {
			return err
		}
		d, err := c.GetChange(ctx, st.Repo, rev)
		if err != nil {
			return err
		}
		printChange(env.Out, d.Change, d.EffectiveMetadata)
		fmt.Fprintf(env.Out, "  tree %s\n", d.Change.Tree)
		if d.LatestRevision != d.Change.Revision {
			fmt.Fprintf(env.Out, "  superseded by %s\n", d.LatestRevision.Short())
		}
		for _, a := range d.Annotations {
			fmt.Fprintf(env.Out, "  note by %s at %s: %s\n", a.Author, a.CreatedAt.Format(time.RFC3339), a.Note)
		}
		return nil
	case "annotate":
		note := fs.String("note", "", "free text")
		var mf metaFlags
		mf.register(fs)
		if err := fs.Parse(reorder(args)); err != nil {
			return err
		}
		rev, err := revArg(fs, st)
		if err != nil {
			return err
		}
		md, err := mf.metadata()
		if err != nil {
			return err
		}
		if _, err := c.Annotate(ctx, st.Repo, rev, *note, md); err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "annotated %s\n", rev.Short())
		return nil
	case "layers":
		return layersCmd(ctx, c, st, root, args, env.Out)
	case "audit":
		n := fs.Int("n", 20, "records to print")
		verify := fs.Bool("verify", false, "only verify, print nothing else")
		anchors := anchorFlags(fs, env)
		if err := fs.Parse(args); err != nil {
			return err
		}
		ac, err := anchors(workspaceWitness(root))
		if err != nil {
			return err
		}
		return auditCmd(ctx, c, st.Repo, *n, *verify, ac, env.Out)
	case "find":
		wi := fs.String("work-item", "", "work item")
		label := fs.String("label", "", "label")
		if err := fs.Parse(args); err != nil {
			return err
		}
		var res []core.ChangeSummary
		switch {
		case *wi != "":
			w, err := core.ParseWorkItem(*wi)
			if err != nil {
				return err
			}
			res, err = c.FindWorkItem(ctx, w)
			if err != nil {
				return err
			}
		case *label != "":
			res, err = c.FindLabel(ctx, *label)
			if err != nil {
				return err
			}
		default:
			return errors.New("pass --work-item or --label")
		}
		for _, s := range res {
			fmt.Fprintf(env.Out, "%s %-20s %s %s (%s, via %s)\n", s.Revision.Short(), s.Repo, s.CreatedAt.Format("2006-01-02"), s.Subject, s.Author, s.Via)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q (try strata help)", cmd)
}

func repoCmd(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 {
		return errors.New("usage: strata repo create|list")
	}
	fs := flag.NewFlagSet("repo", flag.ContinueOnError)
	fs.SetOutput(env.Out)
	api := fs.String("api", "", "API URL")
	desc := fs.String("d", "", "description")
	isLayer := fs.Bool("layer", false, "create a Layer repository")
	extends := fs.String("extends", "", "inherit from LAYER[@REF]")
	if err := fs.Parse(reorder(args[1:])); err != nil {
		return err
	}
	st, _, _ := LoadState(env.Dir)
	c, err := env.client(firstNonEmpty(*api, st.API))
	if err != nil {
		return err
	}
	switch args[0] {
	case "create":
		if fs.NArg() != 1 {
			return errors.New("usage: strata repo create NAME")
		}
		in := NewRepo{Name: fs.Arg(0), Description: *desc, Kind: core.KindCode}
		if *isLayer {
			in.Kind = core.KindLayer
		}
		if *extends != "" {
			l, err := core.ParseLayerRef(*extends)
			if err != nil {
				return err
			}
			in.Extends = &l
		}
		r, err := c.CreateRepoWith(ctx, in)
		if err != nil {
			return err
		}
		fmt.Fprintf(env.Out, "created %s %s", r.Kind, r.Name)
		if r.Extends != nil {
			fmt.Fprintf(env.Out, " extending %s", r.Extends)
		}
		fmt.Fprintln(env.Out)
	case "list":
		repos, err := c.ListRepos(ctx)
		if err != nil {
			return err
		}
		for _, r := range repos {
			fmt.Fprintf(env.Out, "%-24s %s\n", r.Name, r.Description)
		}
	default:
		return fmt.Errorf("unknown repo command %q", args[0])
	}
	return nil
}

// reorder moves positional arguments after flags, so "clone repo --ref x" works.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !isBool(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func isBool(f string) bool {
	switch strings.TrimLeft(f, "-") {
	case "amend", "layer", "verify":
		return true
	}
	return false
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func revArg(fs *flag.FlagSet, st State) (core.Hash, error) {
	if fs.NArg() > 0 {
		h := core.Hash(fs.Arg(0))
		if !h.Valid() {
			return "", fmt.Errorf("revision %q: give the full 64-character hash", fs.Arg(0))
		}
		return h, nil
	}
	if st.Head == "" {
		return "", errors.New("no HEAD yet")
	}
	return st.Head, nil
}

func printChange(w io.Writer, c core.Change, md core.Metadata) {
	fmt.Fprintf(w, "%s  change %s\n", c.Revision.Short(), c.ChangeID[:13])
	fmt.Fprintf(w, "  %s  %s\n", c.Author, c.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  %s\n", c.Subject())
	var tags []string
	for _, wi := range md.WorkItems {
		tags = append(tags, wi.String())
	}
	for _, l := range md.Labels {
		tags = append(tags, "#"+l)
	}
	for k, v := range md.Attributes {
		tags = append(tags, k+"="+v)
	}
	if len(tags) > 0 {
		fmt.Fprintf(w, "  [%s]\n", strings.Join(tags, " "))
	}
}

func push(ctx context.Context, c *Client, st *State, root, msg string, md core.Metadata, amend bool, review string, out io.Writer) error {
	if strings.TrimSpace(msg) == "" {
		return errors.New("a message is required: -m \"PAY-42 what changed\"")
	}
	if left, err := unresolved(root); err != nil {
		return err
	} else if len(left) > 0 {
		return fmt.Errorf("unresolved conflicts in %v: edit them (remove the <<<<<<< markers, delete any .theirs copy), then push", left)
	}
	snap, err := TakeSnapshot(root)
	if err != nil {
		return err
	}
	sent, err := snap.Upload(ctx, c, st.Repo)
	if err != nil {
		return err
	}
	in := NewChange{Tree: snap.Root, Message: msg, Metadata: md, Ref: st.Ref, Parents: []core.Hash{}}
	expected := st.Head
	in.ExpectedRef = &expected
	if amend {
		if st.Head == "" {
			return errors.New("nothing to amend")
		}
		head, err := c.GetChange(ctx, st.Repo, st.Head)
		if err != nil {
			return err
		}
		in.ChangeID = head.Change.ChangeID
		in.Parents = head.Change.Parents
	} else if st.Head != "" {
		in.Parents = []core.Hash{st.Head}
	}
	if review != "" {
		// A review push leaves the working copy's ref and HEAD alone: the
		// change waits on review/NAME until it is approved and promoted.
		in.Ref = "review/" + review
		cur, _, err := c.GetRef(ctx, st.Repo, in.Ref)
		if err != nil {
			return err
		}
		in.ExpectedRef = &cur.Target
		ch, err := c.CreateChange(ctx, st.Repo, in)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "pushed %s to %s for review (%d files, %d new objects)\nask a colleague: strata approve %s\nthen: strata promote %s\n",
			ch.Revision.Short(), in.Ref, snap.Files, sent, ch.Revision, ch.Revision)
		return nil
	}
	ch, err := c.CreateChange(ctx, st.Repo, in)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 409 {
			return fmt.Errorf("%s: %s moved on the server; run strata pull, then push again", st.Repo, st.Ref)
		}
		return err
	}
	st.Head, st.ChangeID = ch.Revision, ch.ChangeID
	if err := SaveState(root, *st); err != nil {
		return err
	}
	fmt.Fprintf(out, "pushed %s to %s (%d files, %d new objects)\n", ch.Revision.Short(), st.Ref, snap.Files, sent)
	printChange(out, ch, ch.Metadata)
	return nil
}

func clone(ctx context.Context, c *Client, repo, refName, dir string, out io.Writer) error {
	if des, err := os.ReadDir(dir); err == nil && len(des) > 0 {
		return fmt.Errorf("%s exists and is not empty", dir)
	}
	st := State{API: c.API, Repo: repo, Ref: refName}
	ref, ok, err := c.GetRef(ctx, repo, refName)
	if err != nil {
		return err
	}
	if ok {
		d, err := c.GetChange(ctx, repo, ref.Target)
		if err != nil {
			return err
		}
		n, err := Checkout(ctx, c, repo, d.Change.Tree, dir)
		if err != nil {
			return err
		}
		st.Head, st.ChangeID = d.Change.Revision, d.Change.ChangeID
		fmt.Fprintf(out, "cloned %s@%s into %s (%d files)\n", repo, ref.Target.Short(), dir, n)
	} else {
		fmt.Fprintf(out, "cloned empty repository %s into %s\n", repo, dir)
	}
	if err := SaveState(dir, st); err != nil {
		return err
	}
	r, err := c.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if r.Extends != nil {
		return PullLayers(ctx, c, st, dir, out)
	}
	return nil
}

func layersCmd(ctx context.Context, c *Client, st State, root string, args []string, out io.Writer) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show":
		eff, err := c.Layers(ctx, st.Repo, st.Ref, true)
		if err != nil {
			return err
		}
		printEffective(out, eff)
		return nil
	case "pull":
		return PullLayers(ctx, c, st, root, out)
	case "set", "unset":
		var l *core.LayerRef
		if args[0] == "set" {
			if len(args) != 2 {
				return errors.New("usage: strata layers set LAYER[@REF]")
			}
			p, err := core.ParseLayerRef(args[1])
			if err != nil {
				return err
			}
			l = &p
		}
		violations, err := c.SetExtends(ctx, st.Repo, l)
		if err != nil {
			return err
		}
		if l == nil {
			fmt.Fprintf(out, "%s no longer extends a layer\n", st.Repo)
		} else {
			fmt.Fprintf(out, "%s now extends %s\n", st.Repo, l)
		}
		for _, v := range violations {
			fmt.Fprintf(out, "  warning: %s is locked by %s; remove it from %s or pushes will be rejected\n", v.Path, v.LockedBy, st.Repo)
		}
		return PullLayers(ctx, c, st, root, out)
	case "fleet":
		layer := st.Repo
		if len(args) > 1 {
			layer = args[1]
		}
		subs, err := c.Subscribers(ctx, layer)
		if err != nil {
			return err
		}
		if len(subs) == 0 {
			fmt.Fprintf(out, "no repository extends %s yet\n", layer)
		}
		for _, s := range subs {
			ref := ""
			if s.Repo.Extends != nil {
				ref = "@" + s.Repo.Extends.Ref
			}
			fmt.Fprintf(out, "%*s%s (%s) via %s%s\n", 2*(s.Depth-1), "", s.Repo.Name, s.Repo.Kind, s.Via, ref)
		}
		return nil
	}
	return fmt.Errorf("unknown layers command %q", args[0])
}
