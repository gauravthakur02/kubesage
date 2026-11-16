package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

func kubeStrip(s string) string { return ansi.Strip(s) }

// paletteItems are the kubesage screens and commands reachable from ctrl+p.
var paletteItems = []struct{ label, cmd string }{
	{"Helm releases", "helm"}, {"Helm charts in use", "charts"}, {"Pulse: cluster health", "pulse"},
	{"Clusters overview", "clusters"}, {"Policy & security checks", "policy"}, {"AI incident triage", "incident"},
	{"Right-sizing & cost", "rightsize"}, {"TLS certificates & expiry", "certs"}, {"Images in use (Trivy scan)", "images"},
	{"Storage: PVC usage", "storage"}, {"GitOps: Argo CD & Flux apps", "gitops"}, {"Audit log of changes", "audit"},
	{"AI chat", "ai"}, {"Saved views", "view"}, {"Export table to CSV", "export csv"}, {"Export table to Markdown", "export md"},
	{"Toggle live updates (watch/poll)", "live"}, {"Upgrade readiness (deprecated APIs)", "upgrade"},
	{"My permissions here (RBAC)", "rbac"}, {"Why was I forbidden?", "why"}, {"Alerts from the watch list", "alerts"},
	{"Watch this namespace (alerts)", "pin ns"}, {"Quotas & node pool capacity", "quota"}, {"Policy reports: Kyverno, Gatekeeper, PSA", "policyreports"},
	{"Velero backups", "backups"}, {"Search all connected clusters", "find "}, {"Incident report of the selected object", "report"},
	{"PromQL query", "prom "}, {"Restart needed: outdated ConfigMaps/Secrets", "restart-needed"},
	{"Autoscaling: HPA, KEDA, VPA", "autoscaling"}, {"Kubeconfig: contexts, add AKS cluster", "kubeconfig"},
	{"Team runbooks", "runbooks"}, {"Secrets health & rotation", "secrets-health"}, {"Service mesh sidecars", "mesh"},
	{"Cost per namespace (OpenCost)", "opencost"}, {"Apply local manifests (dry-run diff)", "apply "},
	{"Port-forward profiles", "fwd"},
	{"Fleet: health of every cluster", "fleet"}, {"Event history (kept locally, beyond 1h)", "history"},
	{"Scale-to-zero schedules", "schedules"}, {"Chaos test the selected workload", "chaos"},
	{"Share this screen read-only (web)", "share"}, {"Stop sharing", "share stop"}, {"Daily report (Slack/Teams)", "daily-report"}, {"Show/hide columns", "cols"}, {"Toggle mouse", "mouse"}, {"Help", "help"},
}

// openPalette fuzzy-searches resources, kubesage screens, saved views, contexts and plugins (ctrl+p).
func (a *App) openPalette() tea.Cmd {
	type item struct {
		label string
		run   func() tea.Cmd
	}
	var items []item
	for _, p := range paletteItems {
		cmd := p.cmd
		items = append(items, item{fmt.Sprintf("▸ %-36s :%s", p.label, strings.TrimSpace(cmd)), func() tea.Cmd {
			if strings.HasSuffix(cmd, " ") { // needs an argument: open the command line prefilled
				a.openInput(ovCommand, ":", func(s string) tea.Cmd { return a.runCommand(s) })
				a.input.SetValue(cmd)
				a.input.CursorEnd()
				return nil
			}
			return a.runCommand(cmd)
		}})
	}
	views := make([]string, 0, len(a.cfg.Views))
	for n := range a.cfg.Views {
		views = append(views, n)
	}
	sort.Strings(views)
	for _, v := range views {
		v := v
		items = append(items, item{fmt.Sprintf("★ view  %-30s :view %s", v, v), func() tea.Cmd { return a.runCommand("view " + v) }})
	}
	all := append([]kube.Resource{}, kube.Resources()...)
	all = append(all, a.discovered[a.client.Context]...)
	for _, r := range all {
		r := r
		alias := r.Name
		if len(r.Aliases) > 0 {
			alias = r.Aliases[0]
		}
		items = append(items, item{fmt.Sprintf("☰ %-36s :%s", r.Kind+"  ("+r.Group+")", alias), func() tea.Cmd { return a.switchResource(r) }})
	}
	for _, p := range a.cfg.Plugins {
		p := p
		items = append(items, item{fmt.Sprintf("⚡ plugin %-29s %s", p.Name, p.Key), func() tea.Cmd {
			if row, ok := a.selected(); ok {
				return a.runPlugin(p, row)
			}
			a.setStatus("select a row first", true)
			return nil
		}})
	}
	for _, c := range a.client.Contexts() {
		c := c
		items = append(items, item{fmt.Sprintf("⎈ context %-28s :tabnew @%s", c, c), func() tea.Cmd { return a.runCommand("tabnew @" + c) }})
	}
	labels := make([]string, len(items))
	for i, it := range items {
		labels[i] = it.label
	}
	a.openPicker("Command palette · type to search screens, resources, views, plugins, clusters", labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return items[i].run()
			}
		}
		return nil
	})
	return nil
}

// ---------- plugins ----------

func pluginApplies(p config.Plugin, res string) bool {
	if len(p.Scopes) == 0 {
		return true
	}
	for _, s := range p.Scopes {
		if s == "all" || s == res {
			return true
		}
		if r, ok := kube.Lookup(s); ok && r.Name == res {
			return true
		}
	}
	return false
}

// pluginFor finds a plugin bound to key for the current resource.
func (a *App) pluginFor(key string) (config.Plugin, bool) {
	for _, p := range a.cfg.Plugins {
		if p.Key == key && pluginApplies(p, a.res.Name) {
			return p, true
		}
	}
	return config.Plugin{}, false
}

func (a *App) pluginEnv(row kube.Row) map[string]string {
	return map[string]string{"NAME": row.Name, "NAMESPACE": row.Namespace, "CONTEXT": a.client.Context, "KIND": a.res.Kind,
		"RESOURCE": a.res.Name, "KUBECONFIG": a.client.Kubeconfig}
}

// runPlugin runs a user plugin on the selected row (after confirmation unless confirm: false).
func (a *App) runPlugin(p config.Plugin, row kube.Row) tea.Cmd {
	env := a.pluginEnv(row)
	expand := func(s string) string { return os.Expand(s, func(k string) string { return env[k] }) }
	args := make([]string, len(p.Args))
	for i, x := range p.Args {
		args[i] = expand(x)
	}
	display := strings.TrimSpace(p.Command + " " + strings.Join(args, " "))
	run := func() tea.Cmd {
		a.audit("plugin "+p.Name, a.client.Context, row.Namespace, display, nil)
		cmd := exec.Command(p.Command, args...)
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		if p.Background {
			title := "plugin " + p.Name + " · " + display
			return a.openReport("plugin", "running "+display+"…", func(_ context.Context) (string, string, error) {
				out, err := cmd.CombinedOutput()
				if err != nil && len(out) == 0 {
					return "", "", err
				}
				return title, string(out), nil
			})
		}
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err: err} })
	}
	if p.Confirm != nil && !*p.Confirm {
		return run()
	}
	a.askConfirm(fmt.Sprintf("Run plugin %q on %s?\n  %s", p.Name, a.client.Context, display), "", run)
	return nil
}
