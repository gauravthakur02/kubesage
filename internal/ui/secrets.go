package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

const (
	secretsHealthRes = "secretshealth"
	meshRes          = "mesh"
	costRes          = "opencost"
)

func (a *App) secretsHealthResource() kube.Resource {
	c, days := a.client, a.cfg.SecretRotationDays
	return kube.VirtualResource(secretsHealthRes, "Secret", "Secrets", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"KIND", "NAMESPACE", "NAME", "TYPE", "CHANGED", "USED BY", "STATUS"}}
		list, err := c.SecretsHealth(ctx, days)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		bad, old := 0, 0
		for _, s := range list {
			changed := "-"
			if !s.Changed.IsZero() {
				changed = kube.Age(metav1Time(s.Changed)) + " ago"
			}
			used := "-"
			if s.Kind == "Secret" {
				used = fmt.Sprintf("%d pod(s)", s.UsedBy)
			}
			switch s.H {
			case kube.HealthErr:
				bad++
			case kube.HealthWarn:
				old++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: s.Namespace, Name: s.Kind + "/" + s.Name, Health: s.H, Cells: []string{s.Kind, s.Namespace, s.Name, s.Type, changed, used, s.Status}})
		}
		t.Note = fmt.Sprintf("%d items · %d not syncing · %d due for rotation · values are never read here", len(list), bad, old)
		return t
	})
}

func (a *App) meshResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(meshRes, "Mesh namespace", "Mesh", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"NAMESPACE", "MESH", "INJECTION", "PODS", "WITH SIDECAR", "SIDECAR NOT READY"}}
		list, err := c.MeshStatus(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		meshed := 0
		for _, m := range list {
			if m.Mesh != "" {
				meshed++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: m.Namespace, Name: m.Namespace, Health: m.H, Cells: []string{
				m.Namespace, nonEmptyStr(m.Mesh, "-"), m.Injection, fmt.Sprint(m.Pods), fmt.Sprint(m.WithSidecar), fmt.Sprint(m.NotReady)}})
		}
		t.Note = fmt.Sprintf("%d namespaces · %d in a mesh · yellow: injection on but some pods have no sidecar (restart them)", len(list), meshed)
		return t
	})
}

func (a *App) costResource(aggregate string) kube.Resource {
	c, oc := a.client, a.cfg.OpenCost
	window := nonEmptyStr(oc.Window, "7d")
	return kube.VirtualResource(costRes, "Cost", "Cost", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{strings.ToUpper(aggregate), "CPU", "RAM", "STORAGE", "NETWORK", "TOTAL", "EFFICIENCY"}}
		lines, source, err := c.Costs(ctx, oc.URL, window, aggregate)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		var total float64
		money := func(f float64) string { return fmt.Sprintf("%.2f", f) }
		for _, l := range lines {
			total += l.Total
			h := kube.HealthOK
			if l.Eff > 0 && l.Eff < 0.3 {
				h = kube.HealthWarn
			}
			t.Rows = append(t.Rows, kube.Row{Name: l.Name, Health: h, Cells: []string{l.Name, money(l.CPU), money(l.RAM), money(l.PV), money(l.Network), money(l.Total), fmt.Sprintf("%.0f%%", l.Eff*100)}})
		}
		t.Note = fmt.Sprintf("last %s · total %.2f · source %s · yellow: under 30%% efficient (see :rightsize)", window, total, source)
		return t
	})
}
