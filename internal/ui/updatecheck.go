package ui

import (
	"context"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/update"
)

type updateMsg struct{ version, url string }

// checkUpdate asks GitHub for the latest release once at startup (release builds only;
// off with update_check: false or KUBEWEAVE_NO_UPDATE_CHECK=1).
func (a *App) checkUpdate() tea.Cmd {
	v := a.opts.Version
	if !update.IsRelease(v) || os.Getenv("KUBEWEAVE_NO_UPDATE_CHECK") != "" || (a.cfg.UpdateCheck != nil && !*a.cfg.UpdateCheck) {
		return nil
	}
	return func() tea.Msg {
		r, err := update.Latest(context.Background())
		if err != nil || !update.Newer(v, r.Version) {
			return nil
		}
		return updateMsg{version: r.Version, url: r.URL}
	}
}
