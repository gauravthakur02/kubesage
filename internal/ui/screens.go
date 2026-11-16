package ui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// screen describes one of kubesage's own table screens. New screens register here instead of
// adding cases to isKubesageTable, kubesageTitle, kubesageHints and extraRowAction.
type screenDef struct {
	title  func(a *App) string
	hints  []kv
	action func(a *App, key string, row kube.Row) (tea.Cmd, bool)
}

var screens = map[string]screenDef{}

func registerScreen(name string, s screenDef) { screens[name] = s }

func (a *App) currentScreen() (screenDef, bool) {
	s, ok := screens[a.res.Name]
	return s, ok
}
