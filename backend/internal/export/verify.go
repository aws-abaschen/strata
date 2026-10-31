package export

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws-abaschen/strata/backend/internal/anchor"
	"github.com/aws-abaschen/strata/backend/internal/core"
)

// Report is what Verify established.
type Report struct {
	Repo        string
	Records     int
	Changes     int
	Trees       int
	Blobs       int
	Refs        int
	Annotations int
	Approvals   int
	Omitted     int
	Anchors     []anchor.Checked
	// Unanchored counts ledger records after the last anchored one: they
	// rest on the hash chain alone.
	Unanchored int
	// BreakGlass is set when the export includes the break-glass ledger.
	BreakGlass *ChainReport
}

// ChainReport covers a ledger exported without content (break-glass).
type ChainReport struct {
	Records    int
	Anchors    []anchor.Checked
	Unanchored int
}

type verifier struct {
	dir   string
	side  string
	git   gitRepo
	rep   *Report
	trees map[[2]string]bool // (strata tree, git tree) pairs already checked
	blobs map[string]bool
}

// Verify checks an export offline. The ledger (hash-chained, and checked
// against signed anchors when trust has a key) is the list of what the
// export must contain; every change, tree and file is checked against the
// SHA-256 hashes that list implies, and the Git repository against them.
func Verify(dir string, trust anchor.Trust) (Report, error) {
	v := &verifier{dir: dir, side: filepath.Join(dir, "strata"), git: gitRepo{filepath.Join(dir, "git")}, rep: &Report{},
		trees: map[[2]string]bool{}, blobs: map[string]bool{}}
	err := v.run(trust)
	if err != nil {
		err = fmt.Errorf("%w: %v", errFailed, err)
	}
	return *v.rep, err
}

