package ui

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSaveAndOpenView(t *testing.T) {
	a := newMultiApp(t)
	run(a, a.runCommand("deploy"), 0)
	run(a, a.runCommand("vsplit @aks-prod kube-system"), 0)
	run(a, a.runCommand("events"), 0)
	key(a, "-") // stacked split of the prod pane
	run(a, a.runCommand("split ai"), 0)
	run(a, a.runCommand("view save oncall"), 0)
	if !strings.Contains(a.status, `view "oncall" saved`) {
		t.Fatalf("status = %q", a.status)
	}
	data, err := readFile(a.cfg.Path())
	if err != nil || !strings.Contains(data, "oncall:") || !strings.Contains(data, "context: aks-prod") || !strings.Contains(data, "resource: events") {
		t.Fatalf("view not persisted:\n%s %v", data, err)
	}
	want := len(a.panes())

	// Back to a single pane, then reopen it in a new tab via b.
	run(a, a.applyLayout("single"), 0)
	press(a, "esc") // leave the AI chat (it keeps typed keys) for the table
	press(a, "b")
	if a.ov != ovPicker || !strings.HasPrefix(a.picker.items[0], "oncall") {
		t.Fatalf("view picker: %v", a.picker.items)
	}
	press(a, "enter")
	if len(a.tabs) != 2 || len(a.panes()) != want {
		t.Fatalf("view should open in a new tab with %d panes, got tabs=%d panes=%d", want, len(a.tabs), len(a.panes()))
	}
	ctxs, ress := map[string]bool{}, map[string]bool{}
	for _, p := range a.panes() {
		ctxs[p.client.Context+"/"+p.client.Namespace] = true
		ress[p.res.Name] = true
	}
	if !ctxs["aks-staging/shop"] || !ctxs["aks-prod/kube-system"] || !ress["deployments"] || !ress["events"] {
		t.Fatalf("restored panes: %v %v", ctxs, ress)
	}
	if v := ansi.Strip(a.View()); !strings.Contains(v, "oncall") {
		t.Fatal("the tab should be named after the view")
	}
	run(a, a.runCommand("view rm oncall"), 0)
	if _, ok := a.cfg.Views["oncall"]; ok {
		t.Fatal("view rm")
	}
}

func readFile(p string) (string, error) {
	b, err := os.ReadFile(p)
	return string(b), err
}
