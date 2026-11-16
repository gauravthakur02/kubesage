package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

type applyPlanMsg struct {
	pane  *pane
	path  string
	how   string
	items []kube.ApplyItem
	err   error
}

func (m applyPlanMsg) targetPane() *pane { return m.pane }

// openApplyPlan renders local manifests, dry-runs them on the server and shows the diff (:apply <path>).
func (a *App) openApplyPlan(path string) tea.Cmd {
	if strings.TrimSpace(path) == "" {
		a.setStatus("usage: :apply <file | folder | kustomization | chart>   (shows a server-side dry-run diff first)", true)
		return nil
	}
	c := a.client
	ns := c.Namespace
	if ns == kube.AllNamespaces {
		ns = "default"
	}
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind, a.textTitle, a.textLines, a.applyPlan = modeText, "applyplan", "rendering and dry-running "+path+"…", nil, nil
	a.renderText(true)
	return tagPane(a.pane, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*timeout)
		defer cancel()
		y, how, err := kube.RenderLocal(ctx, path)
		if err != nil {
			return applyPlanMsg{path: path, err: err}
		}
		objs, err := kube.ParseManifests(y)
		if err != nil {
			return applyPlanMsg{path: path, err: err}
		}
		items, err := c.PlanApply(ctx, objs, ns)
		return applyPlanMsg{path: path, how: how, items: items, err: err}
	})
}

func (a *App) handleApplyPlan(m applyPlanMsg) {
	s := a.st
	if m.err != nil {
		a.textTitle, a.textLines = "✗ apply "+m.path, []string{s.ErrText.Render(m.err.Error())}
		a.renderText(true)
		return
	}
	a.applyPlan = &m
	counts := map[string]int{}
	var lines []string
	for _, it := range m.items {
		counts[it.State]++
	}
	for _, it := range m.items {
		obj := fmt.Sprintf("%s %s", it.Kind, objName(it.Namespace, it.Name))
		switch it.State {
		case "new":
			lines = append(lines, s.OKText.Render("+ create  ")+obj)
		case "changed":
			lines = append(lines, s.WarnText.Render("~ change  ")+obj)
			d := diffLines(strings.Split(it.Live, "\n"), strings.Split(it.Result, "\n"), 2)
			for _, l := range a.renderDiffLines(d) {
				lines = append(lines, "    "+l)
			}
		case "unchanged":
			lines = append(lines, s.Muted.Render("= same    "+obj))
		default:
			lines = append(lines, s.ErrText.Render("✗ error   ")+obj+"  "+s.ErrText.Render(firstLineStr(fmt.Sprint(it.Err), 160)))
		}
	}
	lines = append(lines, "")
	if counts["new"]+counts["changed"] > 0 && counts["error"] == 0 {
		lines = append(lines, s.OKText.Bold(true).Render(fmt.Sprintf("Server-side dry-run passed. A: apply %d object(s) to %s · esc: cancel", counts["new"]+counts["changed"], a.client.Context)))
	} else if counts["error"] > 0 {
		lines = append(lines, s.ErrText.Render("Fix the errors first; nothing will be applied while any object fails the dry-run."))
	} else {
		lines = append(lines, s.OKText.Render("✓ the cluster already matches these manifests"))
	}
	a.textTitle = fmt.Sprintf("apply %s (%s) → %s · %d new · %d changed · %d same · %d errors", m.path, m.how, a.client.Context,
		counts["new"], counts["changed"], counts["unchanged"], counts["error"])
	a.textLines = lines
	a.renderText(false)
	a.vp.GotoTop()
}

// confirmApplyPlan applies the planned objects after confirmation (A in the plan view).
func (a *App) confirmApplyPlan() tea.Cmd {
	p := a.applyPlan
	if p == nil {
		return nil
	}
	n, bad := 0, 0
	for _, it := range p.items {
		switch it.State {
		case "new", "changed":
			n++
		case "error":
			bad++
		}
	}
	if n == 0 || bad > 0 {
		a.setStatus("nothing to apply (or the dry-run has errors)", true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	c, items := a.client, p.items
	a.askConfirm(fmt.Sprintf("Apply %d object(s) from %s to %s (server-side apply)?", n, p.path, c.Context), c.Context, func() tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 3*timeout)
			defer cancel()
			applied, errs := c.ExecuteApply(ctx, items)
			var err error
			if len(errs) > 0 {
				err = fmt.Errorf("%s", strings.Join(errs, "; "))
			}
			return actionMsg{status: fmt.Sprintf("applied %d object(s) from %s", applied, p.path), err: err}
		}
	})
	return nil
}
