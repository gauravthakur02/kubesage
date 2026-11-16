package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

const (
	scalingRes    = "autoscaling"
	kubeconfigRes = "kubeconfig"
)

func (a *App) scalingResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(scalingRes, "Autoscaler", "Scaling", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"KIND", "NAMESPACE", "NAME", "TARGET", "REPLICAS", "METRICS (now/target)", "STATUS", "LAST SCALE"}}
		list, err := c.Autoscalers(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		problems := 0
		for _, as := range list {
			reps := "-"
			if as.Kind != "VPA" {
				reps = fmt.Sprintf("%d (%d–%d)", as.Current, as.Min, as.Max)
			}
			if as.H == kube.HealthErr || as.H == kube.HealthWarn {
				problems++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: as.Namespace, Name: as.Target, Health: as.H, Cells: []string{
				as.Kind, as.Namespace, as.Name, as.Target, reps, as.Metrics, as.Status, as.LastScale}})
		}
		t.Note = fmt.Sprintf("%d autoscalers · %d need attention", len(list), problems)
		return t
	})
}

// ---------- kubeconfig manager ----------

func (a *App) kubeconfigResource() kube.Resource {
	kc, store := a.client.Kubeconfig, a.cl
	return kube.LocalResource(kubeconfigRes, "Context", "Contexts", func() kube.Table {
		t := kube.Table{Columns: []string{"", "CONTEXT", "SERVER", "AUTH", "NAMESPACE", "LAST CHECK"}}
		list, err := kube.KubeContexts(kc)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		stale := 0
		for _, ci := range list {
			cur := ""
			if ci.Current {
				cur = "★"
			}
			h, check := kube.HealthNeutral, "not checked (p)"
			info := store.get(ci.Name)
			switch {
			case info.checking:
				check = "checking…"
			case info.err != nil:
				h, check = kube.HealthErr, "✗ "+firstLineStr(info.err.Error(), 50)
				msg := strings.ToLower(info.err.Error())
				if strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "i/o timeout") {
					check, stale = "✗ unreachable: stale?", stale+1
				}
			case !info.at.IsZero():
				h, check = kube.HealthOK, fmt.Sprintf("✓ %s · %d/%d nodes · %s", info.version, info.ready, info.nodes, info.latency.Round(time.Millisecond))
			}
			auth := nonEmptyStr(ci.AuthExec, "token/cert")
			t.Rows = append(t.Rows, kube.Row{Name: ci.Name, Health: h, Cells: []string{cur, ci.Name, ci.Server, auth, ci.Namespace, check}})
		}
		t.Note = fmt.Sprintf("%d contexts", len(list))
		if stale > 0 {
			t.Note += fmt.Sprintf(" · %d look stale", stale)
		}
		return t
	})
}

func (a *App) kubeconfigRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	kc := a.client.Kubeconfig
	switch key {
	case "enter":
		return a.runCommand("tabnew @" + row.Name), true
	case "p":
		return a.probe(row.Name), true
	case "r":
		a.openInput(ovInput, "rename context "+row.Name+" to › ", func(to string) tea.Cmd {
			to = strings.TrimSpace(to)
			if to == "" || to == row.Name {
				return nil
			}
			bak, err := kube.RenameContext(kc, row.Name, to)
			a.audit("kubeconfig.rename", row.Name, "", "→ "+to, err)
			if err != nil {
				a.setStatus("rename failed: "+err.Error(), true)
				return nil
			}
			_ = a.client.ReloadContexts()
			a.setStatus("✓ renamed "+row.Name+" → "+to+" · backup: "+bak, false)
			return a.refresh()
		})
		return nil, true
	case "D":
		if row.Name == a.client.Context {
			a.setStatus("this tab uses "+row.Name+"; switch to another context before deleting it", true)
			return nil, true
		}
		a.askConfirmTyped(fmt.Sprintf("Delete context %s from your kubeconfig? (a backup is written first; unused cluster and user entries go too)", row.Name), row.Name, func() tea.Cmd {
			bak, err := kube.DeleteContext(kc, row.Name)
			a.audit("kubeconfig.delete", row.Name, "", "", err)
			if err != nil {
				a.setStatus("delete failed: "+err.Error(), true)
				return nil
			}
			_ = a.client.ReloadContexts()
			a.setStatus("✓ deleted "+row.Name+" · backup: "+bak, false)
			return a.refresh()
		})
		return nil, true
	case "a":
		return a.addAKSCluster(), true
	}
	return nil, false
}

