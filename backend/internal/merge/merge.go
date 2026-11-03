package cli

import (
	"bytes"
	"strings"
)

// Conflict markers written into text files.
const (
	markerOurs   = "<<<<<<< yours\n"
	markerSplit  = "=======\n"
	markerTheirs = ">>>>>>> server"
)

// maxMergeCells caps the line-diff table (lines(base) x lines(side)).
const maxMergeCells = 4_000_000

// isText reports whether b looks like text we can merge line by line.
func isText(b []byte) bool { return bytes.IndexByte(b, 0) < 0 }

// splitLines splits after each newline, so joining gives the input back.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// matches returns, for each line of a, the index of the line of b it is
// matched with in a longest common subsequence, or -1.
func matches(a, b []string) ([]int, bool) {
	n, m := len(a), len(b)
	if n*m > maxMergeCells {
		return nil, false
	}
	// lcs[i][j] = LCS length of a[i:], b[j:]
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	out := make([]int, n)
	for i := range out {
		out[i] = -1
	}
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i] == b[j]:
			out[i] = j
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out, true
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Merge3 merges two edits of the same base, line by line (diff3). Regions
// changed on one side only are taken from that side; regions changed the
// same way on both sides are taken once; others become a conflict block.
// ok is false when the inputs are too large to merge.
func Merge3(base, ours, theirs []byte, theirsLabel string) (merged []byte, conflicts int, ok bool) {
	b, o, t := splitLines(base), splitLines(ours), splitLines(theirs)
	mo, ok1 := matches(b, o)
	mt, ok2 := matches(b, t)
	if !ok1 || !ok2 {
		return nil, 0, false
	}
	var out bytes.Buffer
	region := func(bb, oo, tt []string) {
		switch {
		case equal(oo, bb):
			out.WriteString(strings.Join(tt, ""))
		case equal(tt, bb), equal(oo, tt):
			out.WriteString(strings.Join(oo, ""))
		default:
			conflicts++
			out.WriteString(markerOurs)
			writeBlock(&out, oo)
			out.WriteString(markerSplit)
			writeBlock(&out, tt)
			out.WriteString(markerTheirs + " " + theirsLabel + "\n")
		}
	}
	bi, oi, ti := 0, 0, 0
	for i := 0; i <= len(b); i++ {
		if i < len(b) && (mo[i] < 0 || mt[i] < 0) {
			continue
		}
		oe, te := len(o), len(t)
		if i < len(b) {
			oe, te = mo[i], mt[i]
		}
		region(b[bi:i], o[oi:oe], t[ti:te])
		if i < len(b) {
			out.WriteString(b[i])
			bi, oi, ti = i+1, oe+1, te+1
		}
	}
	return out.Bytes(), conflicts, true
}

// writeBlock writes lines, making sure the block ends with a newline so the
// next marker starts on its own line.
func writeBlock(w *bytes.Buffer, lines []string) {
	s := strings.Join(lines, "")
	w.WriteString(s)
	if s != "" && !strings.HasSuffix(s, "\n") {
		w.WriteByte('\n')
	}
}

// HasConflictMarkers reports whether a file still holds unresolved markers.
func HasConflictMarkers(b []byte) bool {
	return bytes.Contains(b, []byte(markerOurs)) && bytes.Contains(b, []byte(markerTheirs))
}
