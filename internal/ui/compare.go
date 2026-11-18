package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// compareCommand: `:diff @context` compares the selected object with the same object on another
// cluster; `:diff @context all` compares every object of the current type in the namespace.
func (a *App) compareCommand(arg string) tea.Cmd {
	fields := strings.Fields(arg)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "@") {
		a.setStatus("usage: :diff @<context> (selected object) · :diff @<context> all (whole namespace)", true)
		return nil
	}
	other := strings.TrimPrefix(fields[0], "@")
	found := false
	for _, c := range a.client.Contexts() {
		if c == other {
			found = true
		}
	}
	if !found {
		a.setStatus("unknown context "+other, true)
		return nil
	}
	if other == a.client.Context {
		a.setStatus("pick another context than the current one", true)
		return nil
	}
	r := a.res
	if !r.Watchable() { // not a real API kind
		a.setStatus("cluster diff works on Kubernetes resources (pods, deployments, configmaps, …)", true)
		return nil
	}
	ns := a.client.Namespace
	whole := len(fields) > 1 && fields[1] == "all"
	row, ok := a.selected()
	if !whole && !ok {
		whole = true
	}
	if r.Namespaced && ns == kube.AllNamespaces {
		if !ok {
			a.setStatus("pick a namespace first (:ns <name>)", true)
			return nil
		}
		ns = row.Namespace
	}
	here, connect := a.client, a.connect(other, ns, "probe")
	if whole {
		return a.openReport("diff", "comparing with "+other+"…", func(ctx context.Context) (string, string, error) {
			m := connect().(connectMsg)
			if m.err != nil {
				return "", "", m.err
			}
			entries, err := kube.CompareNamespace(ctx, here, m.client, r, ns)
			if err != nil {
				return "", "", err
			}
			counts := map[string]int{}
			s := a.st
			var lines []string
			label := map[string]string{"differs": s.WarnText.Render("≠ differs   "), "only-here": s.ErrText.Render("◀ only here "),
				"only-there": s.ErrText.Render("▶ only there"), "same": s.OKText.Render("= same      ")}
			for _, e := range entries {
				counts[e.State]++
				lines = append(lines, label[e.State]+"  "+e.Name)
			}
			lines = append(lines, "", s.Muted.Render(fmt.Sprintf("◀ here = %s · ▶ there = %s · status, ids and generated fields ignored · secrets compared by hash", here.Context, other)),
				s.Muted.Render("select an object in the table and run :diff @"+other+" to see the exact difference"))
			title := fmt.Sprintf("%s in %s: %s ⇄ %s · %d differ · %d only here · %d only there · %d same", strings.ToLower(r.Kind)+"s", ns,
				here.Context, other, counts["differs"], counts["only-here"], counts["only-there"], counts["same"])
			return title, strings.Join(lines, "\n"), nil
		})
	}
	name := row.Name
	return a.openReport("diff", "comparing with "+other+"…", func(ctx context.Context) (string, string, error) {
		m := connect().(connectMsg)
		if m.err != nil {
			return "", "", m.err
		}
		get := func(c *kube.Client) (string, error) {
			obj, err := c.Get(ctx, r, ns, name)
			if err != nil {
				return "", fmt.Errorf("%s: %w", c.Context, err)
			}
			return kube.Comparable(obj), nil
		}
		ya, err := get(here)
		if err != nil {
			return "", "", err
		}
		yb, err := get(m.client)
		if err != nil {
			return "", "", err
		}
		d := diffLines(strings.Split(ya, "\n"), strings.Split(yb, "\n"), 3)
		add, del := diffStats(d)
		lines := a.renderDiffLines(d)
		if add+del == 0 {
			lines = []string{a.st.OKText.Render("✓ identical (ignoring status, ids, timestamps and generated fields)")}
		}
		lines = append(lines, "", a.st.Muted.Render(fmt.Sprintf("− %s · + %s", here.Context, other)))
		return fmt.Sprintf("diff %s %s/%s: %s → %s · %s", strings.ToLower(r.Kind), ns, name, here.Context, other, diffHeader(add, del)), strings.Join(lines, "\n"), nil
	})
}
