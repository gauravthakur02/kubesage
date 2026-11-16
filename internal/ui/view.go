package ui

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"kubesage/internal/agent"
	"kubesage/internal/config"
	"kubesage/internal/kube"
)

// View renders the whole screen.
func (a *App) View() string {
	if a.w == 0 {
		return "starting kubesage…"
	}
	header := a.viewHeader()
	innerW, innerH := a.fullSize()
	var body, title string
	switch a.ov {
	case ovPicker:
		title, body = a.picker.title, a.viewPicker(innerW, innerH)
	case ovHelp:
		title, body = "Help", a.viewHelp(innerW, innerH)
	case ovConfirm:
		title, body = "Confirm", a.viewConfirm(innerW, innerH)
	case ovApprove:
		title, body = "AI wants to run a command", a.viewApprove(innerW, innerH)
	default:
		var panes string
		if a.zoomed {
			panes = a.renderPane(a.pane, true)
		} else {
			panes = a.renderNode(a.root)
		}
		return lipgloss.JoinVertical(lipgloss.Left, header, panes, a.viewFooter())
	}
	box := a.box(title, body, innerW, innerH, false)
	return lipgloss.JoinVertical(lipgloss.Left, header, box, a.viewFooter())
}

// box draws a rounded border with the title embedded in the top edge.
func (a *App) box(title, body string, w, h int, focused bool) string {
	b := lipgloss.RoundedBorder()
	bc := lipgloss.NewStyle().Foreground(a.st.T.Border)
	if focused {
		b = lipgloss.ThickBorder()
		bc = lipgloss.NewStyle().Foreground(a.st.T.Accent)
	}
	t := " " + ansi.Truncate(title, w-4, "…") + " "
	top := bc.Render(b.TopLeft+b.Top) + a.st.BoxTitle.Render(t) +
		bc.Render(strings.Repeat(b.Top, max(0, w-1-lipgloss.Width(t)))+b.TopRight)
	lines := strings.Split(body, "\n")
	var out []string
	out = append(out, top)
	for i := 0; i < h; i++ {
		l := ""
		if i < len(lines) {
			l = lines[i]
		}
		pad := w - lipgloss.Width(l)
		if pad < 0 {
			l = ansi.Truncate(l, w, "")
			pad = 0
		}
		out = append(out, bc.Render(b.Left)+l+strings.Repeat(" ", pad)+bc.Render(b.Right))
	}
	out = append(out, bc.Render(b.BottomLeft+strings.Repeat(b.Bottom, w)+b.BottomRight))
	return strings.Join(out, "\n")
}

func (a *App) viewHeader() string {
	s := a.st
	left := []string{LogoMark(s)}
	ctxStyle := s.Pill
	if a.danger() {
		ctxStyle = s.Danger
	}
	if a.launcher {
		left = append(left, s.PillAlt.Render("choose a cluster"))
	} else {
		left = append(left, ctxStyle.Render("ctx "+a.client.Context))
	}
	if !a.launcher {
		left = append(left, s.Pill.Render("ns "+a.client.NamespaceLabel()))
	}
	if v := a.cl.get(a.client.Context).version; v != "" {
		left = append(left, s.Pill.Render("k8s "+v))
	}
	aiLabel := "AI off"
	if a.provider != nil {
		aiLabel = "AI " + a.profile + " · " + a.provider.Model()
	}
	left = append(left, s.PillAlt.Render(aiLabel))
	if ps := a.panes(); len(ps) > 1 {
		var parts []string
		for i, p := range ps {
			label := fmt.Sprintf("%d:%s", i+1, paneSummary(p))
			if p == a.pane {
				label = "[" + label + "]"
			}
			parts = append(parts, label)
		}
		pill := "▦ " + strings.Join(parts, " ")
		if a.zoomed {
			pill += " · zoomed"
		}
		left = append(left, s.Pill.Render(pill))
	}
	if pf := a.forwardPill(); pf != "" {
		left = append(left, lipgloss.NewStyle().Foreground(s.T.Bg).Background(s.T.Info).Padding(0, 1).Render(pf))
	}
	if a.readOnly() {
		left = append(left, s.Danger.Render("READ-ONLY"))
	} else if a.danger() {
		left = append(left, s.Danger.Render("⚠ PROD"))
	}
	line1 := strings.Join(left, " ")
	if lipgloss.Width(line1) > a.w {
		line1 = ansi.Truncate(line1, a.w, "…")
	}

	var tabs []string
	if len(a.tabs) > 1 {
		for i := range a.tabs {
			label := fmt.Sprintf("%d %s", i+1, a.tabLabel(i))
			if i == a.tab {
				tabs = append(tabs, lipgloss.NewStyle().Foreground(s.T.Bg).Background(s.T.Accent2).Bold(true).Padding(0, 1).Render(label))
			} else {
				tabs = append(tabs, lipgloss.NewStyle().Foreground(s.T.Fg).Background(s.T.Subtle).Padding(0, 1).Render(label))
			}
			tabs = append(tabs, " ")
		}
		tabs = append(tabs, s.Muted.Render("│ "))
	}
	tabs = append(tabs, s.Key.Render(" 0 ☰ All")+" ")
	for i, f := range a.cfg.Favorites {
		r, ok := kube.Lookup(f)
		if !ok {
			continue
		}
		label := fmt.Sprintf("%d %s", i+1, r.Short)
		if r.Name == a.res.Name && a.mode == modeTable {
			tabs = append(tabs, s.TabActive.Render(label))
		} else {
			tabs = append(tabs, s.Tab.Render(label))
		}
	}
	pulse := s.Tab.Render("H Pulse")
	if a.mode == modePulse {
		pulse = s.TabActive.Render("H Pulse")
	}
	aiTab := s.Tab.Render("A AI")
	if a.mode == modeAI {
		aiTab = s.TabActive.Render("A AI")
	}
	helmTab := s.Tab.Render("M ⎈ Helm")
	if a.mode == modeTable && (a.res.Name == helmRes || a.res.Name == chartsRes) {
		helmTab = s.TabActive.Render("M ⎈ Helm")
	}
	tabs = append(tabs, s.Muted.Render("│"), helmTab, pulse, aiTab)
	line2 := ansi.Truncate(strings.Join(tabs, ""), a.w, "…")
	return line1 + "\n" + line2
}

