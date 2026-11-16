package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// Key handling of text, pulse, AI and overlay modes.

func (a *App) updateText(k tea.KeyMsg) tea.Cmd {
	if cmd, handled := a.updateEdit(k.String()); handled {
		return cmd
	}
	switch {
	case a.textKind == "drain" && k.String() == "A":
		return a.startDrain()
	case a.textKind == "nodehealth" && k.String() == "u":
		if n := a.target.name; n != "" {
			return a.toggleCordon(n, strings.Contains(strings.Join(a.textLines, " "), "cordoned"))
		}
	case a.textKind == "nodehealth" && k.String() == "X":
		return a.openDrainPreview(a.target.name)
	case a.textKind == "logs" && (k.String() == "J" || k.String() == "i" || k.String() == "x" || k.String() == "ctrl+s"):
		cmd, _ := a.logsKey(k.String())
		return cmd
	case a.textKind == "applyplan" && k.String() == "A":
		return a.confirmApplyPlan()
	case a.textKind == "graph" && k.String() == "t":
		return a.openTimeline(a.target.res.Kind, a.target.ns, a.target.name)
	case a.textKind == "timeline" && k.String() == "r":
		return a.openGraph(a.target.res.Kind, a.target.ns, a.target.name)
	}
	switch k.String() {
	case "/":
		a.openInput(ovInput, "search › ", func(q string) tea.Cmd { a.setSearch(q); return nil })
		a.input.SetValue(a.search)
		a.input.CursorEnd()
		return nil
	case "n":
		a.nextMatch(1)
		return nil
	case "N":
		a.nextMatch(-1)
		return nil
	case "t":
		if a.textKind == "logs" && len(a.logSpec.pods) > 0 {
			return a.pickLogWindow()
		}
	case "T":
		if a.textKind == "logs" && len(a.logSpec.pods) > 0 {
			spec := a.logSpec
			spec.timestamps = !spec.timestamps
			return a.startLogs(spec)
		}
	case "esc", "q":
		if k.String() == "esc" && a.search != "" {
			a.search, a.matches = "", nil
			a.renderText(false)
			a.setStatus("search cleared", false)
			return nil
		}
		a.search, a.matches = "", nil
		a.stopLogs()
		a.reveal, a.secret = false, nil // never leave decoded secret values behind
		a.mode = a.prevMode
		if a.mode == modeText || a.mode == modeAI {
			a.mode = modeTable
		}
		return a.refresh()
	case "f":
		a.follow = !a.follow
		a.renderText(a.follow)
		return nil
	case "w":
		a.nowrap = !a.nowrap
		a.renderText(a.follow && a.textKind == "logs")
		return nil
	case "x":
		if a.textKind == "secret" || (a.textKind == "yaml" && a.target.res.Name == "secrets") {
			return a.toggleReveal()
		}
		if a.textKind == "helmvalues" && a.helmRel != nil {
			what := "Helm values"
			if a.helmShow == "defaults" {
				what = "chart default values"
			}
			if a.reveal {
				a.reveal = false
				a.renderHelmText()
				return nil
			}
			a.askConfirm(fmt.Sprintf("Show unmasked %s of %s/%s on %s?", what, a.helmRel.Namespace, a.helmRel.Name, a.client.Context), a.helmRel.Name, func() tea.Cmd {
				a.reveal, a.revealedAt = true, time.Now()
				a.renderHelmText()
				return nil
			})
			return nil
		}
	case "h":
		if a.target.res.Name == "pods" && a.textKind != "health" {
			return a.openHealth(a.target.ns, a.target.name)
		}
	case "s":
		if a.target.res.Name == "pods" {
			return fetchPods(a.client, a.target.res, a.target.ns, a.target.name, "shell")
		}
	case "l":
		if a.textKind == "health" {
			return fetchPods(a.client, a.target.res, a.target.ns, a.target.name, "logs")
		}
	case "y":
		return tagPane(a.pane, fetchText(a.client, a.target.res, a.target.ns, a.target.name, "yaml", a.reveal))
	case "d":
		return tagPane(a.pane, fetchText(a.client, a.target.res, a.target.ns, a.target.name, "describe", false))
	case "a":
		a.stopLogs()
		return a.analyze(a.target.res, a.target.ns, a.target.name)
	case "g", "home":
		a.vp.GotoTop()
		return nil
	case "G", "end":
		a.vp.GotoBottom()
		return nil
	}
	var cmd tea.Cmd
	a.vp, cmd = a.vp.Update(k)
	if a.textKind == "logs" {
		a.follow = a.vp.AtBottom()
	}
	return cmd
}

