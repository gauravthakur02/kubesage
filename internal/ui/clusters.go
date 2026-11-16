package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// clusterInfo is the last known health of one kube context.
type clusterInfo struct {
	version  string
	nodes    int
	ready    int
	latency  time.Duration
	err      error
	checking bool
	at       time.Time
}

// clusterStore is the connection pool: one base client per context, shared by every
// pane and tab that uses it, plus the last probe result. Safe for concurrent use.
type clusterStore struct {
	mu      sync.Mutex
	clients map[string]*kube.Client
	info    map[string]*clusterInfo
	usage   map[string]int  // panes per context, published by the UI loop
	marked  map[string]bool // contexts ticked in the Clusters view (space)
}

func (s *clusterStore) isMarked(ctx string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.marked[ctx]
}

func (s *clusterStore) toggle(ctx string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.marked == nil {
		s.marked = map[string]bool{}
	}
	s.marked[ctx] = !s.marked[ctx]
}

func (s *clusterStore) takeMarked(order []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range order {
		if s.marked[c] {
			out = append(out, c)
		}
	}
	s.marked = nil
	return out
}

func newClusterStore() *clusterStore {
	return &clusterStore{clients: map[string]*kube.Client{}, info: map[string]*clusterInfo{}}
}

func (s *clusterStore) client(ctx string) (*kube.Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[ctx]
	return c, ok
}

func (s *clusterStore) add(c *kube.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[c.Context]; !ok {
		s.clients[c.Context] = c
	}
}

func (s *clusterStore) get(ctx string) clusterInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i, ok := s.info[ctx]; ok {
		return *i
	}
	return clusterInfo{}
}

func (s *clusterStore) update(ctx string, fn func(*clusterInfo)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.info[ctx]
	if !ok {
		i = &clusterInfo{}
		s.info[ctx] = i
	}
	fn(i)
}