func (a *App) viewFooter() string {
	s := a.st
	var line1 string
	switch a.ov {
	case ovCommand, ovFilter, ovInput:
		line1 = s.Key.Render(a.inputHint) + a.input.View()
	default:
		line1 = a.hints()
	}
	line1 = ansi.Truncate(line1, a.w, "…")

	st := a.status
	style := s.Status
	if a.statusErr {
		style = s.ErrText
	}
	right := s.Muted.Render(fmt.Sprintf("↻ %ds · %s", a.cfg.RefreshSeconds, time.Now().Format("15:04:05")))
	if a.loading || a.aiBusy || a.pulseBusy || a.ag.running {
		right = a.spin.View() + " " + right
	}
	left := ansi.Truncate(style.Render(st), max(0, a.w-lipgloss.Width(right)-1), "…")
	gap := max(1, a.w-lipgloss.Width(left)-lipgloss.Width(right))
	return line1 + "\n" + left + strings.Repeat(" ", gap) + right
}

func (a *App) hints() string {
	var list []kv
	switch a.mode {
	case modeText:
		if a.textKind == "editdiff" {
			list = []kv{{"A", "apply"}, {"e", "edit again"}, {"esc", "discard"}, {"/", "search"}, {"↑↓", "scroll"}}
			break
		}
		list = []kv{{"esc", "back"}, {"↑↓/pgup", "scroll"}, {"g/G", "top/bottom"}, {"a", "ask AI"}, {"y", "yaml"}, {"d", "describe"}}
		if a.textKind == "logs" {
			list = append(list, kv{"f", "follow"}, kv{"t", "time window"}, kv{"T", "timestamps"})
		}
		list = append(list, kv{"/", "search"})
		if a.search != "" {
			list = append(list, kv{"n/N", "next/prev match"})
		}
		wrap := "wrap: on"
		if a.nowrap {
			wrap = "wrap: off"
		}
		list = append(list, kv{"w", wrap})
		if a.target.res.Name == "secrets" {
			if a.reveal {
				list = append(list, kv{"x", "mask"})
			} else {
				list = append(list, kv{"x", "unmask"})
			}
		}
		if a.target.res.Name == "pods" && a.textKind != "health" {
			list = append(list, kv{"h", "health"})
		}
		if a.target.res.Name == "pods" {
			list = append(list, kv{"s", "shell"})
		}
	case modeAI:
		list = []kv{{"enter", "send"}, {"esc", "stop/back"}, {"ctrl+r", "run cmd from reply"}, {"pgup/pgdn", "scroll"}, {"ctrl+n", "new chat"}, {"ctrl+o", "AI profile"}}
	case modePulse:
		list = []kv{{"↑↓", "select"}, {"enter/a", "AI analyze"}, {"t", "AI triage all"}, {"d", "describe"}, {"l", "logs"}, {"r", "refresh"}, {"esc", "back"}}
	default:
		if a.res.Name == clustersRes {
			list = []kv{{"enter", "open"}, {"space", "select several"}, {"v", "side by side"}, {"c", "switch this tab"},
				{"p", "health check"}, {"/", "filter"}, {"q", "quit"}}
			break
		}
		if h := a.kubesageHints(); h != nil {
			list = h
			break
		}
		if a.res.Name == chartsRes {
			list = []kv{{"enter", "releases"}, {"v", "default values"}, {"r", "README"}, {"t", "templates"}, {"i", "info"}, {"esc", "back"}}
			break
		}
		if a.res.Name == helmRes {
			list = []kv{{"enter", "values"}, {"m", "manifest"}, {"h", "history · diff · rollback"}, {"/", "filter"}, {"esc", "back"}}
			break
		}
		if _, _, ok := isHelmHistory(a.res); ok {
			list = []kv{{"enter/d", "diff vs current"}, {"v", "values"}, {"U", "helm rollback"}, {"esc", "back"}}
			break
		}
		if a.res.Name == policyRes {
			list = []kv{{"enter", "workload YAML"}, {"a", "AI fix"}, {"e", "safe edit"}, {"/", "filter"}, {"esc", "back"}}
			break
		}
		if _, _, ok := isRollout(a.res); ok {
			list = []kv{{"enter/d", "diff vs current"}, {"U", "roll back to this revision"}, {"esc", "back to deployments"}, {"r", "refresh"}}
			break
		}
		if a.res.Name == "nodes" {
			list = []kv{{"h", "node health"}, {"u", "cordon/uncordon"}, {"X", "drain preview"}, {":taint", "taints"}, {"enter", "describe"},
				{"a", "AI analyze"}, {"/", "filter"}, {"?", "help"}}
			break
		}
		if a.res.Name == "deployments" {
			list = []kv{{"o", "rollout history"}, {"enter", "describe"}, {"a", "AI analyze"}, {"l", "logs"}, {"S", "scale"},
				{"R", "restart"}, {"y", "yaml"}, {"/", "filter"}, {":", "cmd"}, {"?", "help"}}
			break
		}
		if a.res.Name == "secrets" {
			list = []kv{{"enter", "view (masked)"}, {"y", "yaml"}, {"d", "describe"}, {"/", "filter"}, {":", "cmd"}, {"?", "help"}}
			break
		}
		list = []kv{{"0", "all resources"}, {"enter", "describe"}, {"h", "health"}, {"a", "AI analyze"}, {"l", "logs"}, {"s", "shell"}, {"y", "yaml"}, {"/", "filter"},
			{"F", "port-fwd"}, {"f", "forwards"}, {"|/-", "split"}, {"!", "kubectl"}, {":", "cmd"}, {"n", "ns"}, {"c", "ctx"}, {"H", "pulse"}, {"?", "help"}}
	}
	if len(a.panes()) > 1 {
		list = append([]kv{{"ctrl+w", "next pane"}, {"z", "zoom"}, {"ctrl+x", "close"}}, list...)
	}
	var parts []string
	for _, p := range list {
		parts = append(parts, a.st.Key.Render(p.k)+" "+a.st.KeyDesc.Render(p.v))
	}
	return strings.Join(parts, a.st.Muted.Render("  "))
}

