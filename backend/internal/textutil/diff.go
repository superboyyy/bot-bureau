package textutil

import (
	"fmt"
	"strings"
)

const diffContext = 3

// Unified is a line-based unified diff of old → new, named for the approval card.
// Identical inputs yield an empty string. The result is truncated at limit characters
// (0 means no cap) so a whole-file rewrite cannot blow up an event.
func Unified(path, old, new string, limit int) string {
	a, b := splitLines(old), splitLines(new)
	if eqLines(a, b) {
		return ""
	}
	ops := diffOps(a, b)
	var bld strings.Builder
	fmt.Fprintf(&bld, "--- a/%s\n+++ b/%s\n", path, path)
	for _, h := range hunks(ops) {
		bld.WriteString(h)
	}
	out := bld.String()
	if limit <= 0 {
		return out
	}
	return Truncate(out, limit)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

func eqLines(a, b []string) bool {
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

type opKind int

const (
	opEq opKind = iota
	opDel
	opAdd
)

type diffOp struct {
	kind opKind
	line string
}

func hunks(ops []diffOp) []string {
	// Collect change indices, then grow each cluster by context, merging those that overlap.
	var changes []int
	for i, o := range ops {
		if o.kind != opEq {
			changes = append(changes, i)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	type span struct{ lo, hi int }
	var spans []span
	lo, hi := changes[0], changes[0]
	for _, c := range changes[1:] {
		if c <= hi+2*diffContext+1 {
			hi = c
			continue
		}
		spans = append(spans, span{lo, hi})
		lo, hi = c, c
	}
	spans = append(spans, span{lo, hi})

	var out []string
	for _, s := range spans {
		start := s.lo - diffContext
		if start < 0 {
			start = 0
		}
		end := s.hi + diffContext + 1
		if end > len(ops) {
			end = len(ops)
		}
		out = append(out, formatHunk(ops, start, end))
	}
	return out
}

func formatHunk(ops []diffOp, start, end int) string {
	oldStart, newStart := 1, 1
	for i := 0; i < start; i++ {
		switch ops[i].kind {
		case opEq, opDel:
			oldStart++
		}
		switch ops[i].kind {
		case opEq, opAdd:
			newStart++
		}
	}
	oldN, newN := 0, 0
	var body strings.Builder
	for _, o := range ops[start:end] {
		switch o.kind {
		case opEq:
			oldN++
			newN++
			body.WriteByte(' ')
		case opDel:
			oldN++
			body.WriteByte('-')
		case opAdd:
			newN++
			body.WriteByte('+')
		}
		body.WriteString(o.line)
		body.WriteByte('\n')
	}
	if oldN == 0 {
		oldStart = 0
	}
	if newN == 0 {
		newStart = 0
	}
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@\n%s", oldStart, oldN, newStart, newN, body.String())
}

// diffOps is an LCS walk. Inputs whose product exceeds the cap fall back to a common
// prefix/suffix plus a single middle replace, so a 10 000-line rewrite cannot allocate
// a hundred-million-cell table.
func diffOps(a, b []string) []diffOp {
	const capProduct = 250000
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	if int64(len(a))*int64(len(b)) > capProduct {
		return coarseOps(a, b)
	}
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			ops = append(ops, diffOp{opEq, a[i]})
			i++
			j++
			continue
		}
		if dp[i+1][j] >= dp[i][j+1] {
			ops = append(ops, diffOp{opDel, a[i]})
			i++
			continue
		}
		ops = append(ops, diffOp{opAdd, b[j]})
		j++
	}
	for i < n {
		ops = append(ops, diffOp{opDel, a[i]})
		i++
	}
	for j < m {
		ops = append(ops, diffOp{opAdd, b[j]})
		j++
	}
	return ops
}

func coarseOps(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	asuf, bsuf := len(a), len(b)
	for asuf > pre && bsuf > pre && a[asuf-1] == b[bsuf-1] {
		asuf--
		bsuf--
	}
	var ops []diffOp
	for i := 0; i < pre; i++ {
		ops = append(ops, diffOp{opEq, a[i]})
	}
	for i := pre; i < asuf; i++ {
		ops = append(ops, diffOp{opDel, a[i]})
	}
	for i := pre; i < bsuf; i++ {
		ops = append(ops, diffOp{opAdd, b[i]})
	}
	for i := asuf; i < len(a); i++ {
		ops = append(ops, diffOp{opEq, a[i]})
	}
	return ops
}
