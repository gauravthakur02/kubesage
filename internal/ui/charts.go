package ui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"sigs.k8s.io/yaml"

	"kubesage/internal/kube"
)

const chartsRes = "helmcharts"

// chartsResource groups the deployed Helm releases by chart.
func (a *App) chartsResource() kube.Resource {
	c := a.client
	r := kube.VirtualResource(chartsRes, "Helm chart", "Charts", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"CHART", "VERSIONS", "APP VERSIONS", "RELEASES", "NAMESPACES", "STATUS", "DESCRIPTION"}}
		rels, err := c.HelmReleases(ctx, c.Namespace)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		type agg struct {
			versions, apps, nss map[string]bool
			releases            []kube.HelmRelease
			failed, pending     int
			desc                string
		}
		by := map[string]*agg{}
		for _, rel := range rels {
			g := by[rel.Chart]
			if g == nil {
				g = &agg{versions: map[string]bool{}, apps: map[string]bool{}, nss: map[string]bool{}}
				by[rel.Chart] = g
			}
			g.versions[rel.ChartVersion] = true
			if rel.AppVersion != "" {
				g.apps[rel.AppVersion] = true
			}
			g.nss[rel.Namespace] = true
			g.releases = append(g.releases, rel)
			switch h := helmStatusHealth(rel.Status); h {
			case kube.HealthErr:
				g.failed++
			case kube.HealthWarn:
				g.pending++
			}
			if g.desc == "" {
				g.desc = rel.ChartInfo.Description
			}
		}
		names := make([]string, 0, len(by))
		for n := range by {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			g := by[n]
			status, h := fmt.Sprintf("%d deployed", len(g.releases)-g.failed-g.pending), kube.HealthOK
			if g.pending > 0 {
				status, h = fmt.Sprintf("%d pending", g.pending), kube.HealthWarn
			}
			if g.failed > 0 {
				status, h = fmt.Sprintf("%d failed", g.failed), kube.HealthErr
			}
			// Row.Namespace/Name point at the newest release using this chart (source of its files).
			newest := g.releases[0]
			for _, rel := range g.releases {
				if rel.Updated.After(newest.Updated) {
					newest = rel
				}
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: newest.Namespace, Name: newest.Name, Health: h, Cells: []string{
				n, setList(g.versions), setList(g.apps), strconv.Itoa(len(g.releases)), setList(g.nss), status, g.desc}})
		}
		t.Note = fmt.Sprintf("%d charts · %d releases", len(names), len(rels))
		return t
	})
	r.Aliases = []string{"charts"}
	return r
}

func setList(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > 3 {
		return strings.Join(out[:3], ", ") + fmt.Sprintf(" +%d", len(out)-3)
	}
	return strings.Join(out, ", ")
}

func (a *App) openCharts() tea.Cmd {
	back := a.res
	cmd := a.switchResource(a.chartsResource())
	if back.Name != chartsRes {
		a.backTo = &back
	}
	a.setStatus("enter: releases of this chart · v: default values · r: README · t: templates · i: info", false)
	return cmd
}

func (a *App) chartRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	chart := ""
	if len(row.Cells) > 0 {
		chart = row.Cells[0]
	}
	switch key {
	case "enter":
		back := a.res
		cmd := a.switchResource(a.helmResource())
		a.backTo = &back
		a.filter = chart
		a.setStatus("releases of chart "+chart+" · / to change the filter", false)
		return cmd, true
	case "v":
		return a.openChartText(row.Namespace, row.Name, "defaults"), true
	case "r":
		return a.openChartText(row.Namespace, row.Name, "readme"), true
	case "t":
		return a.openChartText(row.Namespace, row.Name, "templates"), true
	case "i":
		return a.openChartText(row.Namespace, row.Name, "info"), true
	}
	return nil, false
}

// openChartText shows a part of the chart packaged with release ns/name.
func (a *App) openChartText(ns, name, show string) tea.Cmd {
	kind := "chartdoc"
	switch show {
	case "defaults":
		kind = "helmvalues"
	case "templates":
		kind = "manifest"
	}
	cmd := a.openHelmText(ns, name, 0, kind)
	a.helmShow = show
	return cmd
}

// renderChartText fills the text view for helmShow; it reports false when helmShow is not a chart part.
func (a *App) renderChartText(r *kube.HelmRelease) bool {
	ch := r.ChartInfo
	head := fmt.Sprintf("chart %s %s (from release %s/%s)", ch.Name, ch.Version, r.Namespace, r.Name)
	switch a.helmShow {
	case "defaults":
		v := any(ch.Defaults)
		state := "secret-looking values masked · x to unmask"
		if a.reveal {
			state = "UNMASKED · masks again in 60s · x to mask now"
		} else {
			v = maskValues(v)
		}
		b, _ := yaml.Marshal(v)
		if len(ch.Defaults) == 0 {
			b = []byte("# this chart has no default values\n")
		}
		a.textTitle = "default values · " + head + " · " + state
		a.textLines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	case "readme":
		a.textTitle = "README · " + head
		md := ch.Readme()
		if md == "" {
			md = "This chart has no README."
		}
		a.textLines = strings.Split(strings.TrimRight(md, "\n"), "\n")
	case "templates":
		a.textTitle = fmt.Sprintf("templates (%d) · %s · / to search", len(ch.Templates), head)
		var lines []string
		for _, f := range ch.Templates {
			lines = append(lines, a.st.Title.Render("# ── "+f.Name+" "+strings.Repeat("─", max(3, 60-len(f.Name)))))
			lines = append(lines, strings.Split(strings.TrimRight(string(f.Data), "\n"), "\n")...)
			lines = append(lines, "")
		}
		if len(lines) == 0 {
			lines = []string{"no templates packaged"}
		}
		a.textLines = lines
	case "info":
		a.textTitle = "info · " + head
		kv := func(k, v string) string { return a.st.YAMLKey.Render(fmt.Sprintf("%-14s", k)) + " " + v }
		lines := []string{kv("Chart", ch.Name), kv("Version", ch.Version), kv("App version", ch.AppVersion),
			kv("Description", ch.Description), kv("Home", ch.Home)}
		if len(ch.Sources) > 0 {
			lines = append(lines, kv("Sources", strings.Join(ch.Sources, ", ")))
		}
		if len(ch.Keywords) > 0 {
			lines = append(lines, kv("Keywords", strings.Join(ch.Keywords, ", ")))
		}
		for i, d := range ch.Dependencies {
			k := ""
			if i == 0 {
				k = "Dependencies"
			}
			lines = append(lines, kv(k, d))
		}
		files := []string{}
		for _, f := range ch.Files {
			files = append(files, f.Name)
		}
		lines = append(lines, kv("Templates", strconv.Itoa(len(ch.Templates))), kv("Files", strings.Join(files, ", ")))
		lines = append(lines, "", a.st.Muted.Render("v default values · r README · t templates · esc back"))
		a.textLines = lines
	default:
		return false
	}
	a.renderText(true)
	return true
}