func (v *verifier) run(trust anchor.Trust) error {
	var m Manifest
	if err := readJSON(filepath.Join(v.side, "manifest.json"), &m); err != nil {
		return err
	}
	if m.Format != Format {
		return fmt.Errorf("manifest format %q, want %q", m.Format, Format)
	}
	v.rep.Repo = m.Repo

	// 1. The ledger: complete from seq 1, hash-chained, and anchored.
	recs, err := readJSONL[core.AuditRecord](filepath.Join(v.side, "audit.jsonl"))
	if err != nil {
		return err
	}
	if len(recs) == 0 || recs[0].Seq != 1 {
		return errors.New("audit.jsonl must start at seq 1")
	}
	for _, r := range recs {
		if r.Repo != m.Repo {
			return fmt.Errorf("ledger seq %d belongs to %q, not %q", r.Seq, r.Repo, m.Repo)
		}
	}
	if _, err := core.VerifyAudit(recs, ""); err != nil {
		return fmt.Errorf("ledger: %v", err)
	}
	v.rep.Records = len(recs)
	if v.rep.Anchors, v.rep.Unanchored, err = v.anchors("anchors.json", recs, trust); err != nil {
		return err
	}

	// 2. Replay the ledger: revisions, ref states, annotations, approvals.
	l, err := replay(recs)
	if err != nil {
		return err
	}

	// 3. Exactly the ledger's revisions are exported, each sealed correctly.
	files, err := listDir(filepath.Join(v.side, "changes"), ".json")
	if err != nil {
		return err
	}
	revRefs, err := v.git.refs("refs/strata/revisions/")
	if err != nil {
		return err
	}
	if err := sameSet("revision", l.revSeq, files, "is missing from strata/changes"); err != nil {
		return err
	}
	revNames := map[string]bool{}
	for name := range revRefs {
		revNames[strings.TrimPrefix(name, "refs/strata/revisions/")] = true
	}
	if err := sameSet("revision", l.revSeq, revNames, "has no ref under refs/strata/revisions in Git"); err != nil {
		return err
	}
	changes := map[core.Hash]core.Change{}
	for rev := range l.revSeq {
		b, err := os.ReadFile(filepath.Join(v.side, "changes", rev+".json"))
		if err != nil {
			return err
		}
		if string(core.HashBytes(b)) != rev {
			return fmt.Errorf("strata/changes/%s.json does not hash to its revision (ledger seq %d)", core.Hash(rev).Short(), l.revSeq[rev])
		}
		var c core.Change
		if err := json.Unmarshal(b, &c); err != nil {
			return fmt.Errorf("change %s: %v", core.Hash(rev).Short(), err)
		}
		if c.Repo != m.Repo {
			return fmt.Errorf("change %s belongs to %q", core.Hash(rev).Short(), c.Repo)
		}
		c.Revision = core.Hash(rev)
		changes[c.Revision] = c
	}

	// 4. Each Git commit carries exactly its change's tree, parents and author.
	for rev, c := range changes {
		sha := revRefs["refs/strata/revisions/"+string(rev)]
		body, err := v.git.getKind(sha, "commit")
		if err != nil {
			return fmt.Errorf("revision %s: %v", rev.Short(), err)
		}
		gc, err := parseCommit(body)
		if err != nil {
			return fmt.Errorf("revision %s: %v", rev.Short(), err)
		}
		if err := v.tree(c.Tree, gc.Tree, "/"); err != nil {
			return fmt.Errorf("revision %s: %v", rev.Short(), err)
		}
		if len(gc.Parents) != len(c.Parents) {
			return fmt.Errorf("revision %s: Git commit has %d parents, the change has %d", rev.Short(), len(gc.Parents), len(c.Parents))
		}
		for i, p := range c.Parents {
			if revRefs["refs/strata/revisions/"+string(p)] != gc.Parents[i] {
				return fmt.Errorf("revision %s: Git parent %d is not the commit of %s", rev.Short(), i+1, p.Short())
			}
		}
		want := ident(c.Author, c.CreatedAt)
		if gc.Author != want || gc.Committer != want {
			return fmt.Errorf("revision %s: Git author is %q, want %q", rev.Short(), gc.Author, want)
		}
		if gc.Message != commitMessage(c) {
			return fmt.Errorf("revision %s: Git commit message differs from the change", rev.Short())
		}
	}
	v.rep.Changes = len(changes)

	// 5. Branches are exactly the refs the ledger leaves standing.
	heads, err := v.git.refs("refs/heads/")
	if err != nil {
		return err
	}
	omittedRefs := map[string]bool{}
	for _, r := range m.OmittedRefs {
		if validGitRef(r) {
			return fmt.Errorf("manifest omits ref %q, which Git can hold", r)
		}
		omittedRefs[r] = true
	}
	for name, target := range l.refs {
		if omittedRefs[name] {
			continue
		}
		got, ok := heads["refs/heads/"+name]
		if !ok {
			return fmt.Errorf("ref %s (at %s per the ledger) is missing from Git", name, target.Short())
		}
		if got != revRefs["refs/strata/revisions/"+string(target)] {
			return fmt.Errorf("ref %s in Git does not point at %s, its target per the ledger", name, target.Short())
		}
	}
	for full := range heads {
		name := strings.TrimPrefix(full, "refs/heads/")
		if _, ok := l.refs[name]; !ok {
			return fmt.Errorf("Git has ref %s, which the ledger does not leave standing", name)
		}
	}
	v.rep.Refs = len(heads)

	// 6. Annotations and approvals match the ledger one for one.
	anns, err := readJSONL[core.Annotation](filepath.Join(v.side, "annotations.jsonl"))
	if err != nil {
		return err
	}
	var gotAnn []string
	for _, a := range anns {
		gotAnn = append(gotAnn, annKey(a.Revision, a.Author, a.Note, a.Metadata))
	}
	if err := sameList("annotation", l.annotations, gotAnn); err != nil {
		return err
	}
	apps, err := readJSONL[core.Approval](filepath.Join(v.side, "approvals.jsonl"))
	if err != nil {
		return err
	}
	var gotApp []string
	for _, a := range apps {
		gotApp = append(gotApp, appKey(a.Revision, a.Approver, a.Note))
	}
	if err := sameList("approval", l.approvals, gotApp); err != nil {
		return err
	}
	v.rep.Annotations, v.rep.Approvals = len(anns), len(apps)

	// 7. The repository's Layer setting is the last one the ledger recorded.
	var repo core.Repo
	if err := readJSON(filepath.Join(v.side, "repo.json"), &repo); err != nil {
		return err
	}
	if repo.Name != m.Repo || !sameExtends(repo.Extends, l.extends) {
		return fmt.Errorf("repo.json (extends %v) does not match the ledger (extends %v)", repo.Extends, l.extends)
	}

	// 8. Optional break-glass ledger.
	bgPath := filepath.Join(v.side, "break-glass", "audit.jsonl")
	if _, err := os.Stat(bgPath); err == nil {
		bg, err := readJSONL[core.AuditRecord](bgPath)
		if err != nil {
			return err
		}
		if len(bg) > 0 {
			if bg[0].Seq != 1 {
				return errors.New("break-glass ledger must start at seq 1")
			}
			if _, err := core.VerifyAudit(bg, ""); err != nil {
				return fmt.Errorf("break-glass ledger: %v", err)
			}
		}
		ch, un, err := v.anchors("break-glass/anchors.json", bg, trust)
		if err != nil {
			return fmt.Errorf("break-glass: %v", err)
		}
		v.rep.BreakGlass = &ChainReport{Records: len(bg), Anchors: ch, Unanchored: un}
	} else if m.BreakGlass {
		return errors.New("manifest says the break-glass ledger is included, but it is missing")
	}
	v.rep.Trees, v.rep.Blobs = len(v.trees), len(v.blobs)
	return nil
}

