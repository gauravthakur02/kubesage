package ui

import (
	"fmt"
	"strings"
)

// diffLines returns a unified diff (context lines around changes) between a and b.
// It uses a longest-common-subsequence table, fine for manifests of a few thousand lines.
func diffLines(a, b []string, context int) []string {
	n, m := len(a), len(b)
	if n*m > 9_000_000 { // very large: fall back to a coarse whole-file diff
		var out []string
		for _, l := range a {
			out = append(out, "-"+l)
		}
		for _, l := range b {
			out = append(out, "+"+l)
		}
		return out
	}
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
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	// Keep only changes and `context` lines around them.
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for x := max(0, k-context); x <= min(len(ops)-1, k+context); x++ {
				keep[x] = true
			}
		}
	}
	var out []string
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && len(out) > 0 {
			out = append(out, "@@")
		}
		gap = false
		out = append(out, string(o.kind)+o.text)
	}
	return out
}

// diffStats counts added and removed lines.
func diffStats(d []string) (add, del int) {
	for _, l := range d {
		switch {
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return
}

// renderDiff colours a diff for the text view.
func (a *App) renderDiffLines(d []string) []string {
	s := a.st
	out := make([]string, 0, len(d))
	for _, l := range d {
		switch {
		case l == "@@":
			out = append(out, s.Muted.Render("  ⋯"))
		case strings.HasPrefix(l, "+"):
			out = append(out, s.OKText.Render("+ "+l[1:]))
		case strings.HasPrefix(l, "-"):
			out = append(out, s.ErrText.Render("- "+l[1:]))
		default:
			out = append(out, s.Muted.Render("  "+l[1:]))
		}
	}
	if len(out) == 0 {
		out = append(out, s.OKText.Render("no differences"))
	}
	return out
}

func diffHeader(add, del int) string { return fmt.Sprintf("+%d −%d", add, del) }
