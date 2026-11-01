// Package core holds Strata's data model: content hashes, trees, changes and
// the work-tracking metadata attached to changes.
package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Hash is a lowercase hex SHA-256 of an object's bytes. SHA-256 is used because
// S3 verifies it natively on upload (x-amz-checksum-sha256), so a client can
// never store content under the wrong address.
type Hash string

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// HashBytes returns the content address of b.
func HashBytes(b []byte) Hash {
	sum := sha256.Sum256(b)
	return Hash(hex.EncodeToString(sum[:]))
}

// Valid reports whether h is a well-formed hash.
func (h Hash) Valid() bool { return hashRe.MatchString(string(h)) }

// Short returns the first 12 characters, for display.
func (h Hash) Short() string {
	if len(h) < 12 {
		return string(h)
	}
	return string(h[:12])
}

// ErrInvalid wraps every validation error, so callers can map it to a 400.
var ErrInvalid = errors.New("invalid")

// ErrPolicy means the request is well-formed but a governance rule forbids
// it, for example a push that changes a file a Layer locked.
var ErrPolicy = errors.New("policy violation")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var (
	repoRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	refRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,62}$`)
	attrRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)
	sysRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	keyRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._#-]{0,63}$`)
)

// ValidateRepoName checks a repository name.
func ValidateRepoName(n string) error {
	if !repoRe.MatchString(n) {
		return invalid("repository name %q: use 2-63 lowercase letters, digits or dashes", n)
	}
	if n == DRChain {
		return invalid("repository name %q is reserved", n)
	}
	return nil
}

// ValidateRefName checks a ref (branch) name.
func ValidateRefName(n string) error {
	if !refRe.MatchString(n) || strings.Contains(n, "..") || strings.HasSuffix(n, "/") {
		return invalid("ref name %q", n)
	}
	return nil
}

// EntryKind is the type of a tree entry.
type EntryKind string

const (
	KindFile EntryKind = "file"
	KindExec EntryKind = "exec"
	KindDir  EntryKind = "dir"
)

// TreeEntry is one name in a directory.
type TreeEntry struct {
	Name string    `json:"name"`
	Kind EntryKind `json:"kind"`
	Hash Hash      `json:"hash"`
	Size int64     `json:"size,omitempty"`
}

// Tree is a directory listing. Trees are stored as objects like files.
type Tree struct {
	Entries []TreeEntry `json:"entries"`
}

// Encode validates the tree, sorts it by name and returns its canonical bytes
// and hash. Two trees with the same content always have the same hash.
func (t Tree) Encode() ([]byte, Hash, error) {
	entries := slices.Clone(t.Entries)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for i, e := range entries {
		if e.Name == "" || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, "/\\\x00") {
			return nil, "", invalid("tree entry name %q", e.Name)
		}
		if i > 0 && entries[i-1].Name == e.Name {
			return nil, "", invalid("duplicate tree entry %q", e.Name)
		}
		if e.Kind != KindFile && e.Kind != KindExec && e.Kind != KindDir {
			return nil, "", invalid("tree entry %q kind %q", e.Name, e.Kind)
		}
		if !e.Hash.Valid() {
			return nil, "", invalid("tree entry %q hash", e.Name)
		}
		if e.Kind == KindDir {
			entries[i].Size = 0
		}
	}
	if entries == nil {
		entries = []TreeEntry{}
	}
	b, err := json.Marshal(Tree{Entries: entries})
	if err != nil {
		return nil, "", err
	}
	return b, HashBytes(b), nil
}

// DecodeTree parses and validates tree bytes, and checks they hash to want.
func DecodeTree(b []byte, want Hash) (Tree, error) {
	var t Tree
	if err := json.Unmarshal(b, &t); err != nil {
		return Tree{}, invalid("object %s is not a tree", want.Short())
	}
	canon, _, err := t.Encode()
	if err != nil {
		return Tree{}, err
	}
	if HashBytes(b) != want || string(canon) != string(b) {
		return Tree{}, invalid("tree %s is not in canonical form", want.Short())
	}
	return t, nil
}

// WorkItem links a change to an item in an external tracker (Jira, Azure
// Boards, ServiceNow, GitHub issues...).
type WorkItem struct {
	System string `json:"system"`
	Key    string `json:"key"`
	URL    string `json:"url,omitempty"`
}

// String renders "system:KEY".
func (w WorkItem) String() string { return w.System + ":" + w.Key }

// ParseWorkItem parses "KEY" (Jira by default) or "system:KEY".
func ParseWorkItem(s string) (WorkItem, error) {
	sys, key, ok := strings.Cut(s, ":")
	if !ok {
		sys, key = "jira", s
	}
	w := WorkItem{System: strings.ToLower(sys), Key: key}
	return w, w.validate()
}

