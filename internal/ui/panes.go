package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"

	"kubesage/internal/kube"
)

// pane holds everything one view needs. App embeds the focused pane, so the
// rest of the UI code works on "the current pane" through promoted fields.
type pane struct {
	id       int
	client   *kube.Client // per-pane namespace; context is shared by all panes
	mode     mode
	prevMode mode

	// resource table
	res          kube.Resource
	table        kube.Table
	visible      []int
	cursor       int
	offset       int
	filter       string
	loading      bool
	listErr      error
	listGen      int
	lastList     time.Time // when the last list was requested
	watchPending bool      // a debounced watch refresh is scheduled

	// text / logs view
	vp         viewport.Model
	textTitle  string
	textKind   string
	textLines  []string
	follow     bool
	logCh      chan string
	logCancel  context.CancelFunc
	logGen     int
	target     targetRef
	reveal     bool
	revealedAt time.Time
	secret     *corev1.Secret // decoded secret being viewed (dropped when leaving the view)
	nowrap     bool           // long lines are wrapped by default; w toggles

	// search in the text/log view
	search      string
	matches     []int // logical line indices that match
	matchIdx    int
	visualStart []int // first visual (wrapped) line of each logical line

	// what the log view is streaming (so it can be restarted with another window)
	logSpec logSpec

	// backTo is where esc returns from a drill-down table (e.g. rollout history → deployments)
	backTo *kube.Resource

	// edit is an open safe-edit session (editor → diff + dry-run → apply)
	edit *editState

	// drain is the previewed drain plan shown in this pane
	drain *kube.DrainPlan

	// helmRel is the Helm release shown in the values/manifest view
	helmRel *kube.HelmRelease

	// pulse / issues (per pane, so two clusters can be compared)
	pulse       *kube.Pulse
	issues      []kube.Issue
	issueCursor int
	pulseErr    error
	pulseBusy   bool

	pw, ph int // inner (drawable) size, set by layout
}

func newPane(id int, c *kube.Client, r kube.Resource) *pane {
	return &pane{id: id, client: c, res: r, follow: true, vp: viewport.New(80, 20)}
}

// layoutNode is a binary split tree. Leaves hold panes.
type layoutNode struct {
	p        *pane
	stacked  bool    // children above/below each other (else side by side)
	ratio    float64 // share of the first child
	a, b     *layoutNode
	parent   *layoutNode
	ow, oh   int // outer size assigned by layout
	ox, oy   int
	isZoomed bool
}

func (n *layoutNode) leaf() bool { return n.p != nil }

// leaves returns panes in reading order (left→right, top→bottom).
func (n *layoutNode) leaves() []*pane {
	if n == nil {
		return nil
	}
	if n.leaf() {
		return []*pane{n.p}
	}
	return append(n.a.leaves(), n.b.leaves()...)
}

func (n *layoutNode) find(p *pane) *layoutNode {
	if n == nil {
		return nil
	}
	if n.p == p {
		return n
	}
	if f := n.a.find(p); f != nil {
		return f
	}
	return n.b.find(p)
}

func (n *layoutNode) assign(w, h int) {
	n.ow, n.oh = w, h
	if n.leaf() {
		n.p.pw, n.p.ph = max(10, w-2), max(3, h-2)
		return
	}
	if n.stacked {
		ha := clampSplit(int(float64(h)*n.ratio), h, 5)
		n.a.assign(w, ha)
		n.b.assign(w, h-ha)
		return
	}
	wa := clampSplit(int(float64(w)*n.ratio), w, 16)
	n.a.assign(wa, h)
	n.b.assign(w-wa, h)
}

func clampSplit(v, total, minSize int) int {
	if total < 2*minSize {
		return total / 2
	}
	return max(minSize, min(total-minSize, v))
}

// ---------- pane management on App ----------

func (a *App) panes() []*pane { return a.root.leaves() }

// hasPane reports whether p exists in any tab (results for background tabs still land).
func (a *App) hasPane(p *pane) bool {
	for _, q := range a.allPanes() {
		if q == p {
			return true
		}
	}
	return false
}

func (a *App) paneIndex(p *pane) int {
	for i, q := range a.panes() {
		if q == p {
			return i
		}
	}
	return -1
}

func (a *App) findPaneInMode(m mode) *pane {
	for _, p := range a.panes() {
		if p.mode == m {
			return p
		}
	}
	return nil
}

func (a *App) focus(p *pane) {
	if p == nil || a.root.find(p) == nil {
		return
	}
	a.pane = p
	if p.mode == modeAI {
		a.aiInput.Focus()
		a.sizeChat()
	} else {
		a.aiInput.Blur()
	}
}

