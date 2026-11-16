package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

const staleRes = "staleconfig"

// staleResource lists workloads running with an outdated ConfigMap or Secret.
func (a *App) staleResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(staleRes, "Stale config", "Restart", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"NAMESPACE", "WORKLOAD", "PODS", "CHANGED", "CHANGED", "PODS STARTED", "USED AS", "ACTION"}}
		rows, err := c.StaleConfigs(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		need := 0
		for _, s := range rows {
			h, action := kube.HealthNeutral, "file updated in place; restart if the app does not reload it"
			if s.NeedsRestart {
				h, action = kube.HealthWarn, "restart needed (R)"
				need++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: s.Namespace, Name: s.Workload, Health: h, Cells: []string{
				s.Namespace, s.Workload, fmt.Sprint(s.Pods), s.Object, kube.Age(metav1Time(s.Changed)) + " ago",
				kube.Age(metav1Time(s.PodStarted)) + " ago", s.UsedAs, action}})
		}
		t.Note = fmt.Sprintf("%d workloads use config changed after their pods started · %d need a restart (env or subPath)", len(rows), need)
		return t
	})
}

func (a *App) staleRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	r, name, ok := policyTarget(row)
	if !ok {
		return nil, false
	}
	switch key {
	case "R":
		if r.Name != "deployments" && r.Name != "statefulsets" && r.Name != "daemonsets" {
			a.setStatus("only deployments, statefulsets and daemonsets can be restarted; delete the pod to recreate it", true)
			return nil, true
		}
		if !a.guardWrite() {
			return nil, true
		}
		c, ns := a.client, row.Namespace
		a.askConfirm(fmt.Sprintf("Rollout restart %s %s/%s on %s to load the new %s?", strings.ToLower(r.Kind), ns, name, c.Context, row.Cells[3]), name, func() tea.Cmd {
			return runAction(fmt.Sprintf("restarted %s %s", strings.ToLower(r.Kind), name), func(ctx context.Context) error { return c.Restart(ctx, r, ns, name) })
		})
		return nil, true
	case "enter", "w":
		return a.openGraph(r.Kind, row.Namespace, name), true
	}
	return nil, false
}

// openStartup explains where a pod's start time went (W on a pod, :startup).
func (a *App) openStartup(ns, pod string) tea.Cmd {
	c := a.client
	pods, _ := kube.Lookup("pods")
	a.target = targetRef{pods, ns, pod}
	return a.openReport("startup", "measuring pod startup…", func(ctx context.Context) (string, string, error) {
		r, err := c.StartupBreakdown(ctx, ns, pod)
		if err != nil {
			return "", "", err
		}
		s := a.st
		var longest time.Duration
		for _, p := range r.Phases {
			longest = max(longest, p.Duration)
		}
		lines := []string{s.Title.Render("Where the time went")}
		for _, p := range r.Phases {
			n := 0
			if longest > 0 {
				n = int(30 * p.Duration / longest)
			}
			st := s.OKText
			if p.Duration == longest && longest > 10*time.Second {
				st = s.WarnText
			}
			lines = append(lines, fmt.Sprintf("  %-22s %s %8s  %s", p.Name, st.Render(strings.Repeat("█", n)+strings.Repeat("░", 30-n)), p.Duration.Round(100*time.Millisecond), s.Muted.Render(p.Detail)))
		}
		state := s.OKText.Render("ready")
		if !r.Ready {
			state = s.WarnText.Render("not ready yet")
		}
		lines = append(lines, "", fmt.Sprintf("  total %s · %s · %d restarts", r.Total.Round(time.Second), state, r.Restarts), "", s.Title.Render("Probes and advice"))
		for _, ad := range r.Advice {
			st := s.Muted
			if strings.HasPrefix(ad, "⚠") {
				st = s.WarnText
			}
			lines = append(lines, "  "+st.Render(ad))
		}
		return fmt.Sprintf("startup of pod %s/%s · %s", ns, pod, r.Total.Round(time.Second)), strings.Join(lines, "\n"), nil
	})
}
