package ui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"kubesage/internal/config"
	"kubesage/internal/kube"
)

// viewReadyMsg carries clients for every cluster a saved view needs.
type viewReadyMsg struct {
	name    string
	clients map[string]*kube.Client
	err     error
}

// snapshotView captures a layout node as a saved view.
func snapshotView(n *layoutNode) *config.View {
	if n == nil {
		return nil
	}
	if n.leaf() {
		p := n.p
		ns := p.client.Namespace
		if ns == kube.AllNamespaces {
			ns = "all"
		}
		res := p.res.Name
		switch p.mode {
		case modeAI:
			res = "ai"
		case modePulse:
			res = "pulse"
		}
		return &config.View{Context: p.client.Context, Namespace: ns, Resource: res}
	}
	return &config.View{Stacked: n.stacked, Ratio: n.ratio, A: snapshotView(n.a), B: snapshotView(n.b)}
}

func viewContexts(v *config.View, out map[string]bool) {
	if v == nil {
		return
	}
	if v.A == nil && v.B == nil {
		out[v.Context] = true
		return
	}
	viewContexts(v.A, out)
	viewContexts(v.B, out)
}

// saveView stores the current tab's layout under name in the config file.
func (a *App) saveView(name string) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t") {
		a.setStatus("usage: :view save <name> (one word)", true)
		return
	}
	if a.cfg.Views == nil {
		a.cfg.Views = map[string]config.View{}
	}
	a.cfg.Views[name] = *snapshotView(a.root)
	if err := a.cfg.Save(); err != nil {
		a.setStatus("could not save view: "+err.Error(), true)
		return
	}
	a.setStatus(fmt.Sprintf("view %q saved (%d pane(s)) · b or :view %s opens it", name, len(a.panes()), name), false)
}

func (a *App) viewNames() []string {
	var out []string
	for n := range a.cfg.Views {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// openViewPicker lists saved views (key b).
func (a *App) openViewPicker() tea.Cmd {
	names := a.viewNames()
	if len(names) == 0 {
		a.setStatus("no saved views yet: arrange panes, then :view save <name>", false)
		return nil
	}
	labels := make([]string, len(names))
	for i, n := range names {
		v := a.cfg.Views[n]
		ctxs := map[string]bool{}
		viewContexts(&v, ctxs)
		var cl []string
		for c := range ctxs {
			cl = append(cl, shortCtx(c))
		}
		sort.Strings(cl)
		labels[i] = fmt.Sprintf("%-18s %d pane(s) · %s", n, countLeaves(&v), strings.Join(cl, ", "))
	}
	a.openPicker("Saved views (enter opens in a new tab)", labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return a.openView(names[i])
			}
		}
		return nil
	})
	return nil
}

func countLeaves(v *config.View) int {
	if v == nil {
		return 0
	}
	if v.A == nil && v.B == nil {
		return 1
	}
	return countLeaves(v.A) + countLeaves(v.B)
}

// openView connects to the view's clusters (pooled, so usually instant) and rebuilds it in a new tab.
func (a *App) openView(name string) tea.Cmd {
	v, ok := a.cfg.Views[name]
	if !ok {
		a.setStatus("no saved view "+name, true)
		return nil
	}
	need := map[string]bool{}
	viewContexts(&v, need)
	store, kubeconfig := a.cl, a.client.Kubeconfig
	a.setStatus("opening view "+name+"…", false)
	return func() tea.Msg {
		clients := map[string]*kube.Client{}
		for ctx := range need {
			if c, ok := store.client(ctx); ok {
				clients[ctx] = c
				continue
			}
			c, err := kube.New(kubeconfig, ctx, "")
			if err != nil {
				return viewReadyMsg{name: name, err: fmt.Errorf("%s: %w", ctx, err)}
			}
			store.add(c)
			clients[ctx], _ = store.client(ctx)
		}
		return viewReadyMsg{name: name, clients: clients}
	}
}

func (a *App) buildView(v *config.View, clients map[string]*kube.Client) *layoutNode {
	if v.A == nil && v.B == nil {
		ns := v.Namespace
		if ns == "all" {
			ns = kube.AllNamespaces
		}
		a.nextID++
		p := newPane(a.nextID, clients[v.Context].WithNamespace(ns), kube.Resource{})
		switch v.Resource {
		case "ai":
			p.res, _ = kube.Lookup("pods")
			p.mode, p.prevMode = modeAI, modeTable
		case "pulse":
			p.res, _ = kube.Lookup("pods")
			p.mode = modePulse
		default:
			a.withPane(p, func() { p.res = a.resourceByName(v.Resource) })
		}
		return &layoutNode{p: p}
	}
	n := &layoutNode{stacked: v.Stacked, ratio: v.Ratio, a: a.buildView(v.A, clients), b: a.buildView(v.B, clients)}
	if n.ratio <= 0 || n.ratio >= 1 {
		n.ratio = 0.5
	}
	n.a.parent, n.b.parent = n, n
	return n
}

// resourceByName restores built-in, discovered and kubesage's own tables (for the focused pane).
func (a *App) resourceByName(name string) kube.Resource {
	switch {
	case name == clustersRes:
		return a.clustersResource()
	case name == policyRes:
		return a.policyResource()
	case name == helmRes:
		return a.helmResource()
	case name == chartsRes:
		return a.chartsResource()
	case strings.HasPrefix(name, rolloutPrefix):
		ns, n, _ := strings.Cut(strings.TrimPrefix(name, rolloutPrefix), "/")
		return a.rolloutResource(ns, n)
	}
	if r, ok := a.lookupResource(name); ok {
		return r
	}
	r, _ := kube.Lookup("pods")
	return r
}

func (a *App) handleViewReady(m viewReadyMsg) tea.Cmd {
	if m.err != nil {
		a.setStatus("cannot open view "+m.name+": "+m.err.Error(), true)
		return nil
	}
	v := a.cfg.Views[m.name]
	root := a.buildView(&v, m.clients)
	if len(a.tabs) >= maxTabs {
		a.setStatus(fmt.Sprintf("at most %d tabs", maxTabs), true)
		return nil
	}
	a.saveTab()
	a.tabs = append(a.tabs, &workspace{name: m.name, root: root, focus: root.leaves()[0]})
	a.tab = len(a.tabs) - 1
	a.root, a.zoomed = root, false
	a.focus(root.leaves()[0])
	a.layout()
	var cmds []tea.Cmd
	for _, p := range a.panes() {
		switch p.mode {
		case modeTable:
			cmds = append(cmds, a.refreshPane(p))
		case modePulse:
			p.pulseBusy = true
			cmds = append(cmds, tagPane(p, fetchPulse(p.client)))
		}
	}
	a.syncUsage()
	a.setStatus(fmt.Sprintf("view %q opened in tab %d", m.name, a.tab+1), false)
	return tea.Batch(cmds...)
}

func (a *App) viewCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	switch {
	case len(f) == 0:
		return a.openViewPicker()
	case f[0] == "save" && len(f) == 2:
		a.saveView(f[1])
		return nil
	case (f[0] == "rm" || f[0] == "delete") && len(f) == 2:
		if _, ok := a.cfg.Views[f[1]]; !ok {
			a.setStatus("no saved view "+f[1], true)
			return nil
		}
		delete(a.cfg.Views, f[1])
		if err := a.cfg.Save(); err != nil {
			a.setStatus(err.Error(), true)
		} else {
			a.setStatus("view "+f[1]+" deleted", false)
		}
		return nil
	}
	return a.openView(f[0])
}