// anchors checks a chain's anchors and returns them with the number of
// records after the last anchored one.
//
// With a pinned key an export must carry anchors: someone who rewrote the
// ledger consistently would otherwise only have to drop them. Dropping some
// does not help: any remaining anchor covers every record up to its own seq
// through the hash chain. Anchors cut from the end shrink that coverage, and
// cannot be detected offline, so the uncovered count is always reported.
func (v *verifier) anchors(name string, recs []core.AuditRecord, trust anchor.Trust) ([]anchor.Checked, int, error) {
	var ps []core.AnchorProof
	if err := readJSON(filepath.Join(v.side, filepath.FromSlash(name)), &ps); err != nil {
		return nil, 0, err
	}
	if len(ps) == 0 {
		if trust.Key != nil && len(recs) > 0 {
			return nil, 0, fmt.Errorf("%s carries no anchors: nothing ties this ledger to the pinned key", name)
		}
		return nil, len(recs), nil
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Statement.Seq < ps[j].Statement.Seq })
	ch, err := anchor.VerifyChain(recs, ps, trust)
	if err != nil {
		return nil, 0, fmt.Errorf("anchors: %v", err)
	}
	var last int64
	for _, c := range ch {
		last = max(last, c.Leaf.Seq)
	}
	return ch, len(recs) - int(last), nil
}

// tree checks that a Git tree holds exactly a Strata tree's entries.
func (v *verifier) tree(h core.Hash, sha, at string) error {
	if v.trees[[2]string{string(h), sha}] {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(v.side, "trees", string(h)+".json"))
	if err != nil {
		return fmt.Errorf("tree %s (%s) is missing from strata/trees", h.Short(), at)
	}
	t, err := core.DecodeTree(b, h)
	if err != nil {
		return fmt.Errorf("tree %s (%s): %v", h.Short(), at, err)
	}
	body, err := v.git.getKind(sha, "tree")
	if err != nil {
		return err
	}
	ges, err := parseTree(body)
	if err != nil {
		return err
	}
	byName := map[string]gitEntry{}
	for _, g := range ges {
		byName[g.Name] = g
	}
	kept := 0
	for _, e := range t.Entries {
		p := at + e.Name
		if omitName(e.Name) {
			if _, ok := byName[e.Name]; ok {
				return fmt.Errorf("%s: Git holds an entry the format omits", p)
			}
			v.rep.Omitted++
			continue
		}
		kept++
		g, ok := byName[e.Name]
		if !ok {
			return fmt.Errorf("%s is missing from Git", p)
		}
		if g.Mode != gitMode[e.Kind] {
			return fmt.Errorf("%s: Git mode %s, want %s (%s)", p, g.Mode, gitMode[e.Kind], e.Kind)
		}
		if e.Kind == core.KindDir {
			if err := v.tree(e.Hash, g.SHA, p+"/"); err != nil {
				return err
			}
			continue
		}
		if err := v.blob(e, g.SHA, p); err != nil {
			return err
		}
	}
	if kept != len(ges) {
		return fmt.Errorf("Git tree at %s has entries the change does not", at)
	}
	v.trees[[2]string{string(h), sha}] = true
	return nil
}

