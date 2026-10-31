package export

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// gitRepo reads and writes a bare Git repository with loose objects only.
// Writing objects directly (no git binary, no library) keeps exports
// byte-for-byte reproducible and lets the verifier run where Git is absent.
// Git's SHA-1 is only an address here: integrity comes from Strata's SHA-256
// hashes, which the verifier checks against the content.
type gitRepo struct{ dir string }

func initGit(dir string) (gitRepo, error) {
	for _, d := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return gitRepo{}, err
		}
	}
	files := map[string]string{
		"HEAD":   "ref: refs/heads/main\n",
		"config": "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			return gitRepo{}, err
		}
	}
	return gitRepo{dir}, nil
}

func (g gitRepo) objectPath(sha string) string {
	return filepath.Join(g.dir, "objects", sha[:2], sha[2:])
}

// put stores an object and returns its SHA-1.
func (g gitRepo) put(kind string, body []byte) (string, error) {
	data := append([]byte(fmt.Sprintf("%s %d\x00", kind, len(body))), body...)
	sum := sha1.Sum(data)
	sha := hex.EncodeToString(sum[:])
	p := g.objectPath(sha)
	if _, err := os.Stat(p); err == nil {
		return sha, nil
	}
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	zw.Write(data)
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return sha, os.WriteFile(p, buf.Bytes(), 0o444)
}

// get reads an object and checks that it hashes to its name.
func (g gitRepo) get(sha string) (string, []byte, error) {
	if len(sha) != 40 {
		return "", nil, fmt.Errorf("git object %q: bad name", sha)
	}
	f, err := os.Open(g.objectPath(sha))
	if err != nil {
		return "", nil, fmt.Errorf("git object %s is missing", sha)
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return "", nil, fmt.Errorf("git object %s is corrupt: %v", sha, err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return "", nil, fmt.Errorf("git object %s is corrupt: %v", sha, err)
	}
	sum := sha1.Sum(data)
	if hex.EncodeToString(sum[:]) != sha {
		return "", nil, fmt.Errorf("git object %s does not match its name (content changed)", sha)
	}
	hdr, body, ok := bytes.Cut(data, []byte{0})
	kind, size, _ := strings.Cut(string(hdr), " ")
	if n, err := strconv.Atoi(size); !ok || err != nil || n != len(body) {
		return "", nil, fmt.Errorf("git object %s has a bad header", sha)
	}
	return kind, body, nil
}

func (g gitRepo) getKind(sha, want string) ([]byte, error) {
	kind, body, err := g.get(sha)
	if err == nil && kind != want {
		err = fmt.Errorf("git object %s is a %s, want %s", sha, kind, want)
	}
	return body, err
}

func (g gitRepo) setRef(name, sha string) error {
	p := filepath.Join(g.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(sha+"\n"), 0o644)
}

// refs lists the loose refs under prefix (e.g. "refs/heads/"), by full name.
func (g gitRepo) refs(prefix string) (map[string]string, error) {
	out := map[string]string{}
	root := filepath.Join(g.dir, filepath.FromSlash(prefix))
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(g.dir, p)
		out[filepath.ToSlash(rel)] = strings.TrimSpace(string(b))
		return nil
	})
	return out, err
}

// Git tree modes for Strata entry kinds.
var gitMode = map[core.EntryKind]string{core.KindFile: "100644", core.KindExec: "100755", core.KindDir: "40000"}

type gitEntry struct {
	Mode, Name, SHA string
}

// encodeTree sorts entries the way Git does (a directory sorts as name + "/").
func encodeTree(es []gitEntry) ([]byte, error) {
	key := func(e gitEntry) string {
		if e.Mode == "40000" {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(es, func(i, j int) bool { return key(es[i]) < key(es[j]) })
	var b bytes.Buffer
	for _, e := range es {
		raw, err := hex.DecodeString(e.SHA)
		if err != nil || len(raw) != 20 {
			return nil, fmt.Errorf("tree entry %s: bad sha", e.Name)
		}
		fmt.Fprintf(&b, "%s %s\x00", e.Mode, e.Name)
		b.Write(raw)
	}
	return b.Bytes(), nil
}

func parseTree(body []byte) ([]gitEntry, error) {
	var out []gitEntry
	for len(body) > 0 {
		hdr, rest, ok := bytes.Cut(body, []byte{0})
		mode, name, ok2 := strings.Cut(string(hdr), " ")
		if !ok || !ok2 || len(rest) < 20 {
			return nil, errors.New("malformed git tree")
		}
		out = append(out, gitEntry{Mode: mode, Name: name, SHA: hex.EncodeToString(rest[:20])})
		body = rest[20:]
	}
	return out, nil
}

// ident renders a Strata identity as a Git author: "id <id> unix +0000".
func ident(id string, at time.Time) string {
	clean := strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r == '\n' || r == '\r' || r == 0 {
			return '_'
		}
		return r
	}, id)
	return fmt.Sprintf("%s <%s> %d +0000", clean, clean, at.Unix())
}

// commitMessage is the change message with trailers naming the Strata change.
func commitMessage(c core.Change) string {
	return strings.TrimRight(c.Message, "\n") + "\n\nStrata-Revision: " + string(c.Revision) + "\nStrata-Change-Id: " + c.ChangeID + "\n"
}

func encodeCommit(tree string, parents []string, c core.Change) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, p := range parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	id := ident(c.Author, c.CreatedAt)
	fmt.Fprintf(&b, "author %s\ncommitter %s\n\n%s", id, id, commitMessage(c))
	return []byte(b.String())
}

type gitCommit struct {
	Tree, Author, Committer, Message string
	Parents                          []string
}

func parseCommit(body []byte) (gitCommit, error) {
	hdr, msg, ok := strings.Cut(string(body), "\n\n")
	if !ok {
		return gitCommit{}, errors.New("malformed git commit")
	}
	c := gitCommit{Message: msg}
	for _, line := range strings.Split(hdr, "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.Author = v
		case "committer":
			c.Committer = v
		}
	}
	return c, nil
}

// omitName reports names Git refuses inside a tree (".git" in any case).
// Such entries are kept in the sidecar tree and left out of the Git tree.
func omitName(n string) bool { return strings.EqualFold(n, ".git") }

// validGitRef reports whether a Strata ref name is also a valid Git ref name.
func validGitRef(n string) bool {
	for _, part := range strings.Split(n, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return !strings.HasSuffix(n, ".") && path.Clean(n) == n
}