// ---------- table ----------

func (a *App) tableTitle() string {
	if a.res.Name == clustersRes {
		t := fmt.Sprintf("Clusters (%d)", len(a.visible))
		if a.launcher {
			t = fmt.Sprintf("Choose cluster(s) (%d) · space select · enter open", len(a.visible))
		}
		if a.filter != "" {
			t += " · /" + a.filter
		}
		return t
	}
	if ns, name, ok := isRollout(a.res); ok {
		return fmt.Sprintf("Rollout history deployment %s/%s · %s", ns, name, a.table.Note)
	}
	if t := a.kubesageTitle(); t != "" {
		if a.filter != "" {
			t += " · /" + a.filter
		}
		return t
	}
	if a.res.Name == helmRes || a.res.Name == chartsRes {
		t := fmt.Sprintf("Helm releases · %s · %s", a.client.NamespaceLabel(), a.table.Note)
		if a.res.Name == chartsRes {
			t = fmt.Sprintf("Helm charts in use · %s · %s", a.client.NamespaceLabel(), a.table.Note)
		}
		if a.filter != "" {
			t += " · /" + a.filter
		}
		return t
	}
	if ns, name, ok := isHelmHistory(a.res); ok {
		return fmt.Sprintf("Helm history %s/%s · %s", ns, name, a.table.Note)
	}
	if a.res.Name == policyRes {
		t := fmt.Sprintf("Policy & security · %s · %s", a.client.NamespaceLabel(), a.table.Note)
		if a.filter != "" {
			t += " · /" + a.filter
		}
		return t
	}
	t := fmt.Sprintf("%s (%d)", plural(a.res.Kind), len(a.visible))
	if a.res.Namespaced {
		t += " · " + a.client.NamespaceLabel()
	}
	t += a.liveBadge()
	if a.filter != "" {
		t += " · /" + a.filter
	}
	return t
}

func (a *App) viewTable(w, h int) string {
	s := a.st
	if a.launcher && h > 14 {
		banner := LogoFrame(s, "choose the cluster(s) to open below", a.logoFrame())
		for i := range banner {
			banner[i] = "  " + banner[i]
		}
		return strings.Join(banner, "\n") + "\n" + a.viewTableBody(w, h-len(banner)-1)
	}
	return a.viewTableBody(w, h)
}

