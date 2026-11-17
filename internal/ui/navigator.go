package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// discoverMsg delivers custom resources and other API resources for one cluster.
// open says what to do afterwards: "" (cache only), "navigator", "cmd:<line>" or "crd:<name>".
type discoverMsg struct {
	ctx  string
	res  []kube.Resource
	err  error
	open string
}

func discover(c *kube.Client, open string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := c.Discover(ctx)
		return discoverMsg{ctx: c.Context, res: res, err: err, open: open}
	}
}

// lookupResource finds a built-in or discovered resource by name, alias, kind or short name.
func (a *App) lookupResource(name string) (kube.Resource, bool) {
	if r, ok := kube.Lookup(name); ok {
		return r, true
	}
	return kube.FindIn(a.discovered[a.client.Context], name)
}

func (a *App) handleDiscover(m discoverMsg) tea.Cmd {
	if m.err == nil || len(m.res) > 0 {
		a.discovered[m.ctx] = m.res
	}
	if m.ctx != a.client.Context {
		return nil
	}
	switch {
	case m.open == "navigator":
		if m.err != nil && len(m.res) == 0 {
			a.setStatus("custom resources unavailable: "+m.err.Error(), true)
		}
		a.showNavigator()
	case strings.HasPrefix(m.open, "cmd:"):
		line := strings.TrimPrefix(m.open, "cmd:")
		fields := strings.Fields(line)
		if r, ok := a.lookupResource(fields[0]); ok {
			if len(fields) > 1 && r.Namespaced {
				a.client.Namespace = fields[1]
				if fields[1] == "all" || fields[1] == "-A" {
					a.client.Namespace = kube.AllNamespaces
				}
			}
			return a.switchResource(r)
		}
		a.setStatus("unknown command or resource: "+fields[0]+"   (0 lists every resource)", true)
	case strings.HasPrefix(m.open, "crd:"):
		name := strings.TrimPrefix(m.open, "crd:")
		for _, r := range a.discovered[m.ctx] {
			if r.Name == name {
				return a.switchResource(r)
			}
		}
		a.setStatus("cannot browse "+name+" (not served or not allowed)", true)
	}
	return nil
}

// openNavigator lists every resource type, grouped, to pick from (key 0 or :resources).
func (a *App) openNavigator() tea.Cmd {
	if _, ok := a.discovered[a.client.Context]; ok {
		a.showNavigator()
		return nil
	}
	a.setStatus("discovering API resources and CRDs on "+a.client.Context+"…", false)
	return discover(a.client, "navigator")
}

func (a *App) showNavigator() {
	all := append([]kube.Resource{}, kube.Resources()...)
	all = append(all, a.discovered[a.client.Context]...)
	order := map[string]int{}
	for i, g := range kube.GroupOrder {
		order[g] = i
	}
	sort.SliceStable(all, func(i, j int) bool {
		gi, gj := order[all[i].Group], order[all[j].Group]
		if gi != gj {
			return gi < gj
		}
		if all[i].Group == "Workloads" || all[i].Group == "Cluster" {
			return false // keep the curated order for built-ins
		}
		return all[i].Kind < all[j].Kind
	})
	rel, charts := a.helmResource(), a.chartsResource()
	rel.Group, charts.Group = "Helm", "Helm"
	rel.Aliases = []string{"helm"}
	insights := []kube.Resource{a.sizingResource(), a.certsResource(), a.imagesResource(), a.storageResource(), a.gitopsResource(),
		a.alertsResource(), a.policyReportsResource(), a.backupsResource(), a.staleResource(), a.scalingResource(), a.secretsHealthResource(),
		a.meshResource(), a.costResource("namespace"), a.runbooksResource(), a.kubeconfigResource(), a.fleetResource(), a.historyResource(),
		a.schedulesResource(), a.approvalsResource(), a.nodePoolsResource(), a.argoRolloutsResource(), a.karpenterResource(),
		a.crossplaneResource(), a.clonesResource(), a.catalogResource(), a.changesResource(2 * time.Hour), a.sloResource()}
	for i, al := range []string{"rightsize", "certs", "images", "storage", "gitops", "alerts", "policyreports", "backups", "restart-needed",
		"autoscaling", "secrets-health", "mesh", "opencost", "runbooks", "kubeconfig", "fleet", "history", "schedules", "approvals", "nodepools",
		"argo-rollouts", "karpenter", "crossplane", "clones", "plugins", "changes", "slo"} {
		insights[i].Group, insights[i].Aliases = "Insights", []string{al}
	}
	all = append(append([]kube.Resource{rel, charts}, insights...), all...)
	labels := make([]string, len(all))
	for i, r := range all {
		alias := ""
		if len(r.Aliases) > 0 {
			alias = ":" + r.Aliases[0]
		}
		scope := ""
		if !r.Namespaced && r.Group != "Helm" && r.Group != "Insights" {
			scope = " (cluster)"
		}
		name := r.Kind + scope
		if r.GVR.Group != "" && (r.Group == "Custom resources" || r.Group == "Other APIs") {
			name += "  · " + r.GVR.Group
		}
		cur := "  "
		if r.Name == a.res.Name {
			cur = "▸ "
		}
		labels[i] = fmt.Sprintf("%s%-22s %-52s %s", cur, r.Group, name, alias)
	}
	title := fmt.Sprintf("All resources on %s (%d) · type to filter · enter open", a.client.Context, len(all))
	a.openPicker(title, labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l == s {
				return a.switchResource(all[i])
			}
		}
		return nil
	})
}
