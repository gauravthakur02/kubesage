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
	argoRolloutsRes = "argo-rollouts"
	karpenterRes    = "karpenter"
	crossplaneRes   = "crossplane"
)

// ---------- Argo Rollouts ----------

func (a *App) argoRolloutsResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(argoRolloutsRes, "Rollout", "Rollouts", func() kube.Table {
		t := kube.Table{Columns: []string{"NAME", "STRATEGY", "PHASE", "STEP", "READY", "UPDATED", "IMAGE", "MESSAGE"}}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		rs, found, err := c.ArgoRollouts(ctx)
		switch {
		case err != nil:
			t.Note = "✗ " + err.Error()
			return t
		case !found:
			t.Note = "Argo Rollouts is not installed on this cluster"
			return t
		}
		paused := 0
		for _, r := range rs {
			h := kube.HealthOK
			switch r.Phase {
			case "Degraded":
				h = kube.HealthErr
			case "Paused", "Progressing":
				h = kube.HealthWarn
			}
			if r.Aborted {
				h = kube.HealthErr
			}
			if r.Paused {
				paused++
			}
			step := ""
			if r.Steps > 0 {
				step = fmt.Sprintf("%d/%d", r.Step, r.Steps)
			}
			name := r.Name
			if c.Namespace == kube.AllNamespaces {
				name = r.Namespace + "/" + r.Name
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: r.Namespace, Name: r.Name, Health: h, Cells: []string{name, r.Strategy, r.Phase, step,
				fmt.Sprintf("%d/%d", r.Ready, r.Desired), fmt.Sprint(r.Updated), shortImage(r.Image), r.Message}})
		}
		t.Note = fmt.Sprintf("%d rollouts · %d paused · p promote · P promote fully · X abort · T retry", len(rs), paused)
		return t
	})
}

func shortImage(img string) string {
	if i := strings.LastIndex(img, "/"); i >= 0 {
		return img[i+1:]
	}
	return img
}

func (a *App) rolloutAction(row kube.Row, action string) tea.Cmd {
	if !a.guardWrite() {
		return nil
	}
	c := a.client
	a.askConfirm(fmt.Sprintf("%s rollout %s/%s on %s?", action, row.Namespace, row.Name, c.Context), row.Name, func() tea.Cmd {
		return runAction("rollout "+action+" "+row.Namespace+"/"+row.Name, func(ctx context.Context) error {
			return c.RolloutAction(ctx, row.Namespace, row.Name, action)
		})
	})
	return nil
}

// ---------- Karpenter ----------

func (a *App) karpenterResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(karpenterRes, "NodePool", "Karpenter", func() kube.Table {
		t := kube.Table{Columns: []string{"NODEPOOL", "READY", "NODECLAIMS", "INSTANCE TYPES", "USED", "LIMITS", "DISRUPTION"}}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		ps, found, err := c.KarpenterPools(ctx)
		switch {
		case err != nil:
			t.Note = "✗ " + err.Error()
			return t
		case !found:
			t.Note = "Karpenter is not installed on this cluster (or the karpenter.sh/v1 API is not served)"
			return t
		}
		for _, p := range ps {
			h := kube.HealthOK
			if !strings.HasPrefix(p.Ready, "True") || p.ClaimsReady < p.Claims {
				h = kube.HealthWarn
			}
			t.Rows = append(t.Rows, kube.Row{Name: p.Name, Health: h, Cells: []string{p.Name, p.Ready, fmt.Sprintf("%d/%d ready", p.ClaimsReady, p.Claims),
				strings.Join(p.Types, ","), p.Used, p.Limits, p.Disruption}})
		}
		t.Note = fmt.Sprintf("%d NodePools · enter: YAML · e: safe edit limits/disruption", len(ps))
		return t
	})
}

// ---------- Crossplane ----------