func (a *App) viewTableBody(w, h int) string {
	s := a.st
	if a.listErr != nil && len(a.table.Rows) == 0 {
		return s.ErrText.Render("  ✗ " + a.listErr.Error())
	}
	if len(a.table.Columns) == 0 {
		return s.Muted.Render("  " + a.spin.View() + " loading " + a.res.Name + "…")
	}
	cols := a.table.Columns
	avail := w - 4 // dot + forward mark + spaces

	// Port-forward indicators: tag the NAME cell and draw a ⇄ next to the health dot.
	cellsOf := func(idx int) []string { return a.table.Rows[idx].Cells }
	marks := map[int]int{} // 1 = forwarded directly, 2 = receives traffic via another forward
	if canForward(a.res) && activeForwards() > 0 {
		nameCol := -1
		for i, c := range cols {
			if c == "NAME" {
				nameCol = i
			}
		}
		disp := map[int][]string{}
		for _, idx := range a.visible {
			row := a.table.Rows[idx]
			direct, indirect := kube.ForwardsFor(a.client.Context, a.res.Name, row.Namespace, row.Name)
			if len(direct)+len(indirect) == 0 || nameCol < 0 {
				continue
			}
			var tags []string
			for _, f := range direct {
				tags = append(tags, fmt.Sprintf(":%d", f.Local))
			}
			marks[idx] = 1
			if len(direct) == 0 {
				marks[idx] = 2
				for _, f := range indirect {
					tags = append(tags, fmt.Sprintf("via %s :%d", f.ObjectDesc, f.Local))
				}
			}
			cells := append([]string(nil), row.Cells...)
			cells[nameCol] += "  ⇄ " + strings.Join(tags, " ")
			disp[idx] = cells
		}
		cellsOf = func(idx int) []string {
			if c, ok := disp[idx]; ok {
				return c
			}
			return a.table.Rows[idx].Cells
		}
	}
	// Memory trend (last ~15 min) as an extra column right after MEM.
	memCol := -1
	for i, c := range cols {
		if c == "MEM" {
			memCol = i
		}
	}
	hasTrend := false
	if memCol >= 0 && a.res.Name == "pods" && w >= 110 {
		for _, idx := range a.visible {
			r := a.table.Rows[idx]
			if len(a.hist.get(histKey(a.client.Context, r.Namespace, r.Name))) >= 2 {
				hasTrend = true
				break
			}
		}
	}
	if hasTrend {
		cols = append(append(append([]string{}, cols[:memCol+1]...), "MEM 15m"), cols[memCol+1:]...)
		base := cellsOf
		cellsOf = func(idx int) []string {
			row := a.table.Rows[idx]
			c := base(idx)
			trend := sparkline(series(a.hist.get(histKey(a.client.Context, row.Namespace, row.Name)), true), 10)
			return append(append(append([]string{}, c[:memCol+1]...), trend), c[memCol+1:]...)
		}
	}
	markOf := func(idx int) string {
		switch marks[idx] {
		case 1:
			return lipgloss.NewStyle().Foreground(s.T.Accent2).Bold(true).Render("⇄")
		case 2:
			return lipgloss.NewStyle().Foreground(s.T.Accent2).Render("↔")
		}
		return " "
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipgloss.Width(c)
	}
	for _, idx := range a.visible {
		for i, cell := range cellsOf(idx) {
			if i < len(widths) {
				widths[i] = max(widths[i], min(60, lipgloss.Width(cell)))
			}
		}
	}
	gap := 2
	total := func() int {
		t := 0
		for _, x := range widths {
			t += x + gap
		}
		return t
	}
	for total() > avail {
		wi := 0
		for i := range widths {
			if widths[i] > widths[wi] {
				wi = i
			}
		}
		if widths[wi] <= 4 {
			break
		}
		widths[wi]--
	}
	if extra := avail - total(); extra > 0 {
		widths[len(widths)-1] += extra
	}
	renderCells := func(cells []string) string {
		var b strings.Builder
		for i := range cols {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			c = ansi.Truncate(c, widths[i], "…")
			b.WriteString(c + strings.Repeat(" ", max(0, widths[i]-lipgloss.Width(c))+gap))
		}
		return b.String()
	}

	bodyH := h - 1
	if a.cursor < a.offset {
		a.offset = a.cursor
	}
	if a.cursor >= a.offset+bodyH {
		a.offset = a.cursor - bodyH + 1
	}
	a.offset = max(0, min(a.offset, max(0, len(a.visible)-bodyH)))

	lines := []string{"    " + s.Header.Render(renderCells(cols))}
	for i := a.offset; i < min(len(a.visible), a.offset+bodyH); i++ {
		idx := a.visible[i]
		row := a.table.Rows[idx]
		text := renderCells(cellsOf(idx))
		if i == a.cursor && !a.renderFocused {
			lines = append(lines, lipgloss.NewStyle().Background(s.T.Subtle).Foreground(s.T.Fg).Render(" ▸"+markOf(idx)+" "+text))
		} else if i == a.cursor {
			mark := " "
			if marks[idx] > 0 {
				mark = "⇄"
			}
			lines = append(lines, s.Selected.Render(" ▶"+mark+" "+text))
		} else {
			lines = append(lines, " "+s.dot(row.Health)+markOf(idx)+" "+s.health(row.Health).Render(text))
		}
	}
	if len(a.visible) == 0 {
		msg := "no " + a.res.Name + " found"
		if a.filter != "" {
			msg += " matching /" + a.filter
		}
		lines = append(lines, s.Muted.Render("   "+msg))
	}
	return strings.Join(lines, "\n")
}

// ---------- text view ----------

var (
	yamlKeyRe  = regexp.MustCompile(`^(\s*-?\s*)([\w.\-/]+):(\s|$)`)
	descKeyRe  = regexp.MustCompile(`^(\s*)([A-Z][\w \-/()]*):(\s|$)`)
	errWordRe  = regexp.MustCompile(`(?i)\b(error|fatal|panic|exception|fail(ed|ure)?|oomkilled|crashloop\w*|backoff)\b`)
	warnWordRe = regexp.MustCompile(`(?i)\b(warn(ing)?|unhealthy|timeout|retry(ing)?)\b`)
)

func (a *App) renderText(gotoBottom bool) {
	s := a.st
	if len(a.textLines) == 0 {
		a.vp.SetContent(s.Muted.Render("  " + a.spin.View() + " loading…"))
		return
	}
	out := make([]string, 0, len(a.textLines))
	a.matches = a.matches[:0]
	lowerQ := strings.ToLower(a.search)
	for i, l := range a.textLines {
		l = strings.ReplaceAll(l, "\t", "    ")
		// Merged logs: colour the [pod] prefix and keep it out of the content styling.
		prefix := ""
		if a.textKind == "logs" && len(a.logSpec.pods) > 1 && strings.HasPrefix(l, "[") {
			if j := strings.Index(l, "] "); j > 0 {
				prefix, l = a.podTagStyle(l[1:j]).Render(l[:j+1])+" ", l[j+2:]
			}
		}
		if a.search != "" && a.textKind != "health" && a.textKind != "secret" && strings.Contains(strings.ToLower(ansi.Strip(l)), lowerQ) {
			current := a.matchIdx >= 0 && a.matchIdx < len(a.matches)+1 && len(a.matches) == a.matchIdx
			a.matches = append(a.matches, i)
			out = append(out, prefix+a.highlightSearch(ansi.Strip(l), current))
			continue
		}
		switch a.textKind {
		case "yaml", "helmvalues", "manifest":
			if m := yamlKeyRe.FindStringSubmatchIndex(l); m != nil {
				l = l[:m[4]] + s.YAMLKey.Render(l[m[4]:m[5]]) + s.Muted.Render(":") + s.Base.Render(l[m[5]+1:])
			} else if strings.HasPrefix(strings.TrimSpace(l), "#") {
				l = s.Muted.Render(l)
			}
		case "describe":
			if m := descKeyRe.FindStringSubmatchIndex(l); m != nil {
				l = l[:m[4]] + s.YAMLKey.Render(l[m[4]:m[5]]+":") + s.Base.Render(l[m[5]+1:])
			} else if strings.Contains(l, "Warning") {
				l = s.WarnText.Render(l)
			}
		case "logs":
			switch {
			case strings.HasPrefix(l, "──") || strings.HasPrefix(l, "[kubesage]"):
				l = s.Muted.Render(l)
			case errWordRe.MatchString(l):
				l = s.ErrText.Render(l)
			case warnWordRe.MatchString(l):
				l = s.WarnText.Render(l)
			}
		}
		out = append(out, prefix+l)
	}
	a.visualStart = a.visualStart[:0]
	visual := 0
	for i, l := range out {
		if !a.nowrap && a.vp.Width > 0 && ansi.StringWidth(l) > a.vp.Width {
			out[i] = ansi.Hardwrap(l, a.vp.Width, true)
		}
		a.visualStart = append(a.visualStart, visual)
		visual += strings.Count(out[i], "\n") + 1
	}
	a.vp.SetContent(strings.Join(out, "\n"))
	if gotoBottom && a.textKind == "logs" {
		a.vp.GotoBottom()
	} else if gotoBottom {
		a.vp.GotoTop()
	}
}

