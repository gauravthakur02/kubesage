package ui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
	"github.com/gauravthakur02/kubesage/internal/report"
	"github.com/gauravthakur02/kubesage/internal/share"
)

// shareCommand starts or stops the read-only web view of this screen.
//
//	:share            on this computer only (127.0.0.1)
//	:share lan [port] on the local network (anyone with the link can watch)
//	:share stop
func (a *App) shareCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	if len(f) > 0 && (f[0] == "stop" || f[0] == "off") {
		if a.sharing == nil {
			a.setStatus("not sharing", false)
			return nil
		}
		_ = a.sharing.Stop()
		a.sharing = nil
		a.setStatus("✓ sharing stopped", false)
		return nil
	}
	if a.sharing != nil {
		a.setStatus(fmt.Sprintf("sharing at %s · %d fetches · :share stop ends it", a.sharing.URL, a.sharing.Viewers()), false)
		return nil
	}
	addr := "127.0.0.1:0"
	if len(f) > 0 && f[0] == "lan" {
		port := "0"
		if len(f) > 1 {
			port = f[1]
		}
		addr = "0.0.0.0:" + port
	}
	s, err := share.Start(addr)
	if err != nil {
		a.setStatus("share: "+err.Error(), true)
		return nil
	}
	a.sharing = s
	a.audit("share screen (read-only) at "+strings.Split(s.URL, "?")[0], a.client.Context, a.client.Namespace, "", nil)
	a.setStatus("● sharing read-only: "+s.URL+" · pauses while a secret is revealed · :share stop", false)
	return tea.SetWindowTitle("kubesage ● sharing")
}

// openDailyReport shows the summary `kubesage report` posts (:daily-report, :daily-report post).
func (a *App) openDailyReport(arg string) tea.Cmd {
	c := a.client
	post := strings.TrimSpace(arg) == "post"
	env := a.cfg.Reports.WebhookEnv
	build := func() tea.Cmd {
		return a.openReport("daily-report", "building report…", func(context.Context) (string, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			text := report.Build(ctx, []*kube.Client{c}, report.Options{}, time.Now())
			title := "report " + c.Context + " · :daily-report post sends it to the webhook"
			if post {
				url := os.Getenv(env)
				if env == "" || url == "" {
					return "", "", fmt.Errorf("set reports.webhook_env in the config and export that variable with the webhook URL")
				}
				if err := report.Post(ctx, url, text); err != nil {
					return "", "", err
				}
				title = "report " + c.Context + " · ✓ posted to $" + env
			}
			return title, text, nil
		})
	}
	if post {
		a.askConfirm("Post this cluster's summary to the webhook in $"+env+"? (y/n)", "", build)
		return nil
	}
	return build()
}
