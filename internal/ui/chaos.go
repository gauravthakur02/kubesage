package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const chaosWait = 5 * time.Minute

// openChaos deletes one ready pod of the selected workload and measures recovery (:chaos).
// Refused on contexts matching danger_contexts, in read-only mode and outside change windows.
func (a *App) openChaos() tea.Cmd {
	row, ok := a.selected()
	if a.mode != modeTable || !ok {
		a.setStatus("select a deployment, statefulset, replicaset or pod first", true)
		return nil
	}
	switch a.res.Name {
	case "deployments", "statefulsets", "replicasets", "pods":
	default:
		a.setStatus(":chaos works on deployments, statefulsets, replicasets and pods", true)
		return nil
	}
	if a.dangerFor(a.client.Context) {
		a.setStatus("chaos testing is disabled on production-like contexts (danger_contexts)", true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	r, c, ns, name := a.res, a.client, row.Namespace, row.Name
	q := fmt.Sprintf("Chaos test: delete one ready pod of %s %s/%s and wait up to %s for recovery? Type the name to confirm",
		strings.TrimSuffix(r.Name, "s"), ns, name, chaosWait)
	a.askConfirmTyped(q, name, func() tea.Cmd {
		return a.openReport("chaos", "chaos test running: pod deleted, waiting for recovery…", func(context.Context) (string, string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), chaosWait+30*time.Second)
			defer cancel()
			res, err := c.ChaosKillPod(ctx, r, ns, name, chaosWait)
			if res.Killed != "" {
				a.audit("chaos: delete pod "+res.Killed, c.Context, ns, r.Name+"/"+name, err)
			}
			if err != nil {
				return "", "", err
			}
			s := a.st
			lines := []string{s.Title.Render(fmt.Sprintf("Chaos test · %s %s/%s", strings.TrimSuffix(r.Name, "s"), ns, name)), ""}
			for _, st := range res.Steps {
				lines = append(lines, "  "+st)
			}
			lines = append(lines, "")
			if res.Recovered {
				lines = append(lines, s.OKText.Render(fmt.Sprintf("✓ recovered in %s (replacement %s)", res.After.Round(time.Second), res.Replaced)))
				if res.After > time.Minute {
					lines = append(lines, s.Muted.Render("  slow recovery: check image pull time, readiness probe delays and startup work (W on a pod)"))
				}
			} else {
				lines = append(lines, s.ErrText.Render("✗ did not recover within "+chaosWait.String()),
					s.Muted.Render("  check the workload's events (t) and pending pods; capacity or PodDisruptionBudgets may block it"))
			}
			return "chaos test " + ns + "/" + name, strings.Join(lines, "\n"), nil
		})
	})
	return nil
}