// ---------- AI chat ----------

func (a *App) aiTitle() string {
	if a.provider == nil {
		return "AI assistant · not configured"
	}
	return fmt.Sprintf("AI assistant · %s · %s", a.profile, a.provider.Model())
}

func (a *App) viewAI(w int) string {
	sep := a.st.Muted.Render(strings.Repeat("─", w))
	return a.aiVP.View() + "\n" + sep + "\n" + a.aiInput.View()
}

func (a *App) renderChat() {
	s := a.st
	w := max(20, a.aiVP.Width-2)
	if len(a.chat) == 0 {
		var b strings.Builder
		b.WriteString("\n")
		for _, l := range LogoFrame(s, "AI copilot · "+a.profile, a.logoFrame()) {
			b.WriteString("  " + l + "\n")
		}
		b.WriteString("\n" + s.Title.Render("  Your Kubernetes copilot") + "\n\n")
		tips := []string{
			"Press a on any row (pod, deployment, node…) to get an AI diagnosis with manifest, events and logs attached.",
			"Press H for Pulse, then t to let the AI triage every detected issue.",
			"Or just ask: “why are pods in payments pending?”, “write a PDB for my api deployment”.",
			"Switch model with ctrl+o. Set an API key with :ai-key, add a provider/model with :ai-add (keys go to " + config.KeyringName() + ").",
		}
		for _, t := range tips {
			b.WriteString(lipgloss.NewStyle().Width(w).Render(s.Key.Render("  • ")+s.Base.Render(t)) + "\n")
		}
		if a.provider == nil {
			b.WriteString("\n" + s.ErrText.Render(lipgloss.NewStyle().Width(w).Render("  ✗ "+fmt.Sprint(a.providerErr))))
		}
		if a.cfg.AI.Redact {
			b.WriteString("\n" + s.Muted.Render("  Secret values and sensitive env vars are redacted before anything is sent to the model."))
		}
		a.aiVP.SetContent(b.String())
		return
	}
	var b strings.Builder
	for i, c := range a.chat {
		switch c.role {
		case "user":
			b.WriteString(s.UserLabel.Render("you") + "\n")
			b.WriteString(lipgloss.NewStyle().Width(w).Foreground(s.T.Fg).Render(c.display) + "\n\n")
		case "assistant":
			b.WriteString(s.BotLabel.Render("kubesage") + "\n")
			text := c.content
			if text == "" && a.aiBusy && i == len(a.chat)-1 {
				text = a.spin.View() + " thinking…"
				b.WriteString(s.Muted.Render(text) + "\n\n")
				continue
			}
			b.WriteString(renderMarkdown(s, text, w) + "\n\n")
		case "error":
			b.WriteString(s.ErrText.Render(lipgloss.NewStyle().Width(w).Render("✗ "+c.display)) + "\n\n")
		case "tool":
			b.WriteString(s.PillAlt.Render("kubectl") + "\n")
			for j, l := range strings.Split(c.display, "\n") {
				l = ansi.Truncate(l, w-2, "…")
				switch {
				case j == 0:
					b.WriteString(s.Key.Render(l) + "\n")
				case strings.HasPrefix(l, "✗"):
					b.WriteString(s.ErrText.Render(l) + "\n")
				default:
					b.WriteString(s.Muted.Render("│ ") + s.Base.Render(l) + "\n")
				}
			}
			b.WriteString("\n")
		}
	}
	if a.ag.running {
		if c, ok := a.currentProposal(); ok {
			b.WriteString(s.Muted.Render(a.spin.View() + " running " + c.Display() + " …"))
		}
	} else if a.aiBusy && len(a.chat) > 0 && a.chat[len(a.chat)-1].role == "user" {
		b.WriteString(s.Muted.Render(a.spin.View() + " gathering cluster data…"))
	}
	atBottom := a.aiVP.AtBottom() || a.aiBusy
	a.aiVP.SetContent(lipgloss.NewStyle().PaddingLeft(1).Render(b.String()))
	if atBottom {
		a.aiVP.GotoBottom()
	}
}

var (
	boldRe       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	inlineCodeRe = regexp.MustCompile("`([^`]+)`")
)

