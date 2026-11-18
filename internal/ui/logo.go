package ui

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// The terminal version of assets/icon.svg: a helm wheel (Kubernetes) with a sage leaf
// in the hub (the copilot) and an AI sparkle, beside the two-tone "kube"+"weave" wordmark.
var logoWheel = []string{
	" ●   ●   ● ",
	"  ╲  │  ╱  ",
	" ●──(❦)──● ",
	"  ╱  │  ╲  ",
	" ●   ●   ● ",
}

// spokeAt maps a wheel cell (line, column) to its spoke, numbered clockwise from the top.
// The animation lights the spokes in turn, so the wheel appears to rotate.
var spokeAt = map[[2]int]int{
	{0, 5}: 0, {1, 5}: 0, // top
	{0, 9}: 1, {1, 8}: 1, // top-right
	{2, 9}: 2, {2, 7}: 2, {2, 8}: 2, // right
	{4, 9}: 3, {3, 8}: 3, // bottom-right
	{4, 5}: 4, {3, 5}: 4, // bottom
	{4, 1}: 5, {3, 2}: 5, // bottom-left
	{2, 1}: 6, {2, 2}: 6, {2, 3}: 6, // left
	{0, 1}: 7, {1, 2}: 7, // top-left
}

var sparkleFrames = []string{"✦", "✧", "⋆", "·", "⋆", "✧"}

// Logo renders the static logo (5 lines) with an optional extra line under the tagline.
func Logo(s Styles, extra string) []string { return LogoFrame(s, extra, -1) }

// LogoFrame renders animation frame n (n < 0 = static): one spoke is lit with a fading
// trail turning clockwise, the leaf breathes and the sparkle twinkles.
func LogoFrame(s Styles, extra string, n int) []string {
	t := s.T
	grad := []lipgloss.Color{t.Accent, t.Accent, t.Accent2, t.Accent2, t.Accent2}
	lit := lipgloss.NewStyle().Foreground(t.Info).Bold(true)
	trail := lipgloss.NewStyle().Foreground(t.Fg)
	leafColor := t.OK
	sparkle := "✦"
	if n >= 0 {
		if (n/6)%2 == 1 {
			leafColor = t.Info // breathe between green and teal
		}
		sparkle = sparkleFrames[(n/2)%len(sparkleFrames)]
	}
	leaf := lipgloss.NewStyle().Foreground(leafColor).Bold(true)
	right := []string{
		lipgloss.NewStyle().Foreground(t.Accent2).Render(sparkle),
		lipgloss.NewStyle().Foreground(t.Fg).Bold(true).Render("kube") + lipgloss.NewStyle().Foreground(t.OK).Bold(true).Render("weave"),
		s.Muted.Render("Kubernetes IDE for your terminal · AI copilot"),
		s.Muted.Render(extra),
		"",
	}
	head := -1
	if n >= 0 {
		head = n % 8
	}
	out := make([]string, len(logoWheel))
	for i, l := range logoWheel {
		base := lipgloss.NewStyle().Foreground(grad[i])
		var b strings.Builder
		for col, r := range []rune(l) {
			switch spoke, ok := spokeAt[[2]int{i, col}]; {
			case r == '❦':
				b.WriteString(leaf.Render(string(r)))
			case ok && spoke == head:
				b.WriteString(lit.Render(string(r)))
			case ok && head >= 0 && spoke == (head+7)%8:
				b.WriteString(trail.Render(string(r)))
			default:
				b.WriteString(base.Render(string(r)))
			}
		}
		gap := "   "
		if i == 0 {
			gap = "  "
		}
		out[i] = b.String() + gap + right[i]
	}
	return out
}

// LogoMark is the compact two-tone "⎈ kubeweave" used in the header pill.
func LogoMark(s Styles) string {
	t := s.T
	return lipgloss.NewStyle().Background(t.Accent).Foreground(t.Bg).Bold(true).Padding(0, 0, 0, 1).Render("⎈ kube") +
		lipgloss.NewStyle().Background(t.OK).Foreground(t.Bg).Bold(true).Padding(0, 1, 0, 0).Render("weave")
}

// PrintableLogo is the static logo for the command line.
func PrintableLogo(extra string) string {
	return strings.Join(Logo(newStyles(themeByName("tokyonight")), extra), "\n")
}

// AnimateLogo plays the wheel turning on a real terminal for about d, then leaves the
// final frame on screen. It redraws in place with cursor-up, so it works in any ANSI terminal.
func AnimateLogo(w io.Writer, extra string, d time.Duration) {
	st := newStyles(themeByName("tokyonight"))
	frames := int(d / (70 * time.Millisecond))
	fmt.Fprint(w, "\x1b[?25l") // hide cursor
	defer fmt.Fprint(w, "\x1b[?25h")
	for n := 0; n <= frames; n++ {
		frame := n
		if n == frames {
			frame = -1 // settle on the static logo
		}
		lines := LogoFrame(st, extra, frame)
		if n > 0 {
			fmt.Fprintf(w, "\x1b[%dA", len(lines))
		}
		for _, l := range lines {
			fmt.Fprint(w, "\r\x1b[2K"+l+"\n")
		}
		if n < frames {
			time.Sleep(70 * time.Millisecond)
		}
	}
}