func (w WorkItem) validate() error {
	if !sysRe.MatchString(w.System) {
		return invalid("work item system %q", w.System)
	}
	if !keyRe.MatchString(w.Key) {
		return invalid("work item key %q", w.Key)
	}
	if w.URL != "" && !strings.HasPrefix(w.URL, "https://") {
		return invalid("work item URL must use https")
	}
	return nil
}

// Metadata is structured, queryable data attached to a change: links to work
// items, labels, and free key/value attributes (risk class, ticket type,
// approver, cost centre...).
type Metadata struct {
	WorkItems  []WorkItem        `json:"workItems,omitempty"`
	Labels     []string          `json:"labels,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Limits keep one change's index writes within a DynamoDB transaction.
const (
	MaxWorkItems  = 20
	MaxLabels     = 20
	MaxAttributes = 50
	MaxAttrValue  = 1024
	MaxMessage    = 64 * 1024
)

// Normalize validates, de-duplicates and sorts the metadata in place.
func (m *Metadata) Normalize() error {
	seen := map[string]bool{}
	var items []WorkItem
	for _, w := range m.WorkItems {
		w.System = strings.ToLower(w.System)
		if w.System == "jira" {
			w.Key = strings.ToUpper(w.Key)
		}
		if err := w.validate(); err != nil {
			return err
		}
		if !seen[w.String()] {
			seen[w.String()] = true
			items = append(items, w)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].String() < items[j].String() })
	m.WorkItems = items

	var labels []string
	for _, l := range m.Labels {
		l = strings.ToLower(strings.TrimSpace(l))
		if !labelRe.MatchString(l) {
			return invalid("label %q", l)
		}
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	sort.Strings(labels)
	m.Labels = labels

	for k, v := range m.Attributes {
		if !attrRe.MatchString(k) {
			return invalid("attribute name %q", k)
		}
		if len(v) > MaxAttrValue {
			return invalid("attribute %q is longer than %d bytes", k, MaxAttrValue)
		}
	}
	if len(m.Attributes) == 0 {
		m.Attributes = nil
	}
	if len(m.WorkItems) > MaxWorkItems || len(m.Labels) > MaxLabels || len(m.Attributes) > MaxAttributes {
		return invalid("at most %d work items, %d labels and %d attributes", MaxWorkItems, MaxLabels, MaxAttributes)
	}
	return nil
}

// Merge adds other's entries to m. Attributes in other win.
func (m *Metadata) Merge(other Metadata) {
	m.WorkItems = append(m.WorkItems, other.WorkItems...)
	m.Labels = append(m.Labels, other.Labels...)
	if len(other.Attributes) > 0 && m.Attributes == nil {
		m.Attributes = map[string]string{}
	}
	for k, v := range other.Attributes {
		m.Attributes[k] = v
	}
}

// jiraKeyRe finds Jira-style keys (PROJ-123) in free text.
var jiraKeyRe = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-[1-9][0-9]{0,6})\b`)

// ParseWorkItems finds Jira-style keys in a change message, so "PAY-42 fix
// rounding" links to PAY-42 with no extra flag.
func ParseWorkItems(message string) []WorkItem {
	var out []WorkItem
	for _, m := range jiraKeyRe.FindAllStringSubmatch(message, -1) {
		out = append(out, WorkItem{System: "jira", Key: m[1]})
	}
	return out
}

// Change is Strata's equivalent of a Git commit. Each revision is immutable
// and addressed by Revision. ChangeID is stable across revisions: amending a
// change creates a new revision with the same ChangeID, so reviews, tickets
// and audit records keep pointing at the right thing.
type Change struct {
	Revision    Hash      `json:"revision"`
	ChangeID    string    `json:"changeId"`
	Repo        string    `json:"repo"`
	Tree        Hash      `json:"tree"`
	Parents     []Hash    `json:"parents"`
	Predecessor Hash      `json:"predecessor,omitempty"`
	Author      string    `json:"author"`
	CreatedAt   time.Time `json:"createdAt"`
	Message     string    `json:"message"`
	Metadata    Metadata  `json:"metadata"`
}