// renderMarkdown is a small, dependency-free markdown renderer for chat output.
func renderMarkdown(s Styles, md string, w int) string {
	var out []string
	inCode := false
	wrap := lipgloss.NewStyle().Width(w)
	for _, line := range strings.Split(md, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") {
			inCode = !inCode
			if inCode {
				lang := strings.TrimPrefix(trim, "```")
				out = append(out, s.Muted.Render("  ┌─ "+lang))
			} else {
				out = append(out, s.Muted.Render("  └─"))
			}
			continue
		}
		if inCode {
			out = append(out, s.Muted.Render("  │ ")+s.Code.Render(ansi.Truncate(line, w-4, "…")))
			continue
		}
		inline := func(t string) string {
			t = boldRe.ReplaceAllStringFunc(t, func(m string) string {
				return lipgloss.NewStyle().Bold(true).Foreground(s.T.Fg).Render(boldRe.FindStringSubmatch(m)[1])
			})
			return inlineCodeRe.ReplaceAllStringFunc(t, func(m string) string {
				return s.Code.Render(inlineCodeRe.FindStringSubmatch(m)[1])
			})
		}
		switch {
		case strings.HasPrefix(trim, "#"):
			h := strings.TrimSpace(strings.TrimLeft(trim, "#"))
			out = append(out, s.Header.Render("▍"+inline(h)))
		case strings.HasPrefix(trim, "- ") || strings.HasPrefix(trim, "* "):
			indent := len(line) - len(strings.TrimLeft(line, " "))
			body := wrap.Width(w - indent - 2).Render(s.Base.Render(inline(trim[2:])))
			body = strings.ReplaceAll(body, "\n", "\n"+strings.Repeat(" ", indent+2))
			out = append(out, strings.Repeat(" ", indent)+s.Key.Render("• ")+body)
		default:
			out = append(out, wrap.Render(s.Base.Render(inline(line))))
		}
	}
	return strings.Join(out, "\n")
}

// ---------- pulse ----------

func (a *App) bar(val, total int64, w int, color lipgloss.Color) string {
	if w < 4 {
		w = 4
	}
	filled := 0
	if total > 0 {
		filled = int(float64(w) * float64(min(val, total)) / float64(total))
	}
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", filled)) +
		a.st.Muted.Render(strings.Repeat("░", w-filled))
}

func pct(v, t int64) string {
	if t == 0 {
		return "  –"
	}
	return fmt.Sprintf("%3.0f%%", 100*float64(v)/float64(t))
}

func (a *App) viewPulse(w, h int) string {
	s := a.st
	if a.pulse == nil {
		if a.pulseErr != nil {
			return s.ErrText.Render("  ✗ " + a.pulseErr.Error())
		}
		return s.Muted.Render("  " + a.spin.View() + " scanning cluster…")
	}
	p := a.pulse
	barW := max(10, min(40, w/3))
	t := s.T
	healthColor := func(ok, total int) lipgloss.Color {
		switch {
		case total == 0 || ok == total:
			return t.OK
		case ok == 0:
			return t.Err
		}
		return t.Warn
	}
	label := func(l string) string { return s.Title.Render(fmt.Sprintf("  %-12s", l)) }
	var lines []string
	lines = append(lines, "")
	lines = append(lines, label("Nodes")+a.bar(int64(p.NodesReady), int64(p.NodesTotal), barW, healthColor(p.NodesReady, p.NodesTotal))+
		s.Base.Render(fmt.Sprintf("  %d/%d ready", p.NodesReady, p.NodesTotal)))
	lines = append(lines, label("Deployments")+a.bar(int64(p.DeploysReady), int64(p.DeploysTotal), barW, healthColor(p.DeploysReady, p.DeploysTotal))+
		s.Base.Render(fmt.Sprintf("  %d/%d available", p.DeploysReady, p.DeploysTotal)))

	// stacked pod bar
	podBar := ""
	if p.PodsTotal > 0 {
		seg := func(n int, c lipgloss.Color) string {
			return lipgloss.NewStyle().Foreground(c).Render(strings.Repeat("█", n*barW/p.PodsTotal))
		}
		podBar = seg(p.PodsRunning, t.OK) + seg(p.PodsOK, t.Muted) + seg(p.PodsPending, t.Warn) + seg(p.PodsFailed, t.Err)
		podBar += s.Muted.Render(strings.Repeat("░", max(0, barW-lipgloss.Width(podBar))))
	}
	lines = append(lines, label("Pods")+podBar+"  "+
		s.OKText.Render(fmt.Sprintf("%d running ", p.PodsRunning))+
		s.Muted.Render(fmt.Sprintf("%d done ", p.PodsOK))+
		s.WarnText.Render(fmt.Sprintf("%d pending/unready ", p.PodsPending))+
		s.ErrText.Render(fmt.Sprintf("%d failing", p.PodsFailed)))
	lines = append(lines, label("CPU req")+a.bar(p.CPURequested, p.CPUAllocatable, barW, t.Info)+
		s.Base.Render(fmt.Sprintf("  %s  %dm / %dm", pct(p.CPURequested, p.CPUAllocatable), p.CPURequested, p.CPUAllocatable)))
	lines = append(lines, label("Memory req")+a.bar(p.MemRequested, p.MemAllocatable, barW, t.Accent2)+
		s.Base.Render(fmt.Sprintf("  %s  %.1fGi / %.1fGi", pct(p.MemRequested, p.MemAllocatable),
			float64(p.MemRequested)/(1<<30), float64(p.MemAllocatable)/(1<<30))))
	lines = append(lines, label("Signals")+s.Base.Render(fmt.Sprintf("%d container restarts · %d warning events (1h)", p.Restarts, p.WarningEvents)))
	for i, f := range runningForwards() {
		l := "Forwards"
		if i > 0 {
			l = ""
		}
		lines = append(lines, label(l)+lipgloss.NewStyle().Foreground(t.Info).Render("⇄ "+forwardLabel(f, a.client))+s.Muted.Render("   f to manage"))
	}
	if len(p.TopReasons) > 0 {
		var rs []string
		for r, n := range p.TopReasons {
			rs = append(rs, fmt.Sprintf("%s×%d", r, n))
		}
		sort.Strings(rs)
		lines = append(lines, label("Failing")+s.ErrText.Render(strings.Join(rs, "  ")))
	}
	if a.client.Namespace != kube.AllNamespaces {
		lines = append(lines, s.Muted.Render("  (requests are for this namespace; capacity is cluster-wide)"))
	}
	lines = append(lines, "")

	errs, warns := 0, 0
	for _, is := range a.issues {
		if is.Severity == kube.HealthErr {
			errs++
		} else {
			warns++
		}
	}
	head := fmt.Sprintf("  Detected issues  %s  %s", s.ErrText.Render(fmt.Sprintf("✗ %d errors", errs)), s.WarnText.Render(fmt.Sprintf("▲ %d warnings", warns)))
	lines = append(lines, s.Title.Render(head))
	if len(a.issues) == 0 {
		lines = append(lines, s.OKText.Render("  ✓ No problems detected by the rule engine."))
		return strings.Join(lines, "\n")
	}
	room := max(2, (h-len(lines))/2)
	start := 0
	if a.issueCursor >= room {
		start = a.issueCursor - room + 1
	}
	for i := start; i < min(len(a.issues), start+room); i++ {
		is := a.issues[i]
		icon := s.WarnText.Render("▲")
		if is.Severity == kube.HealthErr {
			icon = s.ErrText.Render("✗")
		}
		obj := is.Kind + "/" + is.Name
		if is.Namespace != "" {
			obj = is.Namespace + "/" + obj
		}
		line1 := fmt.Sprintf("%s %s  %s", icon, is.Reason, obj)
		line2 := "    " + is.Message
		if is.Hint != "" {
			line2 += "  → " + is.Hint
		}
		if i == a.issueCursor {
			lines = append(lines, s.Selected.Render(ansi.Truncate(" ▶ "+ansi.Strip(line1), w, "…")))
		} else {
			lines = append(lines, "   "+ansi.Truncate(line1, w-3, "…"))
		}
		lines = append(lines, s.Muted.Render(ansi.Truncate(line2, w, "…")))
	}
	return strings.Join(lines, "\n")
}

