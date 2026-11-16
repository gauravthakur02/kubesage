package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

func TestWorkspaceTabs(t *testing.T) {
	a := newTestApp(t)
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	key(a, "|") // tab 1 has two panes
	tab1Panes := len(a.panes())

	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	run(a, cmd, 0)
	if len(a.tabs) != 2 || a.tab != 1 || len(a.panes()) != 1 {
		t.Fatalf("ctrl+t should open an empty-layout tab: tabs=%d tab=%d panes=%d", len(a.tabs), a.tab, len(a.panes()))
	}
	run(a, a.runCommand("tabnew kube-system"), 0)
	if a.client.Namespace != "kube-system" || len(a.tabs) != 3 {
		t.Fatalf(":tabnew <ns> should open a tab in that namespace, got %q", a.client.Namespace)
	}
	run(a, a.runCommand("tabname infra"), 0)
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "1 shop·Pods") || !strings.Contains(view, "3 infra") {
		t.Fatalf("tab strip missing:\n%s", strings.Join(strings.Split(view, "\n")[:3], "\n"))
	}
	assertFits(t, a)

	key(a, ">") // wraps to tab 1
	if a.tab != 0 || len(a.panes()) != tab1Panes {
		t.Fatalf("> should go to tab 1 with its %d panes, got tab %d with %d", tab1Panes, a.tab, len(a.panes()))
	}
	key(a, "<")
	if a.tab != 2 {
		t.Fatalf("< should wrap back to tab 3, got %d", a.tab)
	}
	run(a, a.runCommand("tab 2"), 0)
	if a.tab != 1 {
		t.Fatal(":tab 2")
	}
}

func TestBackgroundTabKeepsReceivingResults(t *testing.T) {
	a := newTestApp(t)
	logPane := a.pane
	logPane.mode, logPane.textKind = modeText, "logs"
	logPane.logGen = 7
	run(a, a.newTab(""), 0) // logPane is now in a background tab
	if a.pane == logPane {
		t.Fatal("new tab should focus a new pane")
	}
	a.Update(logLinesMsg{pane: logPane, gen: 7, lines: []string{"hello from background"}})
	if len(logPane.textLines) != 1 || logPane.textLines[0] != "hello from background" {
		t.Fatalf("background pane lost its log lines: %v", logPane.textLines)
	}
	if len(a.textLines) != 0 {
		t.Fatal("lines must not leak into the focused pane")
	}
}

func TestCloseTab(t *testing.T) {
	a := newTestApp(t)
	run(a, a.runCommand("tabnew events"), 0)
	if a.res.Name != "events" {
		t.Fatalf(":tabnew events should open events, got %s", a.res.Name)
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlX}) // last pane in tab → closes tab
	run(a, cmd, 0)
	if len(a.tabs) != 1 || a.res.Name != "pods" {
		t.Fatalf("ctrl+x on a tab's only pane should close the tab: tabs=%d res=%s", len(a.tabs), a.res.Name)
	}
	run(a, a.runCommand("tabclose"), 0)
	if !strings.Contains(a.status, "only tab") {
		t.Fatal("cannot close the last tab")
	}
	_ = kube.AllNamespaces
}