func (v *verifier) blob(e core.TreeEntry, sha, p string) error {
	key := string(e.Hash) + sha
	if v.blobs[key] {
		return nil
	}
	b, err := v.git.getKind(sha, "blob")
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if core.HashBytes(b) != e.Hash {
		return fmt.Errorf("%s: content does not match its SHA-256 in the change", p)
	}
	if int64(len(b)) != e.Size {
		return fmt.Errorf("%s: %d bytes, the change says %d", p, len(b), e.Size)
	}
	v.blobs[key] = true
	return nil
}

// ledger is what replaying a repository's audit chain establishes.
type ledger struct {
	revSeq      map[string]int64     // revision -> seq of its ChangeCreated
	refs        map[string]core.Hash // ref -> target, deleted refs removed
	annotations []string
	approvals   []string
	extends     *core.LayerRef
}

func replay(recs []core.AuditRecord) (ledger, error) {
	l := ledger{revSeq: map[string]int64{}, refs: map[string]core.Hash{}}
	for _, r := range recs {
		var d struct {
			Revision core.Hash       `json:"revision"`
			Ref      json.RawMessage `json:"ref"`
			Note     string          `json:"note"`
			Metadata core.Metadata   `json:"metadata"`
			Approver string          `json:"approver"`
			Settings *core.Repo      `json:"settings"`
		}
		if err := json.Unmarshal(r.Detail, &d); err != nil {
			return l, fmt.Errorf("ledger seq %d: %v", r.Seq, err)
		}
		switch r.Action {
		case "ChangeCreated":
			if _, ok := l.revSeq[string(d.Revision)]; !ok {
				l.revSeq[string(d.Revision)] = r.Seq
			}
			var name string
			if len(d.Ref) > 0 && json.Unmarshal(d.Ref, &name) == nil && name != "" {
				l.refs[name] = d.Revision
			}
		case "RefUpdated":
			var ref core.Ref
			if err := json.Unmarshal(d.Ref, &ref); err != nil {
				return l, fmt.Errorf("ledger seq %d: RefUpdated: %v", r.Seq, err)
			}
			if ref.Target == "" {
				delete(l.refs, ref.Name)
			} else {
				l.refs[ref.Name] = ref.Target
			}
		case "ChangeAnnotated":
			l.annotations = append(l.annotations, annKey(d.Revision, r.Actor, d.Note, d.Metadata))
		case "ApprovalGranted":
			l.approvals = append(l.approvals, appKey(d.Revision, d.Approver, d.Note))
		case "RepoCreated", "RepoSettingsChanged":
			if d.Settings != nil {
				l.extends = d.Settings.Extends
			}
		}
	}
	for name, target := range l.refs {
		if _, ok := l.revSeq[string(target)]; !ok {
			return l, fmt.Errorf("ledger moves ref %s to %s, which it never recorded", name, target.Short())
		}
	}
	return l, nil
}

func annKey(rev core.Hash, author, note string, md core.Metadata) string {
	b, _ := json.Marshal(md)
	return fmt.Sprintf("%s by %s note=%q metadata=%s", rev.Short(), author, note, b)
}

func appKey(rev core.Hash, approver, note string) string {
	return fmt.Sprintf("%s by %s note=%q", rev.Short(), approver, note)
}

func sameExtends(a, b *core.LayerRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameSet[V any](what string, want map[string]V, got map[string]bool, missing string) error {
	for k := range want {
		if !got[k] {
			return fmt.Errorf("%s %s recorded in the ledger %s", what, core.Hash(k).Short(), missing)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			return fmt.Errorf("%s %s is in the export but not in the ledger", what, core.Hash(k).Short())
		}
	}
	return nil
}

// sameList compares two multisets of keys.
func sameList(what string, want, got []string) error {
	count := map[string]int{}
	for _, k := range want {
		count[k]++
	}
	for _, k := range got {
		count[k]--
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch n := count[k]; {
		case n > 0:
			return fmt.Errorf("%s %s recorded in the ledger is missing from the export", what, k)
		case n < 0:
			return fmt.Errorf("%s %s is in the export but not in the ledger", what, k)
		}
	}
	return nil
}

func listDir(dir, ext string) (map[string]bool, error) {
	out := map[string]bool{}
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ext); ok && !e.IsDir() {
			out[n] = true
		}
	}
	return out, nil
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("%s is missing", filepath.Base(p))
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %v", filepath.Base(p), err)
	}
	return nil
}

func readJSONL[T any](p string) ([]T, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("%s is missing", filepath.Base(p))
	}
	var out []T
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("%s line %d: %v", filepath.Base(p), n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}