type aksSubsMsg struct {
	subs []kube.AzureSubscription
	err  error
}
type aksClustersMsg struct {
	clusters []kube.AKSCluster
	err      error
}

// addAKSCluster: pick a subscription, then a cluster, then merge its credentials (az aks get-credentials).
func (a *App) addAKSCluster() tea.Cmd {
	a.setStatus("listing Azure subscriptions (az account list)…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		subs, err := kube.AzureSubscriptions(ctx)
		return aksSubsMsg{subs, err}
	}
}

func (a *App) handleAKSSubs(m aksSubsMsg) tea.Cmd {
	if m.err != nil {
		a.setStatus(m.err.Error()+" (run az login)", true)
		return nil
	}
	labels := make([]string, len(m.subs))
	for i, s := range m.subs {
		labels[i] = s.Name + "  · " + s.ID
	}
	a.openPicker("Azure subscription", labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				sub := m.subs[i].ID
				a.setStatus("listing AKS clusters (az aks list)…", false)
				return func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
					defer cancel()
					cl, err := kube.AKSClusters(ctx, sub)
					return aksClustersMsg{cl, err}
				}
			}
		}
		return nil
	})
	return nil
}

func (a *App) handleAKSClusters(m aksClustersMsg) tea.Cmd {
	if m.err != nil {
		a.setStatus(m.err.Error(), true)
		return nil
	}
	if len(m.clusters) == 0 {
		a.setStatus("no AKS clusters in that subscription (or no access)", true)
		return nil
	}
	have := map[string]bool{}
	for _, c := range a.client.Contexts() {
		have[c] = true
	}
	labels := make([]string, len(m.clusters))
	for i, c := range m.clusters {
		mark := ""
		if have[c.Name] {
			mark = "  (already in kubeconfig)"
		}
		labels[i] = fmt.Sprintf("%-40s %s · %s · %s%s", c.Name, c.ResourceGroup, c.Location, c.Version, mark)
	}
	kc := a.client.Kubeconfig
	a.openPicker("AKS cluster to add", labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l != s {
				continue
			}
			cl := m.clusters[i]
			a.askConfirm(fmt.Sprintf("Add %s (%s) to your kubeconfig with az aks get-credentials?\nA backup of the kubeconfig is written first.", cl.Name, cl.ResourceGroup), "", func() tea.Cmd {
				a.setStatus("az aks get-credentials "+cl.Name+"…", false)
				return func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					bak, err := kube.AKSGetCredentials(ctx, kc, cl)
					status := "added " + cl.Name + " to the kubeconfig"
					if bak != "" {
						status += " (backup " + bak + ")"
					}
					return kubeconfigChangedMsg{status: status, err: err, ctx: cl.Name}
				}
			})
			return nil
		}
		return nil
	})
	return nil
}

type kubeconfigChangedMsg struct {
	status, ctx string
	err         error
}

func (a *App) handleKubeconfigChanged(m kubeconfigChangedMsg) tea.Cmd {
	a.audit("kubeconfig.add", m.ctx, "", "", m.err)
	if m.err != nil {
		a.setStatus(m.err.Error(), true)
		return nil
	}
	_ = a.client.ReloadContexts()
	a.setStatus("✓ "+m.status+" · enter opens it", false)
	if a.res.Name == kubeconfigRes {
		return a.refresh()
	}
	return nil
}