// ---------- modals ----------

func (a *App) center(content string, w, h int) string {
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, a.st.Modal.Render(content))
}

func (a *App) viewPicker(w, h int) string {
	s := a.st
	items := a.picker.visibleItems()
	maxRows := max(3, h-8)
	start := 0
	if a.picker.cursor >= maxRows {
		start = a.picker.cursor - maxRows + 1
	}
	var lines []string
	lines = append(lines, s.Title.Render(a.picker.title))
	lines = append(lines, s.Key.Render("filter › ")+s.Base.Render(a.picker.filter)+s.Muted.Render("▏"))
	lines = append(lines, "")
	for i := start; i < min(len(items), start+maxRows); i++ {
		it := ansi.Truncate(items[i], max(20, min(120, w-12)), "…")
		if i == a.picker.cursor {
			lines = append(lines, s.Selected.Render(" ▶ "+it+" "))
		} else {
			lines = append(lines, s.Base.Render("   "+it))
		}
	}
	if len(items) == 0 {
		lines = append(lines, s.Muted.Render("   no matches"))
	}
	lines = append(lines, "", s.Muted.Render("type to filter · ↑↓ move · enter select · esc cancel"))
	return a.center(strings.Join(lines, "\n"), w, h)
}

func (a *App) viewConfirm(w, h int) string {
	s := a.st
	lines := []string{s.WarnText.Bold(true).Render("⚠ " + a.confirm), ""}
	if a.inputHint != "" {
		lines = append(lines, s.Base.Render("This is a production-like context. Type this to confirm:"),
			s.Key.Render(a.inputHint), "", a.input.View(), "", s.Muted.Render("enter confirm · esc cancel"))
	} else {
		lines = append(lines, s.Base.Render("Press ")+s.Key.Render("y")+s.Base.Render(" to confirm, any other key to cancel."))
	}
	return a.center(strings.Join(lines, "\n"), w, h)
}

