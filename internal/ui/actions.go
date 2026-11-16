package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

type rowActionItem struct {
	key, label string
	run        func() tea.Cmd
}

// press sends a key to the table as if typed (used by the actions menu).
func (a *App) pressKey(k string) tea.Cmd {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	return a.updateTable(msg)
}

// rowActions lists what can be done with the selected row of a Kubernetes resource table.
func (a *App) rowActions(row kube.Row) []rowActionItem {
	r := a.res
	key := func(k, label string) rowActionItem {
		return rowActionItem{k, label, func() tea.Cmd { return a.pressKey(k) }}
	}
	var items []rowActionItem
	add := func(it ...rowActionItem) { items = append(items, it...) }
	add(key("enter", "describe"), key("y", "YAML"))
	if graphKinds[r.Kind] {
		add(key("w", "relationships graph: what it is connected to, health of each part"),
			key("t", "timeline: events, rollouts, restarts, scaling"))
	}
	if r.Name == "pods" {
		add(key("h", "health report"), key("l", "logs"), key("s", "shell"), key("P", "network policies: who may connect"),
			key("N", "network debug: DNS, TCP and HTTP test from inside this pod"))
	} else if r.CanLogs() {
		add(key("l", "logs"))
	}
	if r.Name == "nodes" {
		add(key("h", "node health"), key("u", "cordon / uncordon"), key("X", "drain preview"))
	}
	if r.Name == "serviceaccounts" {
		add(key("p", "permissions: bindings and rules"))
	}
	if r.Name == "cronjobs" {
		add(key("h", "run history"), key("J", "run now"), key("u", "suspend / resume"))
	}
	if r.Name == "deployments" {
		add(key("o", "rollout history · diff · rollback"))
	}
	add(key("a", "AI analyze"))
	if graphKinds[r.Kind] {
		add(rowActionItem{":report", "save an incident report (relationships, timeline, issues, AI answer) as Markdown", func() tea.Cmd { return a.exportReport("") }})
	}
	add(rowActionItem{":pin", "watch this " + strings.ToLower(r.Kind) + ": alert on new problems", func() tea.Cmd { return a.pinCommand("", false) }},
		rowActionItem{":whocan", "who else can change this " + strings.ToLower(r.Kind), func() tea.Cmd { return a.openWhoCan("update " + r.Name) }})
	if r.Name == "deployments" || r.Name == "statefulsets" || r.Name == "daemonsets" {
		ws := strings.TrimSuffix(r.Kind, "") + "/" + row.Name
		add(rowActionItem{":rightsize", "right-sizing: requests vs peak usage", func() tea.Cmd {
			cmd := a.openSizing()
			a.filter = ws
			return cmd
		}})
	}
	if r.Watchable() && len(a.client.Contexts()) > 1 {
		add(rowActionItem{":diff @…", "compare with the same object on another cluster", func() tea.Cmd { return a.pickDiffContext(false) }},
			rowActionItem{":diff @… all", "compare every " + strings.ToLower(r.Kind) + " in this namespace with another cluster", func() tea.Cmd { return a.pickDiffContext(true) }})
	}
	add(key("e", "safe edit (diff + dry-run)"))
	if r.CanScale() {
		add(key("S", "scale"))
	}
	if r.HasSelector() || r.Name == "pods" {
		add(key("F", "port-forward"))
	}
	if r.Name == "deployments" || r.Name == "statefulsets" || r.Name == "daemonsets" {
		add(key("R", "rollout restart"))
	}
	add(key("D", "delete"))
	return items
}

// openActions shows every action for the selected row (space).
func (a *App) openActions() tea.Cmd {
	row, ok := a.selected()
	if !ok {
		return nil
	}
	items := a.rowActions(row)
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = fmt.Sprintf("%-14s %s", it.key, it.label)
	}
	title := fmt.Sprintf("Actions · %s %s · type to filter · enter run · keys work directly too", strings.ToLower(a.res.Kind), objName(row.Namespace, row.Name))
	a.openPicker(title, labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return items[i].run()
			}
		}
		return nil
	})
	return nil
}

// pickDiffContext asks which cluster to compare with.
func (a *App) pickDiffContext(all bool) tea.Cmd {
	var others []string
	for _, c := range a.client.Contexts() {
		if c != a.client.Context {
			others = append(others, c)
		}
	}
	a.openPicker("Compare with which cluster?", others, func(s string) tea.Cmd {
		arg := "@" + s
		if all {
			arg += " all"
		}
		return a.compareCommand(arg)
	})
	return nil
}