func (a *App) focusNext(delta int) {
	ps := a.panes()
	if len(ps) < 2 {
		return
	}
	i := (a.paneIndex(a.pane) + delta + len(ps)) % len(ps)
	a.focus(ps[i])
}

// split adds a pane next to the focused one showing r (or a copy of the current resource).
func (a *App) split(stacked bool, r *kube.Resource) tea.Cmd {
	if len(a.panes()) >= 6 {
		a.setStatus("at most 6 panes", true)
		return nil
	}
	res := a.res
	if r != nil {
		res = *r
	}
	return a.splitWith(stacked, a.client.WithNamespace(a.client.Namespace), res)
}

// splitWith adds a pane with its own client (possibly another cluster) next to the focused one.
func (a *App) splitWith(stacked bool, c *kube.Client, res kube.Resource) tea.Cmd {
	if len(a.panes()) >= 6 {
		a.setStatus("at most 6 panes", true)
		return nil
	}
	if res.Name == clustersRes {
		res, _ = kube.Lookup("pods")
	}
	a.nextID++
	np := newPane(a.nextID, c, res)
	n := a.root.find(a.pane)
	old := &layoutNode{p: n.p, parent: n}
	nw := &layoutNode{p: np, parent: n}
	n.p, n.a, n.b, n.stacked, n.ratio = nil, old, nw, stacked, 0.5
	a.zoomed = false
	a.focus(np)
	a.layout()
	return a.refresh()
}

// closePane removes the focused pane; its sibling takes the space.
func (a *App) closePane() {
	if len(a.panes()) < 2 {
		a.setStatus("this is the only pane", true)
		return
	}
	n := a.root.find(a.pane)
	a.stopLogs()
	parent := n.parent
	sib := parent.a
	if sib == n {
		sib = parent.b
	}
	// Replace parent with sibling (copy fields so references to parent stay valid).
	parent.p, parent.a, parent.b, parent.stacked, parent.ratio = sib.p, sib.a, sib.b, sib.stacked, sib.ratio
	if parent.a != nil {
		parent.a.parent, parent.b.parent = parent, parent
	}
	a.zoomed = false
	a.focus(parent.leaves()[0])
	a.layout()
}

// only keeps the focused pane.
func (a *App) onlyPane() {
	for _, p := range a.panes() {
		if p != a.pane {
			prev := a.pane
			a.pane = p
			a.stopLogs()
			a.pane = prev
		}
	}
	a.root = &layoutNode{p: a.pane}
	a.zoomed = false
	a.layout()
}

// resize grows (+) or shrinks (-) the focused pane within its parent split.
func (a *App) resize(delta float64) {
	n := a.root.find(a.pane)
	if n == nil || n.parent == nil {
		return
	}
	if n.parent.b == n {
		delta = -delta
	}
	n.parent.ratio = max(0.15, min(0.85, n.parent.ratio+delta))
	a.layout()
}

// applyLayout builds a preset arrangement in the focused pane's namespace.
func (a *App) applyLayout(name string) tea.Cmd {
	ns := a.client.Namespace
	mk := func(resName string, m mode) *pane {
		r, _ := kube.Lookup(resName)
		if resName == "" {
			r = a.res
		}
		a.nextID++
		p := newPane(a.nextID, a.client.WithNamespace(ns), r)
		p.mode = m
		return p
	}
	leaf := func(p *pane) *layoutNode { return &layoutNode{p: p} }
	join := func(stacked bool, ratio float64, x, y *layoutNode) *layoutNode {
		n := &layoutNode{stacked: stacked, ratio: ratio, a: x, b: y}
		x.parent, y.parent = n, n
		return n
	}
	var root *layoutNode
	switch name {
	case "triage": // pods | events / AI
		root = join(false, 0.55, leaf(mk("pods", modeTable)), join(true, 0.45, leaf(mk("events", modeTable)), leaf(mk("pods", modeAI))))
	case "ops": // deployments / pods | events
		root = join(false, 0.6, join(true, 0.45, leaf(mk("deployments", modeTable)), leaf(mk("pods", modeTable))), leaf(mk("events", modeTable)))
	case "dash": // pulse | events
		root = join(false, 0.5, leaf(mk("pods", modePulse)), leaf(mk("events", modeTable)))
	case "ai": // current resource | AI
		root = join(false, 0.5, leaf(mk("", modeTable)), leaf(mk("pods", modeAI)))
	case "single", "default":
		a.onlyPane()
		return nil
	default:
		a.setStatus("layouts: triage, ops, dash, ai, single", true)
		return nil
	}
	for _, p := range a.panes() {
		prev := a.pane
		a.pane = p
		a.stopLogs()
		a.pane = prev
	}
	a.root = root
	a.zoomed = false
	ps := a.panes()
	a.focus(ps[0])
	a.layout()
	var cmds []tea.Cmd
	needPulse := false
	for _, p := range ps {
		switch p.mode {
		case modeTable:
			cmds = append(cmds, a.refreshPane(p))
		case modePulse:
			needPulse = true
		case modeAI:
			p.prevMode = modeTable
		}
	}
	if needPulse {
		for _, p := range ps {
			if p.mode == modePulse {
				p.pulseBusy = true
				cmds = append(cmds, tagPane(p, fetchPulse(p.client)))
			}
		}
	}
	a.setStatus("layout → "+name+"   (ctrl+w next pane · z zoom · ctrl+x close)", false)
	return tea.Batch(cmds...)
}

