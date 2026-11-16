package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// Key handling of table panes.

// paneKeys handles split/focus/zoom keys. Typing keys are left alone in the AI chat.
func (a *App) paneKeys(k tea.KeyMsg) (tea.Cmd, bool) {
	switch k.String() {
	case "ctrl+w":
		a.focusNext(1)
		return nil, true
	case "ctrl+x":
		if len(a.panes()) == 1 && len(a.tabs) > 1 {
			return a.closeTab(), true
		}
		a.closePane()
		return nil, true
	case "ctrl+t":
		return a.newTab(""), true
	case "alt+1", "alt+2", "alt+3", "alt+4", "alt+5", "alt+6":
		if ps := a.panes(); int(k.String()[4]-'1') < len(ps) {
			a.focus(ps[k.String()[4]-'1'])
		}
		return nil, true
	}
	if a.mode == modeAI {
		return nil, false
	}
	switch k.String() {
	case "|", "\\":
		return a.split(false, nil), true
	case "-", "_":
		return a.split(true, nil), true
	case ">", ".":
		if len(a.tabs) > 1 {
			return a.switchTab((a.tab + 1) % len(a.tabs)), true
		}
		return nil, true
	case "<", ",":
		if len(a.tabs) > 1 {
			return a.switchTab((a.tab - 1 + len(a.tabs)) % len(a.tabs)), true
		}
		return nil, true
	case "z":
		if len(a.panes()) > 1 {
			a.zoomed = !a.zoomed
			a.layout()
		}
		return nil, true
	case "[":
		a.resize(-0.05)
		return nil, true
	case "]":
		a.resize(0.05)
		return nil, true
	}
	return nil, false
}

