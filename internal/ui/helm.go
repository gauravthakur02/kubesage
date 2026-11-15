package ui

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"sigs.k8s.io/yaml"

	"kubesage/internal/kube"
)

const (
	helmRes         = "helm"
	helmHistoryPref = "helmhistory:"
)

type helmTextMsg struct {
	pane *pane
	rel  *kube.HelmRelease
	kind string // helmvalues or manifest
	err  error
}

func (m helmTextMsg) targetPane() *pane { return m.pane }

func helmStatusHealth(s string) kube.Health {
	switch {
	case s == "deployed":
		return kube.HealthOK
	case s == "failed":
		return kube.HealthErr
	case strings.HasPrefix(s, "pending"), s == "uninstalling":
		return kube.HealthWarn
	}
	return kube.HealthNeutral
}

// helmResource lists Helm releases (latest revision each) in the focused namespace or all.
func (a *App) helmResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(helmRes, "Helm release", "Helm", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"NAME", "NAMESPACE", "REV", "STATUS", "CHART", "APP VERSION", "UPDATED"}}
		rels, err := c.HelmReleases(ctx, c.Namespace)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		for _, r := range rels {
			upd := ""
			if !r.Updated.IsZero() {
				upd = shortAge(r.Updated) + " ago"
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: r.Namespace, Name: r.Name, Health: helmStatusHealth(r.Status), Cells: []string{
				r.Name, r.Namespace, strconv.Itoa(r.Revision), r.Status, r.Chart + "-" + r.ChartVersion, r.AppVersion, upd}})
		}
		t.Note = fmt.Sprintf("%d releases", len(rels))
		return t
	})
}

func shortAge(t time.Time) string { return kube.Age(metav1Time(t)) }

func (a *App) helmHistoryResource(ns, name string) kube.Resource {
	c := a.client
	return kube.VirtualResource(helmHistoryPref+ns+"/"+name, "Helm revision", "History", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"REV", "STATUS", "CHART", "APP VERSION", "UPDATED", "DESCRIPTION"}}
		hist, err := c.HelmHistory(ctx, ns, name)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		for _, r := range hist {
			t.Rows = append(t.Rows, kube.Row{Namespace: ns, Name: strconv.Itoa(r.Revision), Health: helmStatusHealth(r.Status), Cells: []string{
				strconv.Itoa(r.Revision), r.Status, r.Chart + "-" + r.ChartVersion, r.AppVersion, shortAge(r.Updated) + " ago", r.Description}})
		}
		t.Note = fmt.Sprintf("%d revisions", len(hist))
		return t
	})
}

func isHelmHistory(r kube.Resource) (ns, name string, ok bool) {
	rest, ok := strings.CutPrefix(r.Name, helmHistoryPref)
	if !ok {
		return "", "", false
	}
	ns, name, _ = strings.Cut(rest, "/")
	return ns, name, true
}

func (a *App) openHelm() tea.Cmd {
	back := a.res
	cmd := a.switchResource(a.helmResource())
	if back.Name != helmRes {
		a.backTo = &back
	}
	a.setStatus("enter: values · m: manifest · h: history (diff, rollback) · M: charts · esc: back", false)
	return cmd
}

// maskValues hides values under keys that look secret (password, token, key, …).
func maskValues(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if _, isMap := x.(map[string]any); !isMap && kube.IsSensitiveKey(k) && x != nil && fmt.Sprint(x) != "" {
				out[k] = "<masked>"
				continue
			}
			out[k] = maskValues(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = maskValues(x)
		}
		return out
	}
	return v
}

func (a *App) fetchHelmText(ns, name string, rev int, kind string) tea.Cmd {
	c := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hist, err := c.HelmHistory(ctx, ns, name)
		if err != nil {
			return helmTextMsg{err: err}
		}
		for i := range hist {
			if rev == 0 || hist[i].Revision == rev {
				return helmTextMsg{rel: &hist[i], kind: kind}
			}
		}
		return helmTextMsg{err: fmt.Errorf("release %s/%s revision %d not found", ns, name, rev)}
	}
}

func (a *App) openHelmText(ns, name string, rev int, kind string) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind, a.textTitle, a.textLines = modeText, kind, "loading…", nil
	a.reveal, a.helmRel, a.helmShow = false, nil, ""
	a.renderText(true)
	return tagPane(a.pane, a.fetchHelmText(ns, name, rev, kind))
}

func (a *App) handleHelmText(m helmTextMsg) {
	if a.mode != modeText {
		return
	}
	if m.err != nil {
		a.textLines = []string{a.st.ErrText.Render("✗ " + m.err.Error())}
		a.renderText(true)
		return
	}
	a.helmRel = m.rel
	a.renderHelmText()
}

