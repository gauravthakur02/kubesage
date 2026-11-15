package ui

import (
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"kubesage/internal/config"
	"kubesage/internal/kube"
)

// Read-only: KUBESAGE_LIVE_CONTEXT=ctx KUBESAGE_LIVE_NS=ns go test ./internal/ui -run LivePanes -v
func TestLivePanes(t *testing.T) {
	kctx, ns := os.Getenv("KUBESAGE_LIVE_CONTEXT"), os.Getenv("KUBESAGE_LIVE_NS")
	if kctx == "" || ns == "" {
		t.Skip("set KUBESAGE_LIVE_CONTEXT and KUBESAGE_LIVE_NS")
	}
	c, err := kube.New("", kctx, ns)
	if err != nil {
		t.Fatal(err)
	}
	a := New(config.Default(), c, Options{ReadOnly: true})
	a.Update(tea.WindowSizeMsg{Width: 170, Height: 36})
	run(a, a.applyLayout("ops"), 0)
	for _, p := range a.panes() {
		if p.listErr != nil || len(p.table.Rows) == 0 {
			t.Errorf("pane %s: err=%v rows=%d", p.res.Name, p.listErr, len(p.table.Rows))
		}
		t.Logf("pane %d %-12s %3d rows", p.id, p.res.Name, len(p.table.Rows))
	}
	assertFits(t, a)
	if os.Getenv("KUBESAGE_LIVE_SHOW") != "" {
		t.Log("\n" + ansi.Strip(a.View()))
	}
}
