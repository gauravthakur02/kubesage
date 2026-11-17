package ui

import (
	"github.com/gauravthakur02/kubesage/internal/diff"

	"fmt"
	"strings"
)

// diffLines returns a unified diff (context lines around changes) between a and b.
// It uses a longest-common-subsequence table, fine for manifests of a few thousand lines.
func diffLines(a, b []string, context int) []string { return diff.Lines(a, b, context) }

// diffStats counts added and removed lines.
func diffStats(d []string) (add, del int) { return diff.Stats(d) }

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