func (a *App) refreshPane(p *pane) tea.Cmd {
	prev := a.pane
	a.pane = p
	cmd := a.refresh()
	a.pane = prev
	return cmd
}

// withPane runs fn with p temporarily focused (for routing async results and rendering).
func (a *App) withPane(p *pane, fn func()) {
	prev := a.pane
	a.pane = p
	fn()
	if a.pane == p { // fn did not move focus on purpose
		a.pane = prev
	}
}

// ---------- message routing ----------

// paneMsg is implemented by results that belong to a specific pane.
type paneMsg interface{ targetPane() *pane }

func (m listMsg) targetPane() *pane      { return m.pane }
func (m textMsg) targetPane() *pane      { return m.pane }
func (m logLinesMsg) targetPane() *pane  { return m.pane }
func (m cmdResultMsg) targetPane() *pane { return m.pane }
func (m pulseMsg) targetPane() *pane     { return m.pane }

// tagPane stamps pane-scoped results produced by cmd with p.
func tagPane(p *pane, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		switch m := cmd().(type) {
		case listMsg:
			m.pane = p
			return m
		case textMsg:
			m.pane = p
			return m
		case logLinesMsg:
			m.pane = p
			return m
		case cmdResultMsg:
			m.pane = p
			return m
		case pulseMsg:
			m.pane = p
			return m
		case healthMsg:
			m.pane = p
			return m
		case secretMsg:
			m.pane = p
			return m
		case nodeHealthMsg:
			m.pane = p
			return m
		case drainPlanMsg:
			m.pane = p
			return m
		case drainDoneMsg:
			m.pane = p
			return m
		case helmTextMsg:
			m.pane = p
			return m
		default:
			return m
		}
	}
}

// ---------- rendering ----------

func (a *App) sizeChat() {
	if a.aiVP.Width != a.pw || a.aiVP.Height != max(3, a.ph-2) {
		a.aiVP.Width, a.aiVP.Height = a.pw, max(3, a.ph-2)
		a.aiInput.Width = a.pw - 4
		a.renderChat()
		a.aiVP.GotoBottom()
	}
}

func (a *App) renderPane(p *pane, focused bool) string {
	var out string
	a.withPane(p, func() {
		a.renderFocused = focused
		title, body := a.paneContent()
		multi := len(a.panes()) > 1
		if multi {
			title = fmt.Sprintf("%d ", a.paneIndex(p)+1) + title
		}
		if a.multiCluster() {
			badge := "⎈ " + shortCtx(a.client.Context)
			if a.dangerFor(a.client.Context) {
				badge = "⚠ " + badge
			}
			title = badge + " · " + title
		}
		out = a.box(title, body, a.pw, a.ph, focused && multi)
		a.renderFocused = true
	})
	return out
}

func (a *App) renderNode(n *layoutNode) string {
	if n.leaf() {
		return a.renderPane(n.p, n.p == a.pane)
	}
	x, y := a.renderNode(n.a), a.renderNode(n.b)
	if n.stacked {
		return lipgloss.JoinVertical(lipgloss.Left, x, y)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, x, y)
}

// paneContent renders the focused pane's title and body.
func (a *App) paneContent() (string, string) {
	switch a.mode {
	case modeText:
		title := a.textTitle
		if a.textKind == "logs" {
			if a.follow {
				title += "  ⇣ following"
			} else {
				title += "  ⏸ paused"
			}
		}
		return title, a.vp.View()
	case modeAI:
		a.sizeChat()
		return a.aiTitle(), a.viewAI(a.pw)
	case modePulse:
		return "Pulse · " + a.client.NamespaceLabel(), a.viewPulse(a.pw, a.ph)
	}
	return a.tableTitle(), a.viewTable(a.pw, a.ph)
}

func paneSummary(p *pane) string {
	switch p.mode {
	case modeText:
		return strings.SplitN(p.textTitle, " ", 2)[0]
	case modeAI:
		return "AI"
	case modePulse:
		return "Pulse"
	}
	return p.res.Short
}
