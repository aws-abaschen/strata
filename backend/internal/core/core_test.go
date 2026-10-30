package core

import (
	"errors"
	"testing"
	"time"
)

var h1 = HashBytes([]byte("one"))
var h2 = HashBytes([]byte("two"))

func TestTreeEncodeIsCanonical(t *testing.T) {
	a := Tree{Entries: []TreeEntry{{Name: "b", Kind: KindFile, Hash: h1, Size: 3}, {Name: "a", Kind: KindDir, Hash: h2, Size: 9}}}
	b := Tree{Entries: []TreeEntry{{Name: "a", Kind: KindDir, Hash: h2}, {Name: "b", Kind: KindFile, Hash: h1, Size: 3}}}
	ab, ah, err := a.Encode()
	if err != nil {
		t.Fatal(err)
	}
	_, bh, _ := b.Encode()
	if ah != bh {
		t.Fatalf("same content, different hashes: %s %s", ah, bh)
	}
	if _, err := DecodeTree(ab, ah); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTree([]byte(`{"entries":[{"name":"b","kind":"file","hash":"`+string(h1)+`","size":3},{"name":"a","kind":"dir","hash":"`+string(h2)+`"}]}`), ah); err == nil {
		t.Fatal("non-canonical tree bytes must be rejected")
	}
}

func TestTreeRejectsBadNames(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", "a\\b"} {
		_, _, err := Tree{Entries: []TreeEntry{{Name: n, Kind: KindFile, Hash: h1}}}.Encode()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("name %q: want invalid, got %v", n, err)
		}
	}
	_, _, err := Tree{Entries: []TreeEntry{{Name: "a", Kind: KindFile, Hash: h1}, {Name: "a", Kind: KindFile, Hash: h2}}}.Encode()
	if err == nil {
		t.Error("duplicate names must be rejected")
	}
}

func TestSealIsDeterministic(t *testing.T) {
	mk := func() Change {
		return Change{
			ChangeID: NewChangeID(), Repo: "payments", Tree: h1, Author: "ana@example.com",
			CreatedAt: time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC), Message: "PAY-42 fix rounding",
			Metadata: Metadata{Labels: []string{"Risk:Low", "risk:low"}, Attributes: map[string]string{"approver": "bob"}},
		}
	}
	a := mk()
	b := a
	if _, err := a.Seal(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Seal(); err != nil {
		t.Fatal(err)
	}
	if a.Revision != b.Revision || !a.Revision.Valid() {
		t.Fatalf("revisions differ: %s %s", a.Revision, b.Revision)
	}
	if len(a.Metadata.Labels) != 1 || a.Metadata.Labels[0] != "risk:low" {
		t.Fatalf("labels not normalized: %v", a.Metadata.Labels)
	}
	c := mk()
	c.ChangeID = a.ChangeID
	c.Message = "PAY-43 other"
	c.Seal()
	if c.Revision == a.Revision {
		t.Fatal("different content must give a different revision")
	}
}

func TestParseWorkItems(t *testing.T) {
	got := ParseWorkItems("PAY-42: fix rounding\n\nAlso closes OPS-7 and SEC-1001; not utf-8 or A-1")
	want := []string{"PAY-42", "OPS-7", "SEC-1001"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, w := range want {
		if got[i].Key != w || got[i].System != "jira" {
			t.Errorf("item %d: got %v want %s", i, got[i], w)
		}
	}
}

func TestParseWorkItem(t *testing.T) {
	w, err := ParseWorkItem("servicenow:CHG0031337")
	if err != nil || w.System != "servicenow" || w.Key != "CHG0031337" {
		t.Fatalf("got %v %v", w, err)
	}
	w, err = ParseWorkItem("PAY-1")
	if err != nil || w.System != "jira" {
		t.Fatalf("got %v %v", w, err)
	}
	if _, err := ParseWorkItem("jira:bad key"); err == nil {
		t.Fatal("want error")
	}
}

func TestMetadataLimits(t *testing.T) {
	m := Metadata{}
	for i := range MaxLabels + 1 {
		m.Labels = append(m.Labels, "l"+string(rune('a'+i)))
	}
	if err := m.Normalize(); err == nil {
		t.Fatal("too many labels must be rejected")
	}
}

func TestChangeID(t *testing.T) {
	id := NewChangeID()
	if !ValidChangeID(id) || HashBytes(nil).Valid() == ValidChangeID(string(HashBytes(nil))) {
		t.Fatalf("bad id %q", id)
	}
}
