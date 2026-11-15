package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubesage/internal/kube"
)

func key(a *App, k string) tea.Cmd {
	var msg tea.KeyMsg
	switch k {
	case "ctrl+w":
		msg = tea.KeyMsg{Type: tea.KeyCtrlW}
	case "ctrl+x":
		msg = tea.KeyMsg{Type: tea.KeyCtrlX}
	case "alt+1":
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1"), Alt: true}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	_, cmd := a.Update(msg)
	return cmd
}

func assertFits(t *testing.T, a *App) {
	t.Helper()
	lines := strings.Split(a.View(), "\n")
	if len(lines) > a.h {
		t.Fatalf("view has %d lines, terminal is %d", len(lines), a.h)
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > a.w {
			t.Fatalf("line wider than terminal (%d > %d): %q", w, a.w, ansi.Strip(l))
		}
	}
}

func TestSplitFocusAndIndependentPanes(t *testing.T) {
	a := newTestApp(t)
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	first := a.pane

	run(a, key(a, "|"), 0) // side-by-side split, new pane focused
	if len(a.panes()) != 2 || a.pane == first {
		t.Fatal("split should add and focus a second pane")
	}
	// The new pane gets its own resource and namespace without touching the first.
	run(a, a.runCommand("events"), 0)
	a.setNamespace("kube-system")
	if first.res.Name != "pods" || first.client.Namespace != "shop" || a.client.Namespace != "kube-system" {
		t.Fatalf("panes not independent: first=%s/%s second=%s/%s", first.res.Name, first.client.Namespace, a.res.Name, a.client.Namespace)
	}
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "1 Pods (1) · shop") || !strings.Contains(view, "2 Events") || !strings.Contains(view, "▦ 1:Pods [2:Events]") {
		t.Fatalf("both panes should render:\n%s", view)
	}
	assertFits(t, a)

	key(a, "ctrl+w")
	if a.pane != first {
		t.Fatal("ctrl+w should cycle focus")
	}

	// A list result for the unfocused pane goes to that pane, not the focused one.
	second := a.panes()[1]
	second.listGen++
	a.Update(listMsg{pane: second, gen: second.listGen, table: kube.Table{Columns: []string{"X"}, Rows: []kube.Row{{Name: "ev", Cells: []string{"ev"}}}}})
	if len(second.table.Rows) != 1 || first.table.Rows[0].Name != "api-1" || a.pane != first {
		t.Fatal("pane routing broken")
	}

	key(a, "-") // stacked split of the first pane → 3 panes
	if len(a.panes()) != 3 {
		t.Fatalf("expected 3 panes, got %d", len(a.panes()))
	}
	assertFits(t, a)
	key(a, "z")
	if !a.zoomed || !strings.Contains(ansi.Strip(a.View()), "zoomed") {
		t.Fatal("z should zoom")
	}
	assertFits(t, a)
	key(a, "z")
	key(a, "ctrl+x")
	key(a, "ctrl+x")
	if len(a.panes()) != 1 {
		t.Fatalf("closing should leave one pane, got %d", len(a.panes()))
	}
	key(a, "ctrl+x")
	if !strings.Contains(a.status, "only pane") {
		t.Fatal("cannot close the last pane")
	}
	assertFits(t, a)
}

func TestLogsInSplitPane(t *testing.T) {
	a := newTestApp(t)
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	tablePane := a.pane
	cmd := key(a, "L")
	// Run only the pods lookup part (log streaming needs a real API server).
	if len(a.panes()) != 2 || a.pane == tablePane {
		t.Fatal("L should open a new pane")
	}
	_ = cmd
	a.Update(podsMsg{purpose: "logs", pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}}}}})
	if a.mode != modeText || a.textKind != "logs" || tablePane.mode != modeTable {
		t.Fatalf("logs should open in the new pane only: new=%v old=%v", a.mode, tablePane.mode)
	}
	a.stopLogs()
}

func TestLayoutsRenderAtManySizes(t *testing.T) {
	for _, name := range []string{"triage", "ops", "dash", "ai"} {
		a := newTestApp(t)
		run(a, a.applyLayout(name), 0)
		if len(a.panes()) < 2 {
			t.Fatalf("%s: expected several panes", name)
		}
		for _, sz := range [][2]int{{200, 50}, {120, 30}, {80, 24}, {60, 20}} {
			a.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			assertFits(t, a)
		}
		key(a, "]")
		key(a, "[")
		assertFits(t, a)
		run(a, a.applyLayout("single"), 0)
		if len(a.panes()) != 1 {
			t.Fatalf("%s: single should collapse", name)
		}
	}
}

func TestOpenAIReusesAIPane(t *testing.T) {
	a := newTestApp(t)
	run(a, a.applyLayout("triage"), 0)
	podsPane := a.panes()[0]
	a.focus(podsPane)
	press(a, "A")
	if a.pane == podsPane || a.mode != modeAI {
		t.Fatal("A should focus the existing AI pane instead of replacing the table")
	}
	if podsPane.mode != modeTable {
		t.Fatal("table pane must stay a table")
	}
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "AI assistant") || !strings.Contains(view, "Events") {
		t.Fatalf("triage layout should show table, events and AI:\n%s", view)
	}
}

func TestSplitCommands(t *testing.T) {
	a := newTestApp(t)
	run(a, a.runCommand("vsplit nodes"), 0)
	if a.res.Name != "nodes" || len(a.panes()) != 2 {
		t.Fatalf("vsplit nodes: %s %d", a.res.Name, len(a.panes()))
	}
	run(a, a.runCommand("split ai"), 0)
	if a.mode != modeAI || len(a.panes()) != 3 {
		t.Fatal("split ai should open an AI pane")
	}
	a.runCommand("only")
	if len(a.panes()) != 1 || a.mode != modeAI {
		t.Fatal("only keeps the focused pane")
	}
}

func TestNamespaceForAllPanes(t *testing.T) {
	a := newTestApp(t)
	run(a, a.applyLayout("ops"), 0)
	run(a, a.runCommand("ns! kube-system"), 0)
	for _, p := range a.panes() {
		if p.client.Namespace != "kube-system" {
			t.Fatalf("pane %d still in %q", p.id, p.client.Namespace)
		}
	}
	run(a, a.runCommand("ns default"), 0)
	if a.client.Namespace != "default" || a.panes()[1].client.Namespace != "kube-system" {
		t.Fatal(":ns should only change the focused pane")
	}
}