func (a *App) renderHelmText() {
	r := a.helmRel
	if r == nil {
		return
	}
	if a.renderChartText(r) {
		return
	}
	switch a.textKind {
	case "helmvalues":
		v := any(r.Values)
		state := "secret-looking values masked · x to unmask"
		if !a.reveal {
			v = maskValues(v)
		} else {
			state = "UNMASKED · masks again in 60s · x to mask now"
		}
		b, _ := yaml.Marshal(v)
		if len(r.Values) == 0 {
			b = []byte("# no user-supplied values (chart defaults only)\n")
		}
		a.textTitle = fmt.Sprintf("helm values %s/%s rev %d · %s", r.Namespace, r.Name, r.Revision, state)
		a.textLines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	default:
		a.textTitle = fmt.Sprintf("helm manifest %s/%s rev %d · %s %s", r.Namespace, r.Name, r.Revision, r.Chart, r.ChartVersion)
		a.textLines = strings.Split(strings.TrimRight(r.Manifest, "\n"), "\n")
	}
	a.renderText(true)
}

// helmDiff compares a revision with the latest one (values and manifest).
func (a *App) helmDiff(ns, name string, rev int) tea.Cmd {
	c := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		hist, err := c.HelmHistory(ctx, ns, name)
		if err != nil || len(hist) == 0 {
			return textMsg{err: fmt.Errorf("history: %v", err)}
		}
		var old *kube.HelmRelease
		for i := range hist {
			if hist[i].Revision == rev {
				old = &hist[i]
			}
		}
		cur := &hist[0]
		if old == nil {
			return textMsg{err: fmt.Errorf("revision %d not found", rev)}
		}
		ov, _ := yaml.Marshal(maskValues(old.Values))
		cv, _ := yaml.Marshal(maskValues(cur.Values))
		vd := diffLines(strings.Split(string(ov), "\n"), strings.Split(string(cv), "\n"), 2)
		md := diffLines(strings.Split(old.Manifest, "\n"), strings.Split(cur.Manifest, "\n"), 2)
		va, vdl := diffStats(vd)
		ma, mdl := diffStats(md)
		lines := []string{a.st.Title.Render(fmt.Sprintf("Values  %s", diffHeader(va, vdl)))}
		lines = append(lines, a.renderDiffLines(vd)...)
		lines = append(lines, "", a.st.Title.Render(fmt.Sprintf("Manifest  %s", diffHeader(ma, mdl))))
		lines = append(lines, a.renderDiffLines(md)...)
		title := fmt.Sprintf("helm diff %s/%s: revision %d (%s) → current %d (%s)", ns, name, old.Revision, old.ChartVersion, cur.Revision, cur.ChartVersion)
		return textMsg{title: title, kind: "diff", content: strings.Join(lines, "\n")}
	}
}

func (a *App) helmRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	switch key {
	case "enter", "v":
		return a.openHelmText(row.Namespace, row.Name, 0, "helmvalues"), true
	case "m":
		return a.openHelmText(row.Namespace, row.Name, 0, "manifest"), true
	case "h":
		back := a.res
		cmd := a.switchResource(a.helmHistoryResource(row.Namespace, row.Name))
		a.backTo = &back
		a.setStatus("enter/d: diff against current · U: helm rollback to this revision · v: values · esc: back", false)
		return cmd, true
	}
	return nil, false
}

func (a *App) helmHistoryRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	ns, name, _ := isHelmHistory(a.res)
	rev, err := strconv.Atoi(row.Name)
	if err != nil {
		return nil, false
	}
	switch key {
	case "enter", "d":
		a.prevMode, a.mode = a.mode, modeText
		a.textKind, a.textTitle, a.textLines = "diff", "loading diff…", nil
		a.renderText(true)
		return tagPane(a.pane, a.helmDiff(ns, name, rev)), true
	case "v":
		return a.openHelmText(ns, name, rev, "helmvalues"), true
	case "U":
		if !a.guardWrite() {
			return nil, true
		}
		if a.cursor == 0 {
			a.setStatus(fmt.Sprintf("revision %d is the current one", rev), true)
			return nil, true
		}
		if _, err := exec.LookPath("helm"); err != nil {
			a.setStatus(fmt.Sprintf("rollback needs the helm CLI: helm rollback %s %d -n %s --kube-context %s", name, rev, ns, a.client.Context), true)
			return nil, true
		}
		a.askConfirm(fmt.Sprintf("helm rollback %s to revision %d in %s on %s? (creates a new revision)", name, rev, ns, a.client.Context), name, func() tea.Cmd {
			args := []string{"rollback", name, strconv.Itoa(rev), "-n", ns, "--kube-context", a.client.Context, "--wait", "--timeout", "5m"}
			if a.client.Kubeconfig != "" {
				args = append(args, "--kubeconfig", a.client.Kubeconfig)
			}
			return runAction(fmt.Sprintf("helm rollback %s to revision %d", name, rev), func(ctx context.Context) error {
				out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
				if err != nil {
					return fmt.Errorf("%s", strings.TrimSpace(string(out)))
				}
				return nil
			})
		})
		return nil, true
	}
	return nil, false
}