func (a *App) updateTable(k tea.KeyMsg) tea.Cmd {
	_, innerH := a.bodySize()
	page := max(1, innerH-1)
	if a.res.Name == clustersRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.clusterRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if _, _, ok := isRollout(a.res); ok {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.rolloutRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if a.res.Name == policyRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.policyRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if row, ok := a.selected(); ok && a.mode == modeTable {
		if p, ok := a.pluginFor(k.String()); ok {
			return a.runPlugin(p, row)
		}
		if cmd, handled := a.insightRowAction(k.String(), row); handled {
			return cmd
		}
		if cmd, handled := a.extraRowAction(k.String(), row); handled {
			return cmd
		}
	}
	if a.res.Name == chartsRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.chartRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if a.res.Name == helmRes {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.helmRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if _, _, ok := isHelmHistory(a.res); ok {
		if row, ok := a.selected(); ok {
			if cmd, handled := a.helmHistoryRowAction(k.String(), row); handled {
				return cmd
			}
		}
	}
	if k.String() == "esc" && a.filter == "" && a.backTo != nil {
		back := *a.backTo
		a.backTo = nil
		return a.switchResource(back)
	}
	switch k.String() {
	case "q":
		a.quitCleanup()
		return tea.Quit
	case "!":
		a.openInput(ovInput, "kubectl ", a.runManual)
		return nil
	case "f":
		return a.openForwards()
	case "esc":
		if a.filter != "" {
			a.filter = ""
			a.applyFilter()
		}
	case "up", "k":
		a.cursor = max(0, a.cursor-1)
	case "down", "j":
		a.cursor = min(len(a.visible)-1, a.cursor+1)
	case "pgup", "ctrl+u":
		a.cursor = max(0, a.cursor-page)
	case "pgdown", "ctrl+f":
		a.cursor = min(len(a.visible)-1, a.cursor+page)
	case "g", "home":
		a.cursor = 0
	case "G", "end":
		a.cursor = len(a.visible) - 1
	case "tab", "shift+tab":
		favs := a.cfg.Favorites
		idx := 0
		for i, f := range favs {
			if r, ok := kube.Lookup(f); ok && r.Name == a.res.Name {
				idx = i
			}
		}
		if k.String() == "tab" {
			idx = (idx + 1) % len(favs)
		} else {
			idx = (idx - 1 + len(favs)) % len(favs)
		}
		if r, ok := kube.Lookup(favs[idx]); ok {
			return a.switchResource(r)
		}
	case "0":
		return a.openNavigator()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		i := int(k.String()[0] - '1')
		if i < len(a.cfg.Favorites) {
			if r, ok := kube.Lookup(a.cfg.Favorites[i]); ok {
				return a.switchResource(r)
			}
		}
	case ":":
		a.openInput(ovCommand, ":", func(s string) tea.Cmd { return a.runCommand(s) })
	case "/":
		a.openInput(ovFilter, "/", nil)
		a.input.SetValue(a.filter)
	case "?":
		a.ov = ovHelp
	case "r", "ctrl+r":
		return a.refresh()
	case "c":
		a.openPicker("Context", a.client.Contexts(), func(s string) tea.Cmd {
			return a.connect(s, "", "tab")
		})
	case "n":
		return fetchNamespaces(a.client)
	case "H":
		return a.openPulse()
	case "I":
		return a.openIncident()
	case "(":
		a.cycleSort(false)
		return nil
	case ")":
		a.cycleSort(true)
		return nil
	case " ", "space":
		if !a.isKubesageTable() && a.res.Watchable() {
			return a.openActions()
		}
	case "ctrl+p":
		return a.openPalette()
	case "M":
		if a.mode == modeTable && a.res.Name == helmRes {
			return a.openCharts() // M again: releases ⇄ charts
		}
		return a.openHelm()
	case "C":
		return a.openClusters()
	case "b":
		return a.openViewPicker()
	case "V":
		return a.openPolicy()
	case "A":
		a.openAI()
	case "T":
		names := ThemeNames()
		for i, n := range names {
			if n == a.st.T.Name {
				a.applyTheme(names[(i+1)%len(names)])
				break
			}
		}
	}
	if a.cursor < 0 {
		a.cursor = 0
	}
	row, ok := a.selected()
	if !ok {
		return nil
	}
	if a.isKubesageTable() {
		return nil
	}
	if _, _, hh := isHelmHistory(a.res); hh || a.res.Name == helmRes || a.res.Name == chartsRes {
		return nil
	}
	if _, _, ok := isRollout(a.res); ok || a.res.Name == clustersRes || a.res.Name == policyRes {
		return nil // resource actions don't apply to these lists
	}
	return a.rowAction(k.String(), row)
}

func (a *App) rowAction(key string, row kube.Row) tea.Cmd {
	r, ns, name := a.res, row.Namespace, row.Name
	switch key {
	case "enter":
		if r.Name == "namespaces" {
			return a.setNamespace(name)
		}
		if r.Name == "customresourcedefinitions" { // open the objects of this CRD
			for _, d := range a.discovered[a.client.Context] {
				if d.Name == name {
					return a.switchResource(d)
				}
			}
			return discover(a.client, "crd:"+name)
		}
		if r.Name == "secrets" {
			return a.openSecret(ns, name)
		}
		return a.openText(r, ns, name, "describe")
	case "d":
		return a.openText(r, ns, name, "describe")
	case "y":
		return a.openText(r, ns, name, "yaml")
	case "h":
		switch {
		case r.Name == "nodes":
			return a.openNodeHealth(name)
		case r.Name == "pods":
			return a.openHealth(ns, name)
		case r.HasSelector():
			return fetchPods(a.client, r, ns, name, "health")
		}
	case "x":
		if r.Name == "secrets" {
			return a.openSecret(ns, name)
		}
	case "a":
		return a.analyze(r, ns, name)
	case "l", "p":
		if r.CanLogs() {
			purpose := "logs"
			if key == "p" {
				purpose = "prevlogs"
			}
			return fetchPods(a.client, r, ns, name, purpose)
		}
	case "L":
		if r.CanLogs() {
			split := a.split(false, nil)
			return tea.Batch(split, fetchPods(a.client, r, ns, name, "logs"))
		}
	case "s":
		if r.CanLogs() {
			return fetchPods(a.client, r, ns, name, "shell")
		}
	case "e":
		return a.openEdit(r, ns, name)
	case "ctrl+d", "D":
		if a.guardWrite() {
			a.askConfirm(fmt.Sprintf("Delete %s %s?", strings.ToLower(r.Kind), objName(ns, name)), name, func() tea.Cmd {
				return runAction(fmt.Sprintf("deleted %s/%s", r.Name, name), func(ctx context.Context) error { return a.client.Delete(ctx, r, ns, name) })
			})
		}
	case "S":
		if r.CanScale() && a.guardWrite() {
			a.openInput(ovInput, fmt.Sprintf("scale %s to replicas: ", name), func(s string) tea.Cmd {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n < 0 {
					a.setStatus("invalid replica count", true)
					return nil
				}
				return runAction(fmt.Sprintf("scaled %s to %d", name, n), func(ctx context.Context) error { return a.client.Scale(ctx, r, ns, name, n) })
			})
		}
	case "F":
		if canForward(r) {
			if direct, _ := kube.ForwardsFor(a.client.Context, r.Name, ns, name); len(direct) > 0 {
				return a.forwardMenu(direct, func() tea.Cmd { return fetchPortTargets(a.client, r, ns, name) })
			}
			return fetchPortTargets(a.client, r, ns, name)
		}
		a.setStatus("port-forward works on pods, services and deployments/statefulsets/daemonsets", true)
	case "u":
		if r.Name == "nodes" {
			return a.toggleCordon(name, strings.Contains(row.Cells[1], "SchedulingDisabled"))
		}
	case "X":
		if r.Name == "nodes" {
			return a.openDrainPreview(name)
		}
	case "o":
		if r.Name == "deployments" {
			return a.openRollout(ns, name)
		}
		a.setStatus("rollout history is available for deployments", true)
	case "R":
		if r.CanRestart() && a.guardWrite() {
			a.askConfirm(fmt.Sprintf("Rollout restart %s %s?", strings.ToLower(r.Kind), objName(ns, name)), "", func() tea.Cmd {
				return runAction("restarted "+name, func(ctx context.Context) error { return a.client.Restart(ctx, r, ns, name) })
			})
		}
	}
	return nil
}