func (a *App) updatePulse(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "q":
		a.mode = modeTable
		return a.refresh()
	case "up", "k":
		a.issueCursor = max(0, a.issueCursor-1)
	case "down", "j":
		a.issueCursor = min(len(a.issues)-1, a.issueCursor+1)
	case "r":
		a.pulseBusy = true
		return tagPane(a.pane, fetchPulse(a.client))
	case "f":
		return a.openForwards()
	case "t", "A":
		target, issues := a.client, a.issues // capture before focus moves to the AI pane
		a.openAI()
		a.aiClient = target
		a.ag = agentState{}
		a.aiBusy = true
		a.chat = append(a.chat, chatEntry{role: "user", display: "Triage all detected issues on " + target.Context + "…"})
		return triageForAI(target, issues)
	case "enter", "a":
		if a.issueCursor < len(a.issues) {
			is := a.issues[a.issueCursor]
			if r, ok := kube.Lookup(is.Kind); ok {
				return a.analyze(r, is.Namespace, is.Name)
			}
		}
	case "l":
		if a.issueCursor < len(a.issues) {
			is := a.issues[a.issueCursor]
			if r, ok := kube.Lookup(is.Kind); ok && r.CanLogs() {
				return fetchPods(a.client, r, is.Namespace, is.Name, "logs")
			}
		}
	case "d":
		if a.issueCursor < len(a.issues) {
			is := a.issues[a.issueCursor]
			if r, ok := kube.Lookup(is.Kind); ok {
				return a.openText(r, is.Namespace, is.Name, "describe")
			}
		}
	case ":":
		a.openInput(ovCommand, ":", func(s string) tea.Cmd { return a.runCommand(s) })
	case "?":
		a.ov = ovHelp
	}
	if a.issueCursor < 0 {
		a.issueCursor = 0
	}
	return nil
}

func (a *App) updateAI(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		if a.aiBusy {
			a.stopAI()
			a.chat = append(a.chat, chatEntry{role: "error", display: "stopped"})
			a.renderChat()
			return nil
		}
		a.aiInput.Blur()
		a.mode = a.prevMode
		if a.mode == modeAI {
			a.mode = modeTable
		}
		return nil
	case "enter":
		q := strings.TrimSpace(a.aiInput.Value())
		if q == "" || a.aiBusy {
			return nil
		}
		a.aiInput.SetValue("")
		if strings.HasPrefix(q, ":") {
			return a.runCommand(strings.TrimPrefix(q, ":"))
		}
		a.ag = agentState{} // a new question gets a fresh step budget
		return a.ask(q)
	case "ctrl+r":
		if a.aiBusy {
			return nil
		}
		return a.pickSuggested()
	case "ctrl+n", "ctrl+l":
		a.stopAI()
		a.chat = nil
		a.renderChat()
		return nil
	case "ctrl+o":
		a.pickProfile()
		return nil
	case "pgup", "pgdown", "up", "down", "ctrl+u", "ctrl+d":
		var cmd tea.Cmd
		a.aiVP, cmd = a.aiVP.Update(k)
		return cmd
	}
	var cmd tea.Cmd
	a.aiInput, cmd = a.aiInput.Update(k)
	return cmd
}

func (a *App) updateOverlay(k tea.KeyMsg) tea.Cmd {
	switch a.ov {
	case ovApprove:
		return a.updateApprove(k)
	case ovHelp:
		a.ov = ovNone
		return nil

	case ovConfirm:
		if a.inputHint != "" { // type-to-confirm
			switch k.String() {
			case "esc":
				a.ov = ovNone
				a.setStatus("cancelled", false)
				return nil
			case "enter":
				a.ov = ovNone
				if a.input.Value() == a.inputHint {
					return a.onConfirm()
				}
				a.setStatus("name did not match — cancelled", true)
				return nil
			}
			var cmd tea.Cmd
			a.input, cmd = a.input.Update(k)
			return cmd
		}
		a.ov = ovNone
		if k.String() == "y" || k.String() == "Y" {
			return a.onConfirm()
		}
		a.setStatus("cancelled", false)
		return nil

	case ovPicker:
		items := a.picker.visibleItems()
		switch k.String() {
		case "esc":
			a.ov = ovNone
		case "up", "ctrl+k":
			a.picker.cursor = max(0, a.picker.cursor-1)
		case "down", "ctrl+j":
			a.picker.cursor = min(len(items)-1, a.picker.cursor+1)
		case "enter":
			a.ov = ovNone
			if a.picker.cursor < len(items) && a.picker.cursor >= 0 {
				return a.picker.onSelect(items[a.picker.cursor])
			}
		case "backspace":
			if n := len(a.picker.filter); n > 0 {
				a.picker.filter = a.picker.filter[:n-1]
				a.picker.cursor = 0
			}
		default:
			if k.Type == tea.KeyRunes {
				a.picker.filter += string(k.Runes)
				a.picker.cursor = 0
			}
		}
		return nil

	default: // command, filter, generic input
		switch k.String() {
		case "esc":
			if a.ov == ovFilter {
				a.filter = ""
				a.applyFilter()
			}
			a.ov = ovNone
			a.clearSecretInput()
			return nil
		case "enter":
			kind := a.ov
			a.ov = ovNone
			if kind == ovFilter {
				return nil
			}
			value := a.input.Value()
			a.clearSecretInput() // never keep a typed key around in the input widget
			if a.onInput != nil {
				return a.onInput(value)
			}
			return nil
		}
		var cmd tea.Cmd
		a.input, cmd = a.input.Update(k)
		if a.ov == ovFilter {
			a.filter = a.input.Value()
			a.cursor = 0
			a.applyFilter()
		}
		return cmd
	}
}
