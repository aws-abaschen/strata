package merge

import (
	"strings"
	"testing"
)

func TestMerge3(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	cases := []struct {
		name, ours, theirs, want string
		conflicts                int
	}{
		{"only ours", "a\nB\nc\nd\ne\n", base, "a\nB\nc\nd\ne\n", 0},
		{"only theirs", base, "a\nb\nc\nD\ne\n", "a\nb\nc\nD\ne\n", 0},
		{"both, apart", "A\nb\nc\nd\ne\n", "a\nb\nc\nd\nE\n", "A\nb\nc\nd\nE\n", 0},
		{"same edit", "a\nX\nc\nd\ne\n", "a\nX\nc\nd\ne\n", "a\nX\nc\nd\ne\n", 0},
		{"insert and delete", "a\nb\nnew\nc\nd\ne\n", "a\nb\nc\ne\n", "a\nb\nnew\nc\ne\n", 0},
		{"append both ends", "top\n" + base, base + "bottom\n", "top\n" + base + "bottom\n", 0},
		{"clash", "a\nours\nc\nd\ne\n", "a\ntheirs\nc\nd\ne\n",
			"a\n<<<<<<< yours\nours\n=======\ntheirs\n>>>>>>> server abc\nc\nd\ne\n", 1},
	}
	for _, c := range cases {
		got, n, ok := Merge3([]byte(base), []byte(c.ours), []byte(c.theirs), "abc")
		if !ok || string(got) != c.want || n != c.conflicts {
			t.Errorf("%s: got %q (%d conflicts), want %q (%d)", c.name, got, n, c.want, c.conflicts)
		}
	}
}

func TestMerge3NoTrailingNewline(t *testing.T) {
	got, n, _ := Merge3([]byte("x"), []byte("ours"), []byte("theirs"), "s")
	if n != 1 || !HasConflictMarkers(got) || !strings.HasSuffix(string(got), ">>>>>>> server s\n") {
		t.Fatalf("%q", got)
	}
}
