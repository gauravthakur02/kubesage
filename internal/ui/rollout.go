package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"kubesage/internal/kube"
)

const rolloutPrefix = "rollout:"

// rolloutResource lists a Deployment's revisions with its live rollout status.
func (a *App) rolloutResource(ns, name string) kube.Resource {
	c := a.client
	return kube.VirtualResource(rolloutPrefix+ns+"/"+name, "Revision", "Rollout", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"REV", "", "REPLICASET", "IMAGES", "READY", "AGE", "CHANGE-CAUSE"}}
		info, err := c.Rollout(ctx, ns, name)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		t.Note = info.Status
		for _, r := range info.Revisions {
			mark, h := "", kube.HealthNeutral
			if r.Current {
				mark, h = "● current", kube.HealthOK
				if !info.Done {
					h = kube.HealthWarn
				}
			}
			t.Rows = append(t.Rows, kube.Row{Name: strconv.FormatInt(r.Number, 10), Health: h, Cells: []string{
				strconv.FormatInt(r.Number, 10), mark, r.ReplicaSet, kube.ShortImages(r.Images),
				fmt.Sprintf("%d/%d", r.Ready, r.Desired), r.Age, r.ChangeCause}})
		}
		return t
	})
}

func isRollout(r kube.Resource) (ns, name string, ok bool) {
	rest, ok := strings.CutPrefix(r.Name, rolloutPrefix)
	if !ok {
		return "", "", false
	}
	ns, name, _ = strings.Cut(rest, "/")
	return ns, name, true
}

// openRollout shows the revision history of a deployment (esc returns to the deployments list).
func (a *App) openRollout(ns, name string) tea.Cmd {
	back := a.res
	cmd := a.switchResource(a.rolloutResource(ns, name))
	a.backTo = &back
	a.setStatus("enter/d: diff against current · U: roll back to this revision · esc: back", false)
	return cmd
}

// fetchRolloutDiff diffs a revision's pod template against the current one.
func (a *App) fetchRolloutDiff(ns, name string, rev int64) tea.Cmd {
	c := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		info, err := c.Rollout(ctx, ns, name)
		if err != nil {
			return textMsg{err: err}
		}
		var cur, sel *kube.Revision
		for i := range info.Revisions {
			r := &info.Revisions[i]
			if r.Current {
				cur = r
			}
			if r.Number == rev {
				sel = r
			}
		}
		if cur == nil || sel == nil {
			return textMsg{err: fmt.Errorf("revision %d not found", rev)}
		}
		d := diffLines(strings.Split(kube.TemplateYAML(sel.Template), "\n"), strings.Split(kube.TemplateYAML(cur.Template), "\n"), 3)
		add, del := diffStats(d)
		title := fmt.Sprintf("diff deployment/%s: revision %d → current %d · %s (− only in %d, + only in current)", name, sel.Number, cur.Number, diffHeader(add, del), sel.Number)
		return textMsg{title: title, kind: "diff", content: strings.Join(a.renderDiffLines(d), "\n")}
	}
}

// rolloutRowAction handles keys in the rollout history table.
func (a *App) rolloutRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	ns, name, _ := isRollout(a.res)
	rev, err := strconv.ParseInt(row.Name, 10, 64)
	if err != nil {
		return nil, false
	}
	switch key {
	case "enter", "d":
		a.prevMode, a.mode = a.mode, modeText
		a.textKind, a.textTitle, a.textLines = "diff", "loading diff…", nil
		a.renderText(true)
		return tagPane(a.pane, a.fetchRolloutDiff(ns, name, rev)), true
	case "U":
		if !a.guardWrite() {
			return nil, true
		}
		if row.Cells[1] != "" {
			a.setStatus(fmt.Sprintf("revision %d is already the current one", rev), true)
			return nil, true
		}
		a.askConfirm(fmt.Sprintf("Roll back deployment %s/%s to revision %d (%s)?", ns, name, rev, row.Cells[3]), name, func() tea.Cmd {
			return runAction(fmt.Sprintf("rolled back %s to revision %d (a new revision is created)", name, rev), func(ctx context.Context) error {
				return a.client.RollbackTo(ctx, ns, name, rev)
			})
		})
		return nil, true
	}
	return nil, false
}
