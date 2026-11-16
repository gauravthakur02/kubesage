package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// tourPages is the first-run tour (also :tour). Each page is short: a title and a few key lines.
var tourPages = []struct {
	title string
	lines [][2]string
}{
	{"Welcome to kubesage", [][2]string{{"", "A Kubernetes IDE for your terminal. This 60-second tour shows where things are."},
		{"→ / enter", "next page"}, {"←", "previous page"}, {"esc", "skip the tour (reopen it any time with :tour)"}}},
	{"Find anything", [][2]string{{"1-9", "favourite resources (pods, deployments, …)"}, {"0", "every resource type, including CRDs"},
		{":pods kube-system", "jump to a resource in a namespace"}, {"/", "filter the table"}, {"n · c", "switch namespace · switch cluster"}}},
	{"Act on the selected row", [][2]string{{"space", "the actions menu: everything you can do with this row"},
		{"l · s · F", "logs · shell · port-forward"}, {"e", "safe edit: diff and dry-run before anything is applied"},
		{"w · t", "relationships graph · timeline of events, rollouts, restarts"}}},
	{"Ask the AI", [][2]string{{"a", "analyze the selected object"}, {"A", "chat about the cluster"}, {"I", "triage everything unhealthy"},
		{"", "The AI only runs a command after you approve it, and never in read-only mode."}}},
	{"Stay safe", [][2]string{{"--readonly", "start with every change disabled"}, {"danger_contexts", "production-like clusters ask you to type the name"},
		{":audit", "every change kubesage made, with who and why"}}},
	{"Everything else", [][2]string{{"ctrl+p", "command palette: every screen and command, fuzzy search"}, {"?", "all keys"},
		{":fleet · :history", "all clusters at a glance · events older than an hour"}, {"q", "quit"}}},
}

func (a *App) openTour() tea.Cmd {
	a.ov, a.tourPage = ovTour, 0
	return nil
}

func (a *App) updateTour(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "right", "enter", " ", "l", "tab":
		if a.tourPage < len(tourPages)-1 {
			a.tourPage++
			return nil
		}
		return a.finishTour()
	case "left", "h", "shift+tab":
		if a.tourPage > 0 {
			a.tourPage--
		}
		return nil
	case "esc", "q":
		return a.finishTour()
	}
	return nil
}

func (a *App) finishTour() tea.Cmd {
	a.ov = ovNone
	if !a.cfg.TourDone {
		a.cfg.TourDone = true
		_ = a.cfg.Save()
	}
	a.setStatus("tour done · ctrl+p for the palette · ? for every key · :tour to see it again", false)
	return nil
}

func (a *App) viewTour(w, h int) string {
	s := a.st
	p := tourPages[a.tourPage]
	lines := []string{s.Title.Render(p.title), ""}
	for _, l := range p.lines {
		if l[0] == "" {
			lines = append(lines, s.Base.Render(l[1]))
			continue
		}
		lines = append(lines, s.Key.Render(fmt.Sprintf("%-20s", l[0]))+s.Base.Render(l[1]))
	}
	dots := make([]string, len(tourPages))
	for i := range dots {
		dots[i] = "○"
		if i == a.tourPage {
			dots[i] = "●"
		}
	}
	lines = append(lines, "", s.Muted.Render(strings.Join(dots, " ")+fmt.Sprintf("   %d/%d · → next · ← back · esc skip", a.tourPage+1, len(tourPages))))
	return a.center(strings.Join(lines, "\n"), w, h)
}