// Seal validates the change and sets Revision to the hash of its canonical
// JSON form (with Revision empty).
func (c *Change) Seal() ([]byte, error) {
	if err := ValidateRepoName(c.Repo); err != nil {
		return nil, err
	}
	if !changeIDRe.MatchString(c.ChangeID) {
		return nil, invalid("change id %q", c.ChangeID)
	}
	if !c.Tree.Valid() {
		return nil, invalid("tree hash")
	}
	if c.Parents == nil {
		c.Parents = []Hash{}
	}
	for _, p := range c.Parents {
		if !p.Valid() {
			return nil, invalid("parent hash %q", p)
		}
	}
	if c.Predecessor != "" && !c.Predecessor.Valid() {
		return nil, invalid("predecessor hash")
	}
	if strings.TrimSpace(c.Message) == "" || len(c.Message) > MaxMessage {
		return nil, invalid("message must be 1 to %d bytes", MaxMessage)
	}
	if c.Author == "" {
		return nil, invalid("author is required")
	}
	if err := c.Metadata.Normalize(); err != nil {
		return nil, err
	}
	c.CreatedAt = c.CreatedAt.UTC().Truncate(time.Millisecond)
	b, err := c.Preimage()
	if err != nil {
		return nil, err
	}
	c.Revision = HashBytes(b)
	return json.Marshal(c)
}

// Preimage returns the exact bytes a sealed change's Revision is the SHA-256
// of: its JSON form with Revision empty. Exports store these bytes, so a
// verifier hashes them instead of re-serializing JSON.
func (c Change) Preimage() ([]byte, error) {
	c.Revision = ""
	return json.Marshal(c)
}

// Subject returns the first line of the message.
func (c Change) Subject() string {
	s, _, _ := strings.Cut(c.Message, "\n")
	return s
}

var changeIDRe = regexp.MustCompile(`^c[a-z2-7]{26}$`)

// NewChangeID returns a random change id like "c3k7...". It uses a different
// alphabet and length from revisions so the two are never confused.
func NewChangeID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "c" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// ValidChangeID reports whether s is a well-formed change id.
func ValidChangeID(s string) bool { return changeIDRe.MatchString(s) }

// Annotation adds metadata to a change after it was written, without changing
// its revision (like git notes, but indexed). Annotations are append-only.
type Annotation struct {
	Repo      string    `json:"repo"`
	Revision  Hash      `json:"revision"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	Note      string    `json:"note,omitempty"`
	Metadata  Metadata  `json:"metadata"`
}

// Approval is one person's sign-off on one exact revision. It binds to the
// revision hash, so amending the change (a new revision) starts at zero.
type Approval struct {
	Repo      string    `json:"repo"`
	Revision  Hash      `json:"revision"`
	Approver  string    `json:"approver"`
	CreatedAt time.Time `json:"createdAt"`
	Note      string    `json:"note,omitempty"`
}

// Repo is a repository record.
type Repo struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Kind        RepoKind  `json:"kind"`
	Extends     *LayerRef `json:"extends,omitempty"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedBy   string    `json:"updatedBy,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitzero"`
}

// RepoKind says whether a repository holds work (code, docs) or a Layer:
// configuration meant to be inherited by other repositories, not forked.
type RepoKind string

const (
	KindCode  RepoKind = "code"
	KindLayer RepoKind = "layer"
)

// Validate checks the kind, defaulting an empty one to code.
func (k *RepoKind) Validate() error {
	switch *k {
	case "":
		*k = KindCode
	case KindCode, KindLayer:
	default:
		return invalid("repository kind %q: use code or layer", *k)
	}
	return nil
}

// LayerRef points a repository at the Layer it inherits from. Ref lets a
// Layer publisher roll out in waves: subscribers on "canary" get a change
// before those on "main".
type LayerRef struct {
	Layer string `json:"layer"`
	Ref   string `json:"ref"`
}

// String renders "layer@ref".
func (l LayerRef) String() string { return l.Layer + "@" + l.Ref }

// ParseLayerRef parses "layer" or "layer@ref" (ref defaults to main).
func ParseLayerRef(s string) (LayerRef, error) {
	name, ref, ok := strings.Cut(s, "@")
	if !ok {
		ref = "main"
	}
	l := LayerRef{Layer: name, Ref: ref}
	if err := ValidateRepoName(name); err != nil {
		return l, err
	}
	return l, ValidateRefName(ref)
}

// JoinParts concatenates append-mode contributions, top layer first. A
// newline separates parts unless the previous one already ends with one.
func JoinParts(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, p...)
	}
	return out
}

// Ref is a named pointer to a revision.
type Ref struct {
	Name      string    `json:"name"`
	Target    Hash      `json:"target"`
	UpdatedBy string    `json:"updatedBy"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ChangeSummary is what work-item and label searches return.
type ChangeSummary struct {
	Repo      string    `json:"repo"`
	Revision  Hash      `json:"revision"`
	ChangeID  string    `json:"changeId"`
	Subject   string    `json:"subject"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"createdAt"`
	// Via says how the link was made: "change" or "annotation".
	Via string `json:"via"`
}

// Summary returns the change's search summary.
func (c Change) Summary() ChangeSummary {
	return ChangeSummary{Repo: c.Repo, Revision: c.Revision, ChangeID: c.ChangeID, Subject: c.Subject(), Author: c.Author, CreatedAt: c.CreatedAt, Via: "change"}
}