func (a *App) crossplaneResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(crossplaneRes, "Managed resource", "Crossplane", func() kube.Table {
		t := kube.Table{Columns: []string{"KIND", "NAME", "READY", "SYNCED", "EXTERNAL NAME", "GROUP", "MESSAGE"}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		ms, found, err := c.CrossplaneResources(ctx, 2000)
		switch {
		case err != nil && len(ms) == 0:
			t.Note = "✗ " + err.Error()
			return t
		case !found:
			t.Note = "no Crossplane managed resources (category \"managed\") on this cluster"
			return t
		}
		bad := 0
		for _, m := range ms {
			h := kube.HealthOK
			if m.Ready != "True" || m.Synced != "True" {
				h, bad = kube.HealthErr, bad+1
			}
			t.Rows = append(t.Rows, kube.Row{Name: m.Kind + "/" + m.Name, Health: h, Cells: []string{m.Kind, m.Name, m.Ready, m.Synced, m.External, m.Group, m.Message}})
		}
		t.Note = fmt.Sprintf("%d managed resources · %d not ready or not synced", len(ms), bad)
		return t
	})
}

// ---------- Hubble ----------

// openHubble shows the latest network flows of the selected pod, or the namespace (:flows).
func (a *App) openHubble() tea.Cmd {
	ns, pod := a.client.Namespace, ""
	if row, ok := a.selected(); ok && a.mode == modeTable && a.res.Name == "pods" {
		ns, pod = row.Namespace, row.Name
	}
	if ns == kube.AllNamespaces {
		a.setStatus("select a pod or a namespace first", true)
		return nil
	}
	return a.openReport("flows", "asking Hubble for flows…", func(context.Context) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		flows, err := kube.HubbleFlows(ctx, ns, pod, 300)
		if err != nil {
			return "", "", err
		}
		s := a.st
		var b strings.Builder
		dropped := 0
		for _, f := range flows {
			v := s.OKText.Render(fmt.Sprintf("%-10s", f.Verdict))
			if f.Verdict == "DROPPED" || f.Verdict == "ERROR" {
				v, dropped = s.ErrText.Bold(true).Render(fmt.Sprintf("%-10s", f.Verdict)), dropped+1
			}
			fmt.Fprintf(&b, "%s %s %s → %s  %s\n", f.Time.Local().Format("15:04:05"), v, f.Source, f.Dest, s.Muted.Render(f.Summary))
		}
		who := ns
		if pod != "" {
			who += "/" + pod
		}
		return fmt.Sprintf("flows %s · %d flows · %d dropped · / search", who, len(flows), dropped), b.String(), nil
	})
}

func init() {
	registerScreen(argoRolloutsRes, screenDef{
		title: func(a *App) string { return "Argo Rollouts · " + a.client.NamespaceLabel() + " · " + a.table.Note },
		hints: []kv{{"p", "promote"}, {"P", "promote fully"}, {"X", "abort"}, {"T", "retry"}, {"enter", "yaml"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			switch key {
			case "p":
				return a.rolloutAction(row, "promote"), true
			case "P":
				return a.rolloutAction(row, "promote-full"), true
			case "X":
				return a.rolloutAction(row, "abort"), true
			case "T":
				return a.rolloutAction(row, "retry"), true
			case "enter", "y":
				r := kube.Resource{Name: "rollouts.argoproj.io", Kind: "Rollout", Namespaced: true, GVR: kube.RolloutGVR}
				return a.openText(r, row.Namespace, row.Name, "yaml"), true
			}
			return nil, false
		},
	})
	registerScreen(karpenterRes, screenDef{
		title: func(a *App) string { return "Karpenter · " + a.table.Note },
		hints: []kv{{"enter", "yaml"}, {"e", "safe edit"}, {"r", "refresh"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			r := kube.Resource{Name: "nodepools.karpenter.sh", Kind: "NodePool", GVR: kube.KarpenterPoolGVR}
			switch key {
			case "enter", "y":
				return a.openText(r, "", row.Name, "yaml"), true
			case "e":
				if !a.guardWrite() {
					return nil, true
				}
				return a.openEdit(r, "", row.Name), true
			}
			return nil, false
		},
	})
	registerScreen(crossplaneRes, screenDef{
		title: func(a *App) string { return "Crossplane · " + a.table.Note },
		hints: []kv{{"/", "filter"}, {"r", "refresh"}, {"esc", "back"}},
	})
}