func (a *App) viewHelp(w, h int) string {
	s := a.st
	sections := []struct {
		title string
		keys  [][2]string
	}{
		{"Navigation", [][2]string{{"0  :resources", "browse ALL resource types"}, {":crd", "custom resource definitions"}, {"↑↓ j k", "move"}, {"g G", "top / bottom"}, {"1-9 tab", "resource tabs"}, {":pods :deploy …", "jump to resource"},
			{":pods kube-system", "resource in namespace"}, {"/", "filter rows"}, {"n  :ns", "namespace"}, {"c  :ctx", "kube context"}, {"r", "refresh"}}},
		{"Resource actions", [][2]string{{"enter d", "describe"}, {"h", "pod health report"}, {"y", "YAML"},
			{"enter (secret)", "secret viewer · x unmask"}, {"l / p", "logs / previous logs"}, {"s", "shell into container (bash/ash/sh)"},
			{"e", "safe edit: $EDITOR → diff + dry-run → apply"}, {"S", "scale"}, {"R", "rollout restart"}, {"o", "rollout history · diff · rollback (U)"},
			{"h u X (nodes)", "node health · cordon · drain preview"}, {":taint k=v:Effect", "add taint (k- removes)"}, {"D ctrl+d", "delete"},
			{"F", "port-forward"}, {":pf", "manage port-forwards"}, {"!  :!<cmd>", "run kubectl"}}},
		{"Panes", [][2]string{{"|  :vsplit [res]", "split side by side"}, {"-  :split [res]", "split stacked"}, {"L", "logs in a new pane"},
			{"ctrl+w  alt+1-6", "focus next / pane N"}, {"z", "zoom pane"}, {"[ ]", "resize pane"}, {"ctrl+x  :close", "close pane"},
			{":only", "keep only this pane"}, {"ctrl+t  :tabnew [ns|res]", "new tab"}, {">  <  :tab N", "next / prev / go to tab"},
			{"C  :clusters", "all clusters: health, open"}, {"M  :helm", "Helm releases (M again: charts) · values · history"}, {":charts", "Helm charts in use · defaults · README · templates"},
			{"ctrl+p", "command palette: screens, resources, views, clusters"}, {"I  :incident", "AI incident triage of everything unhealthy"},
			{"w / t", "relationships graph / event timeline of the selected object"}, {"P", "network policies of the selected pod"},
			{":rightsize", "requests vs peak usage, waste and cost"}, {":certs", "TLS certificates and expiry"},
			{":images", "images in use, latest tags, Trivy scan"}, {":storage", "PVC usage and mounts"}, {":gitops", "Argo CD and Flux apps"},
			{":diff @ctx [all]", "compare with another cluster"}, {":export csv|json|md", "save the table"}, {":audit", "log of every change made"},
			{"h / J / u on cronjobs", "run history / run now / suspend-resume"}, {"V  :policy", "policy & security checks"}, {":tabnew @ctx [ns]", "tab on another cluster"},
			{":vsplit @ctx [ns]", "compare clusters side by side"}, {"c  :ctx <name>", "switch this tab's cluster"},
			{":tabname <name>", "rename tab"}, {":tabclose", "close tab (ctrl+x on last pane)"}, {":ns! <ns>", "namespace for all panes"}, {":layout <name>", "triage, ops, dash, ai, single"},
			{":view save <name>", "save this tab's layout"}, {"b  :view [name]", "open a saved view"}}},
		{"AI & health", [][2]string{{"a", "AI analyze selected"}, {"A  :ai <q>", "AI chat"}, {"H  :pulse", "cluster pulse & issues"}, {"t (pulse)", "AI triage of all issues"},
			{"ctrl+r (AI)", "run cmd from reply"}, {"ctrl+o", "switch AI profile"},
			{":ai-key [profile]", "set API key (hidden)"}, {":ai-add", "add AI provider/model"}, {":theme", "switch theme (T cycles)"}, {":save", "persist theme/profile"}, {"q ctrl+c", "quit"}}},
	}
	var cols []string
	for _, sec := range sections {
		lines := []string{s.Title.Render(sec.title)}
		for _, k := range sec.keys {
			lines = append(lines, s.Key.Render(fmt.Sprintf("%-18s", k[0]))+s.KeyDesc.Render(k[1]))
		}
		cols = append(cols, strings.Join(lines, "\n"))
	}
	var content string
	switch {
	case w > 150:
		content = lipgloss.JoinHorizontal(lipgloss.Top, cols[0], "   ", cols[1], "   ", cols[2], "   ", cols[3])
	case w > 80:
		content = lipgloss.JoinVertical(lipgloss.Left,
			lipgloss.JoinHorizontal(lipgloss.Top, cols[0], "    ", cols[1]), "",
			lipgloss.JoinHorizontal(lipgloss.Top, cols[2], "    ", cols[3]))
	default:
		content = strings.Join(cols, "\n\n")
	}
	content = strings.Join(LogoFrame(s, "keyboard reference", a.logoFrame()), "\n") + "\n\n" + content
	content += "\n\n" + s.Muted.Render("press any key to close")
	return a.center(content, w, h)
}

func (a *App) viewApprove(w, h int) string {
	s := a.st
	c, ok := a.currentProposal()
	if !ok {
		return ""
	}
	riskStyle := s.WarnText.Bold(true)
	if c.Risk == agent.Read {
		riskStyle = s.OKText.Bold(true)
	}
	lines := []string{
		s.Title.Render("The AI wants to run this on ") + s.Danger.Render(a.aiTarget().Context),
		"",
		s.Code.Render(" " + ansi.Truncate(c.Display(), min(100, w-14), "…") + " "),
		"",
		riskStyle.Render("● "+c.Risk.String()) + s.Muted.Render("  "+c.Reason),
		s.Muted.Render(fmt.Sprintf("command %d of %d in this step", a.ag.idx+1, len(a.ag.queue))),
		"",
	}
	if a.ag.typing {
		lines = append(lines, s.Base.Render(`Production-like context: type "approve" and press enter`), "", a.input.View(), "", s.Muted.Render("esc back"))
	} else {
		lines = append(lines,
			s.Key.Render("y")+s.KeyDesc.Render(" run  ")+s.Key.Render("n")+s.KeyDesc.Render(" skip  ")+
				s.Key.Render("e")+s.KeyDesc.Render(" edit  ")+s.Key.Render("a")+s.KeyDesc.Render(" approve all in this step  ")+
				s.Key.Render("esc")+s.KeyDesc.Render(" stop"))
	}
	return a.center(strings.Join(lines, "\n"), w, h)
}

// plural turns a Kind into a title: NetworkPolicy → NetworkPolicies, Ingress → Ingresses, Endpoints → Endpoints.
func plural(kind string) string {
	switch {
	case strings.HasSuffix(kind, "ss"):
		return kind + "es"
	case strings.HasSuffix(kind, "s"):
		return kind
	case strings.HasSuffix(kind, "y") && !strings.HasSuffix(kind, "ay") && !strings.HasSuffix(kind, "ey"):
		return kind[:len(kind)-1] + "ies"
	}
	return kind + "s"
}

// kv is a key hint in the footer.
type kv struct{ k, v string }