func (s *clusterStore) connected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.clients {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- messages & commands ----------

// connectMsg delivers a connected client for a purpose: "tab" (switch current tab),
// "newtab", "split" (side-by-side pane) or "probe" (health check only).
type connectMsg struct {
	client  *kube.Client
	ctx     string
	ns      string
	purpose string
	err     error
}

type probeMsg struct{ ctx string }

// connect returns a pooled client for ctx, creating (and authenticating) it if needed.
func (a *App) connect(ctxName, ns, purpose string) tea.Cmd {
	store, kubeconfig := a.cl, a.client.Kubeconfig
	return func() tea.Msg {
		if c, ok := store.client(ctxName); ok {
			return connectMsg{client: c, ctx: ctxName, ns: ns, purpose: purpose}
		}
		c, err := kube.New(kubeconfig, ctxName, "")
		if err == nil {
			store.add(c)
			c, _ = store.client(ctxName)
		}
		return connectMsg{client: c, ctx: ctxName, ns: ns, purpose: purpose, err: err}
	}
}

// probe measures API latency, version and node readiness for a connected context.
func (a *App) probe(ctxName string) tea.Cmd {
	store := a.cl
	store.update(ctxName, func(i *clusterInfo) { i.checking = true })
	connect := a.connect(ctxName, "", "probe")
	return func() tea.Msg {
		m := connect().(connectMsg)
		if m.err != nil {
			store.update(ctxName, func(i *clusterInfo) { i.err, i.checking, i.at = m.err, false, time.Now() })
			return probeMsg{ctx: ctxName}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		start := time.Now()
		v, err := m.client.ServerVersion()
		lat := time.Since(start)
		nodes, ready := 0, 0
		if err == nil {
			if list, lerr := m.client.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); lerr == nil {
				for i := range list.Items {
					nodes++
					if kube.NodeReady(&list.Items[i]) {
						ready++
					}
				}
			}
		}
		store.update(ctxName, func(i *clusterInfo) {
			i.version, i.err, i.latency, i.nodes, i.ready, i.checking, i.at = v, err, lat, nodes, ready, false, time.Now()
		})
		return probeMsg{ctx: ctxName}
	}
}

func (a *App) handleConnect(m connectMsg) tea.Cmd {
	if m.err != nil {
		a.cl.update(m.ctx, func(i *clusterInfo) { i.err, i.at = m.err, time.Now() })
		if m.purpose != "probe" {
			a.setStatus("cannot connect to "+m.ctx+": "+m.err.Error(), true)
		}
		return nil
	}
	if m.purpose != "probe" && m.purpose != "bgtab" {
		a.launcher = false
	}
	ns := m.ns
	if ns == "" {
		ns = m.client.Namespace
	}
	if ns == "all" {
		ns = kube.AllNamespaces
	}
	switch m.purpose {
	case "tab": // switch every pane of the current tab to this cluster
		var cmds []tea.Cmd
		for _, p := range a.panes() {
			p.client = m.client.WithNamespace(ns)
			if p.res.Name == clustersRes {
				p.res, _ = kube.Lookup("pods")
			}
			a.withPane(p, func() {
				p.pulse, p.issues = nil, nil
				switch p.mode {
				case modeTable, modeText:
					cmds = append(cmds, a.switchResource(a.res))
				case modePulse:
					p.pulseBusy = true
					cmds = append(cmds, tagPane(p, fetchPulse(p.client)))
				}
			})
		}
		a.setStatus("this tab → cluster "+m.ctx, false)
		return tea.Batch(append(cmds, fetchVersion(m.client))...)
	case "bgtab": // opened from --context a,b,c: keep the current tab in front
		cur := a.tab
		cmd := a.newTabWith(m.client.WithNamespace(ns), a.res)
		return tea.Batch(cmd, a.switchTab(cur), fetchVersion(m.client))
	case "newtab":
		cmd := a.newTabWith(m.client.WithNamespace(ns), a.res)
		a.setStatus(fmt.Sprintf("tab %d → cluster %s", a.tab+1, m.ctx), false)
		return tea.Batch(cmd, fetchVersion(m.client))
	case "split", "split-stacked":
		cmd := a.splitWith(m.purpose == "split-stacked", m.client.WithNamespace(ns), a.res)
		a.setStatus("new pane → cluster "+m.ctx+"   (compare side by side)", false)
		return tea.Batch(cmd, fetchVersion(m.client))
	}
	return nil
}

// usage counts panes per context across all tabs.
func (a *App) usage() map[string]int {
	u := map[string]int{}
	for _, p := range a.allPanes() {
		if p.res.Name == clustersRes && p.mode == modeTable {
			continue // the cluster list itself does not use its client
		}
		u[p.client.Context]++
	}
	return u
}

// multiCluster is true when panes are connected to more than one context.
func (a *App) multiCluster() bool { return len(a.usage()) > 1 }

func (a *App) dangerFor(ctx string) bool {
	return a.dangerRe != nil && a.dangerRe.MatchString(ctx)
}

// shortCtx abbreviates long context names for tab labels and pane titles.
func shortCtx(ctx string) string {
	if len(ctx) <= 26 {
		return ctx
	}
	return ctx[:12] + "…" + ctx[len(ctx)-12:]
}

// ---------- Clusters overview (a virtual resource) ----------

const clustersRes = "contexts"

func (a *App) clustersResource() kube.Resource {
	contexts := a.client.Contexts()
	clusterOf := a.client.ClusterOf
	store := a.cl
	danger := a.dangerFor
	return kube.LocalResource(clustersRes, "Cluster", "Clusters", func() kube.Table {
		u := a.usageSnapshot()
		t := kube.Table{Columns: []string{"CONTEXT", "CLUSTER", "STATUS", "VERSION", "NODES", "API", "OPEN", "CHECKED"}}
		for _, ctx := range contexts {
			i := store.get(ctx)
			connected := u[ctx] > 0
			status, h := "not connected", kube.HealthNeutral
			switch {
			case i.checking:
				status, h = "checking…", kube.HealthNeutral
			case i.err != nil:
				status, h = "✗ "+firstWords(i.err.Error(), 8), kube.HealthErr
			case i.version != "":
				status, h = "✓ reachable", kube.HealthOK
				if i.ready < i.nodes {
					h = kube.HealthWarn
				}
			case connected:
				status, h = "connected", kube.HealthOK
			}
			name := "[ ] " + ctx
			if store.isMarked(ctx) {
				name = "[x] " + ctx
			}
			if danger(ctx) {
				name += "  ⚠ prod"
			}
			nodes, api, checked := "", "", ""
			if i.version != "" {
				nodes = fmt.Sprintf("%d/%d", i.ready, i.nodes)
				api = i.latency.Round(time.Millisecond).String()
			}
			if !i.at.IsZero() {
				checked = kube.Age(metav1.NewTime(i.at)) + " ago"
			}
			open := ""
			if n := u[ctx]; n > 0 {
				open = fmt.Sprintf("%d pane(s)", n)
			}
			t.Rows = append(t.Rows, kube.Row{Name: ctx, Health: h, Cells: []string{name, clusterOf(ctx), status, i.version, nodes, api, open, checked}})
		}
		return t
	})
}

// usageSnapshot is read from list goroutines, so it is guarded by the store lock.
func (a *App) usageSnapshot() map[string]int {
	a.cl.mu.Lock()
	defer a.cl.mu.Unlock()
	out := make(map[string]int, len(a.cl.usage))
	for k, v := range a.cl.usage {
		out[k] = v
	}
	return out
}

// syncUsage publishes pane usage for the Clusters view (called from the UI loop).
func (a *App) syncUsage() {
	u := a.usage()
	a.cl.mu.Lock()
	a.cl.usage = u
	a.cl.mu.Unlock()
}

func firstWords(s string, n int) string {
	w := strings.Fields(s)
	if len(w) > n {
		w = append(w[:n], "…")
	}
	return strings.Join(w, " ")
}

// openClustersPicked opens the chosen contexts: in the startup chooser the first replaces
// the chooser, otherwise each opens in a new tab.
func (a *App) openClustersPicked(picked []string) tea.Cmd {
	var cmds []tea.Cmd
	first := 0
	if a.launcher {
		// The chooser stays up until the first cluster connects, so a failed sign-in
		// leaves the user in the chooser instead of on some other cluster.
		a.setStatus("connecting to "+strings.Join(picked, ", ")+"…", false)
		cmds = append(cmds, a.connect(picked[0], "", "tab"))
		first = 1
	}
	for _, ctx := range picked[first:] {
		purpose := "newtab"
		if len(cmds) > 0 {
			purpose = "bgtab"
		}
		cmds = append(cmds, a.connect(ctx, "", purpose))
	}
	return tea.Batch(cmds...)
}

// openLauncher shows the cluster chooser at startup without contacting any cluster.
func (a *App) openLauncher() tea.Cmd {
	a.launcher = true
	cmd := a.switchResource(a.clustersResource())
	a.setStatus("choose cluster(s): ↑↓ move · space select several · enter open · / filter · q quit", false)
	return cmd
}

// openClusters shows the Clusters overview in the focused pane and probes connected clusters.
func (a *App) openClusters() tea.Cmd {
	a.syncUsage()
	cmds := []tea.Cmd{a.switchResource(a.clustersResource())}
	for _, ctx := range a.cl.connected() {
		cmds = append(cmds, a.probe(ctx))
	}
	a.setStatus("enter: open in new tab · v: side-by-side pane · c: switch this tab · p: health check (may sign in)", false)
	return tea.Batch(cmds...)
}

// clusterRowAction handles keys on the Clusters overview.
// space ticks clusters; enter opens the ticked ones (or the one under the cursor).
// In the startup chooser the first cluster takes over the current tab, the rest get their own tabs.
func (a *App) clusterRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	ctx := row.Name
	switch key {
	case " ", "space", "x":
		a.cl.toggle(ctx)
		if a.cursor < len(a.visible)-1 {
			a.cursor++
		}
		return a.refresh(), true
	case "enter":
		picked := a.cl.takeMarked(a.client.Contexts())
		if len(picked) == 0 {
			picked = []string{ctx}
		}
		return a.openClustersPicked(picked), true
	case "t":
		return a.connect(ctx, "", "newtab"), true
	case "v":
		return a.connect(ctx, "", "split"), true
	case "c":
		return a.connect(ctx, "", "tab"), true
	case "p", "r":
		return a.probe(ctx), true
	}
	return nil, false
}
