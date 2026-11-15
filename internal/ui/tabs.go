package ui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"kubesage/internal/kube"
)

// workspace is one tab: its own split layout, focused pane and zoom state.
// The live tab's state is held in App.root/App.pane/App.zoomed and saved back on switch.
type workspace struct {
	name   string // user-given name; empty = derived from the focused pane
	root   *layoutNode
	focus  *pane
	zoomed bool
}

const maxTabs = 9

func (a *App) saveTab() {
	w := a.tabs[a.tab]
	w.root, w.focus, w.zoomed = a.root, a.pane, a.zoomed
}

// allPanes returns panes of every tab (background tabs keep streaming logs).
func (a *App) allPanes() []*pane {
	var out []*pane
	for i, w := range a.tabs {
		if i == a.tab {
			out = append(out, a.root.leaves()...)
		} else {
			out = append(out, w.root.leaves()...)
		}
	}
	return out
}

// switchTab activates tab i and refreshes its tables.
func (a *App) switchTab(i int) tea.Cmd {
	if i < 0 || i >= len(a.tabs) || i == a.tab {
		return nil
	}
	a.saveTab()
	a.tab = i
	w := a.tabs[i]
	a.root, a.zoomed = w.root, w.zoomed
	a.pane = w.focus
	a.focus(w.focus)
	a.layout()
	var cmds []tea.Cmd
	for _, p := range a.panes() {
		if p.mode == modeTable {
			cmds = append(cmds, a.refreshPane(p))
		}
	}
	return tea.Batch(cmds...)
}

// newTab opens a tab. arg may be a namespace ("all" allowed) or a resource name.
func (a *App) newTab(arg string) tea.Cmd {
	if len(a.tabs) >= maxTabs {
		a.setStatus(fmt.Sprintf("at most %d tabs", maxTabs), true)
		return nil
	}
	res, ns := a.res, a.client.Namespace
	// :tabnew @<context> [namespace] opens the tab on another cluster.
	if f := strings.Fields(arg); len(f) > 0 && strings.HasPrefix(f[0], "@") {
		tns := ""
		if len(f) > 1 {
			tns = f[1]
		}
		return a.connect(strings.TrimPrefix(f[0], "@"), tns, "newtab")
	}
	if arg != "" {
		if r, ok := kube.Lookup(arg); ok {
			res = r
		} else if arg == "all" {
			ns = kube.AllNamespaces
		} else {
			ns = arg
		}
	}
	return a.newTabWith(a.client.WithNamespace(ns), res)
}

// newTabWith opens a tab whose first pane uses client c (any cluster).
func (a *App) newTabWith(c *kube.Client, res kube.Resource) tea.Cmd {
	if len(a.tabs) >= maxTabs {
		a.setStatus(fmt.Sprintf("at most %d tabs", maxTabs), true)
		return nil
	}
	if res.Name == clustersRes {
		res, _ = kube.Lookup("pods")
	}
	a.saveTab()
	a.nextID++
	p := newPane(a.nextID, c, res)
	a.tabs = append(a.tabs, &workspace{root: &layoutNode{p: p}, focus: p})
	a.tab = len(a.tabs) - 1
	a.root, a.zoomed = a.tabs[a.tab].root, false
	a.focus(p)
	a.layout()
	a.setStatus(fmt.Sprintf("tab %d opened   (> / < switch · :tabclose · :tabname)", a.tab+1), false)
	return a.refresh()
}

// closeTab closes the current tab and stops its log streams.
func (a *App) closeTab() tea.Cmd {
	if len(a.tabs) < 2 {
		a.setStatus("this is the only tab", true)
		return nil
	}
	for _, p := range a.root.leaves() {
		a.withPane(p, a.stopLogs)
	}
	closed := a.tab
	a.tabs = append(a.tabs[:closed], a.tabs[closed+1:]...)
	next := min(closed, len(a.tabs)-1)
	w := a.tabs[next]
	a.tab = next
	a.root, a.zoomed = w.root, w.zoomed
	a.focus(w.focus)
	a.layout()
	a.setStatus(fmt.Sprintf("closed tab %d", closed+1), false)
	var cmds []tea.Cmd
	for _, p := range a.panes() {
		if p.mode == modeTable {
			cmds = append(cmds, a.refreshPane(p))
		}
	}
	return tea.Batch(cmds...)
}

// tabLabel names a tab after its focused pane unless the user named it.
func (a *App) tabLabel(i int) string {
	w := a.tabs[i]
	if w.name != "" {
		return w.name
	}
	p := w.focus
	if i == a.tab {
		p = a.pane
	}
	label := p.client.NamespaceLabel()
	if label == "all" {
		label = "all ns"
	}
	if len(label) > 16 {
		label = label[:15] + "…"
	}
	label += "·" + paneSummary(p)
	if a.multiCluster() {
		root := w.root
		if i == a.tab {
			root = a.root
		}
		ctxs := map[string]bool{}
		danger := false
		for _, q := range root.leaves() {
			ctxs[q.client.Context] = true
			danger = danger || a.dangerFor(q.client.Context)
		}
		if len(ctxs) > 1 {
			label = fmt.Sprintf("⎈×%d clusters", len(ctxs))
		} else {
			label = shortCtx(p.client.Context) + " " + label
		}
		if danger {
			label = "⚠ " + label
		}
	}
	return label
}

// tabCommand handles :tabnew, :tabn, :tabp, :tabclose, :tabname, :tab N.
func (a *App) tabCommand(cmd, arg string) (tea.Cmd, bool) {
	switch cmd {
	case "tabnew", "tabe", "tab+":
		return a.newTab(arg), true
	case "tabn", "tabnext":
		return a.switchTab((a.tab + 1) % len(a.tabs)), true
	case "tabp", "tabprev":
		return a.switchTab((a.tab - 1 + len(a.tabs)) % len(a.tabs)), true
	case "tabclose", "tabc":
		return a.closeTab(), true
	case "tabname", "tabrename":
		a.tabs[a.tab].name = strings.TrimSpace(arg)
		return nil, true
	case "tab":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 1 || n > len(a.tabs) {
			a.setStatus(fmt.Sprintf("usage: :tab 1-%d", len(a.tabs)), true)
			return nil, true
		}
		return a.switchTab(n - 1), true
	}
	return nil, false
}
