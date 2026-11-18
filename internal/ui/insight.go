package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// openReport shows a text report produced in the background (graph, timeline, netpol, …).
func (a *App) openReport(kind, loading string, fetch func(ctx context.Context) (title, content string, err error)) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind, a.textTitle, a.textLines = modeText, kind, loading, nil
	a.renderText(true)
	return tagPane(a.pane, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		title, content, err := fetch(ctx)
		if err != nil {
			return textMsg{title: "✗ " + loading, kind: kind, content: a.st.ErrText.Render("✗ " + err.Error())}
		}
		return textMsg{title: title, kind: kind, content: content}
	})
}

var graphKinds = map[string]bool{"Gateway": true, "HTTPRoute": true, "Ingress": true, "Service": true, "Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true, "Pod": true}

var kindIcon = map[string]string{"Ingress": "🌐", "Service": "⇄", "Deployment": "▣", "StatefulSet": "▤", "DaemonSet": "▦", "ReplicaSet": "▢",
	"Pod": "●", "ConfigMap": "⚙", "Secret": "🔑", "PersistentVolumeClaim": "⛁", "HPA": "↕", "HTTPRoute": "↪", "Gateway": "⛩"}

func (a *App) renderGraph(n *kube.GraphNode, prefix string, last, root bool, out *[]string) {
	s := a.st
	branch := ""
	if !root {
		branch = "├─ "
		if last {
			branch = "└─ "
		}
	}
	icon := kindIcon[n.Kind]
	if icon == "" {
		icon = "•"
	}
	line := s.Muted.Render(prefix+branch) + s.health(n.Health).Render(icon) + " " + s.YAMLKey.Render(n.Kind) + " " + s.Title.Render(n.Name)
	if n.Status != "" {
		line += "  " + s.health(n.Health).Render(n.Status)
	}
	if n.Note != "" {
		line += "  " + s.Muted.Render(n.Note)
	}
	*out = append(*out, line)
	childPrefix := prefix
	if !root {
		if last {
			childPrefix += "   "
		} else {
			childPrefix += "│  "
		}
	}
	for i, c := range n.Children {
		a.renderGraph(c, childPrefix, i == len(n.Children)-1, false, out)
	}
}

func countHealth(n *kube.GraphNode) (bad, warn, total int) {
	var rec func(*kube.GraphNode)
	rec = func(x *kube.GraphNode) {
		total++
		switch x.Health {
		case kube.HealthErr:
			bad++
		case kube.HealthWarn:
			warn++
		}
		for _, c := range x.Children {
			rec(c)
		}
	}
	rec(n)
	return
}

// openGraph shows what an object is connected to (key w).
func (a *App) openGraph(kind, ns, name string) tea.Cmd {
	c := a.client
	a.target = targetRef{a.res, ns, name}
	return a.openReport("graph", "loading relationships…", func(ctx context.Context) (string, string, error) {
		root, front, err := c.Relations(ctx, kind, ns, name)
		if err != nil {
			return "", "", err
		}
		var lines []string
		if front != nil && len(front.Children) > 0 {
			lines = append(lines, a.st.Muted.Render("reached through"))
			for i, ch := range front.Children {
				a.renderGraph(ch, "", i == len(front.Children)-1, false, &lines)
			}
			lines = append(lines, "")
		}
		a.renderGraph(root, "", true, true, &lines)
		bad, warn, total := countHealth(root)
		sum := fmt.Sprintf("%d objects", total)
		if bad > 0 {
			sum += fmt.Sprintf(" · %d failing", bad)
		}
		if warn > 0 {
			sum += fmt.Sprintf(" · %d degraded", warn)
		}
		lines = append(lines, "", a.st.Muted.Render("t timeline · y yaml · d describe · a AI analyze · esc back"))
		return fmt.Sprintf("relationships %s %s/%s · %s", strings.ToLower(kind), ns, name, sum), strings.Join(lines, "\n"), nil
	})
}

// openTimeline merges events, rollouts, restarts and pod starts (key t).
func (a *App) openTimeline(kind, ns, name string) tea.Cmd {
	c := a.client
	a.target = targetRef{a.res, ns, name}
	history := a.storedHistory()
	return a.openReport("timeline", "building timeline…", func(ctx context.Context) (string, string, error) {
		entries, err := c.TimelineWith(ctx, kind, ns, name, history)
		if err != nil {
			return "", "", err
		}
		return fmt.Sprintf("timeline %s %s/%s · %d entries, newest first · r relationships", strings.ToLower(kind), ns, name, len(entries)), a.renderTimeline(entries), nil
	})
}

var sourceLabel = map[string]string{"event": "event  ", "rollout": "rollout", "restart": "restart", "pod": "pod    ", "scale": "scale  "}

func (a *App) renderTimeline(entries []kube.TimelineEntry) string {
	s := a.st
	if len(entries) == 0 {
		return s.Muted.Render("nothing recorded (events expire after about an hour)")
	}
	var lines []string
	day := ""
	for _, e := range entries {
		if d := e.Time.Local().Format("Mon 2 Jan 2006"); d != day {
			if day != "" {
				lines = append(lines, "")
			}
			day = d
			lines = append(lines, s.Title.Render("── "+d+" "+strings.Repeat("─", 40)))
		}
		cnt := ""
		if e.Count > 1 {
			cnt = s.Muted.Render(fmt.Sprintf(" ×%d", e.Count))
		}
		lines = append(lines, fmt.Sprintf("%s %s  %s %s  %s%s",
			s.Muted.Render(e.Time.Local().Format("15:04:05")),
			s.Muted.Render(fmt.Sprintf("%8s", kube.Age(metav1Time(e.Time)))),
			s.health(e.Health).Render("▍"+sourceLabel[e.Source]),
			s.YAMLKey.Render(e.Object), s.health(e.Health).Render(e.Text), cnt))
	}
	return strings.Join(lines, "\n")
}

// insightRowAction handles w (relationships) and t (timeline) on graphable rows.
func (a *App) insightRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	if !graphKinds[a.res.Kind] {
		return nil, false
	}
	switch key {
	case "w":
		return a.openGraph(a.res.Kind, row.Namespace, row.Name), true
	case "t":
		return a.openTimeline(a.res.Kind, row.Namespace, row.Name), true
	}
	return nil, false
}
