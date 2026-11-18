package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

const (
	searchPrefix = "search:"
	policyRepRes = "policyreports"
	backupsRes   = "backups"
)

// ---------- multi-cluster search ----------

// searchResource searches every connected cluster (or every kubeconfig context with --all).
func (a *App) searchResource(q string, everywhere bool) kube.Resource {
	ctxs := a.cl.connected()
	if everywhere {
		ctxs = a.client.Contexts()
	}
	if len(ctxs) == 0 {
		ctxs = []string{a.client.Context}
	}
	connects := map[string]tea.Cmd{}
	for _, c := range ctxs {
		connects[c] = a.connect(c, "", "probe")
	}
	return kube.VirtualResource(searchPrefix+q, "Match", "Search", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		t := kube.Table{Columns: []string{"CLUSTER", "KIND", "NAMESPACE", "NAME", "MATCHED"}}
		var mu sync.Mutex
		var wg sync.WaitGroup
		var hits []kube.SearchHit
		var failed []string
		for name, cmd := range connects {
			wg.Add(1)
			go func(name string, cmd tea.Cmd) {
				defer wg.Done()
				m := cmd().(connectMsg)
				var h []kube.SearchHit
				err := m.err
				if err == nil {
					h, err = m.client.Search(ctx, q)
				}
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					failed = append(failed, name)
				}
				hits = append(hits, h...)
			}(name, cmd)
		}
		wg.Wait()
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Context+hits[i].Kind < hits[j].Context+hits[j].Kind })
		for _, h := range hits {
			t.Rows = append(t.Rows, kube.Row{Namespace: h.Namespace, Name: h.Context + "|" + h.Resource + "|" + h.Name, Health: kube.HealthNeutral,
				Cells: []string{h.Context, h.Kind, h.Namespace, h.Name, h.Match}})
		}
		t.Note = fmt.Sprintf("%d matches for %q in %d cluster(s)", len(hits), q, len(connects))
		if len(failed) > 0 {
			sort.Strings(failed)
			t.Note += " · not searched: " + kube.ShortList(failed, 2)
		}
		return t
	})
}

func (a *App) searchCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	everywhere := false
	var q []string
	for _, x := range f {
		if x == "--all" || x == "-A" {
			everywhere = true
			continue
		}
		q = append(q, x)
	}
	if len(q) == 0 {
		a.setStatus("usage: :find <name|image|ip|host|label=value> [--all]   (searches connected clusters; --all = every context)", true)
		return nil
	}
	scope := "connected clusters"
	if everywhere {
		scope = "every kubeconfig context (may ask you to sign in)"
	}
	return a.openVirtual(a.searchResource(strings.Join(q, " "), everywhere), "searching "+scope+" · enter: open · esc: back")
}

func (a *App) searchRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	if key != "enter" && key != "d" && key != "y" {
		return nil, false
	}
	parts := strings.SplitN(row.Name, "|", 3)
	if len(parts) != 3 {
		return nil, false
	}
	ctx, resName, name := parts[0], parts[1], parts[2]
	if ctx != a.client.Context {
		return a.runCommand(fmt.Sprintf("tabnew @%s %s", ctx, row.Namespace)), true
	}
	r, _ := kube.Lookup(resName)
	kind := "describe"
	if key == "y" {
		kind = "yaml"
	}
	return a.openText(r, row.Namespace, name, kind), true
}

// ---------- Helm upgrade preview ----------

// helmPreview renders `helm upgrade --dry-run=server` and diffs the manifest with what runs now.
func (a *App) helmPreview(ns, rel, args string) tea.Cmd {
	if _, err := exec.LookPath("helm"); err != nil {
		a.setStatus("the upgrade preview needs the helm CLI", true)
		return nil
	}
	if strings.TrimSpace(args) == "" {
		a.openInput(ovInput, "helm upgrade "+rel+" › chart [--version x] [-f values.yaml] [--set k=v] › ", func(s string) tea.Cmd { return a.helmPreview(ns, rel, s) })
		return nil
	}
	c := a.client
	return a.openReport("diff", "rendering helm upgrade --dry-run…", func(ctx context.Context) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		hist, err := c.HelmHistory(ctx, ns, rel)
		if err != nil || len(hist) == 0 {
			return "", "", fmt.Errorf("release %s/%s: %v", ns, rel, err)
		}
		cmdArgs := append([]string{"upgrade", rel}, strings.Fields(args)...)
		cmdArgs = append(cmdArgs, "-n", ns, "--kube-context", c.Context, "--dry-run=server", "-o", "json")
		if c.Kubeconfig != "" {
			cmdArgs = append(cmdArgs, "--kubeconfig", c.Kubeconfig)
		}
		out, err := exec.CommandContext(ctx, "helm", cmdArgs...).Output()
		if err != nil {
			msg := err.Error()
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				msg = strings.TrimSpace(string(ee.Stderr))
			}
			return "", "", fmt.Errorf("helm: %s", msg)
		}
		var res struct {
			Manifest string `json:"manifest"`
			Chart    struct {
				Metadata struct {
					Version    string `json:"version"`
					AppVersion string `json:"appVersion"`
				} `json:"metadata"`
			} `json:"chart"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return "", "", fmt.Errorf("unexpected helm output: %w", err)
		}
		cur := hist[0]
		d := diffLines(strings.Split(cur.Manifest, "\n"), strings.Split(res.Manifest, "\n"), 3)
		add, del := diffStats(d)
		lines := a.renderDiffLines(d)
		if add+del == 0 {
			lines = []string{a.st.OKText.Render("✓ no change to the rendered manifests")}
		}
		lines = append(lines, "", a.st.Muted.Render("Server-side dry-run: nothing was changed. Run the same helm upgrade yourself (or from your pipeline) to apply."))
		return fmt.Sprintf("helm upgrade preview %s/%s: %s %s → %s %s · %s", ns, rel, cur.ChartVersion, cur.AppVersion, res.Chart.Metadata.Version, res.Chart.Metadata.AppVersion, diffHeader(add, del)),
			strings.Join(lines, "\n"), nil
	})
}

// ---------- policy reports ----------

func (a *App) policyReportsResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(policyRepRes, "Policy result", "Policies", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"ENGINE", "POLICY", "RESULT", "NAMESPACE", "OBJECT", "MESSAGE"}}
		vs, engines, err := c.PolicyReports(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		fail := 0
		for _, v := range vs {
			if v.H == kube.HealthErr {
				fail++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: v.Namespace, Name: v.Object, Health: v.H, Cells: []string{v.Engine, v.Policy, v.Result, v.Namespace, v.Object, firstLineStr(v.Message, 100)}})
		}
		t.Note = fmt.Sprintf("%d results · %d failing · engines: %s", len(vs), fail, nonEmptyStr(strings.Join(engines, ", "), "none found"))
		return t
	})
}

// ---------- quotas & capacity ----------

func (a *App) openCapacity() tea.Cmd {
	c := a.client
	return a.openReport("capacity", "reading quotas and node capacity…", func(ctx context.Context) (string, string, error) {
		quotas, limits, pools, err := c.Capacity(ctx)
		if err != nil {
			return "", "", err
		}
		s := a.st
		bar := func(pct int) string {
			if pct < 0 {
				return s.Muted.Render("no limit")
			}
			n := min(20, pct/5)
			st := s.OKText
			switch {
			case pct >= 90:
				st = s.ErrText
			case pct >= 75:
				st = s.WarnText
			}
			return st.Render(strings.Repeat("█", n)+strings.Repeat("░", 20-n)) + fmt.Sprintf(" %3d%%", pct)
		}
		var lines []string
		lines = append(lines, s.Title.Render("Resource quotas · "+c.NamespaceLabel()))
		if len(quotas) == 0 {
			lines = append(lines, s.Muted.Render("  none"))
		}
		for _, q := range quotas {
			lines = append(lines, fmt.Sprintf("  %-22s %-28s %s  %s / %s", q.Namespace+"/"+q.Quota, q.Resource, bar(q.Percent), q.Used, q.Hard))
		}
		if len(limits) > 0 {
			lines = append(lines, "", s.Title.Render("Limit ranges"))
			for _, l := range limits {
				lines = append(lines, "  "+l)
			}
		}
		lines = append(lines, "", s.Title.Render("Node pools (requests vs allocatable)"))
		if len(pools) == 0 {
			lines = append(lines, s.Muted.Render("  not available (needs list on nodes and all pods)"))
		}
		removable := 0
		for _, p := range pools {
			cpuPct, memPct := int(p.CPUReq*100/max(p.CPUAlloc, 1)), int(p.MemReq*100/max(p.MemAlloc, 1))
			lines = append(lines, fmt.Sprintf("  %-14s %2d nodes  CPU %s  %s / %s", p.Pool, p.Nodes, bar(cpuPct), milli(p.CPUReq), milli(p.CPUAlloc)),
				fmt.Sprintf("  %-14s %8s  MEM %s  %s / %s", "", "", bar(memPct), kube.FormatBytes(p.MemReq), kube.FormatBytes(p.MemAlloc)))
			if p.Removable > 0 {
				removable += p.Removable
				lines = append(lines, s.WarnText.Render(fmt.Sprintf("  %-14s %8s  ↓ %d node(s) could go and keep 20%% headroom (check PDBs, zones and max pod spread first)", "", "", p.Removable)))
			}
		}
		title := fmt.Sprintf("capacity · %d quotas · %d node pools", len(quotas), len(pools))
		if removable > 0 {
			title += fmt.Sprintf(" · %d node(s) look removable", removable)
		}
		return title, strings.Join(lines, "\n"), nil
	})
}

// ---------- Velero ----------

func (a *App) backupsResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(backupsRes, "Backup", "Backups", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"KIND", "NAME", "PHASE", "STARTED", "DURATION", "ERRORS", "WARNINGS", "SCHEDULE", "EXPIRES"}}
		bs, found, err := c.VeleroBackups(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		if !found {
			t.Note = "Velero is not installed on this cluster"
			return t
		}
		failed := 0
		for _, b := range bs {
			started, dur, exp := "-", "-", "-"
			if !b.Started.IsZero() {
				started = kube.Age(metav1Time(b.Started)) + " ago"
			}
			if !b.Completed.IsZero() && !b.Started.IsZero() {
				dur = b.Completed.Sub(b.Started).Round(time.Second).String()
			}
			if !b.Expires.IsZero() {
				exp = "in " + time.Until(b.Expires).Round(time.Hour).String()
			}
			phase := b.Phase
			if b.Kind == "Schedule" {
				dur = "-"
				if b.Paused {
					phase = "Paused"
				}
				if !b.Started.IsZero() {
					started = "last " + started
				}
			}
			if b.H == kube.HealthErr {
				failed++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: b.Namespace, Name: b.Name, Health: b.H, Cells: []string{b.Kind, b.Name, phase, started, dur,
				fmt.Sprint(b.Errors), fmt.Sprint(b.Warnings), b.Schedule, exp}})
		}
		t.Note = fmt.Sprintf("%d items · %d failed", len(bs), failed)
		return t
	})
}

// ---------- incident report export ----------

// exportReport writes relationships, timeline, open issues and the last AI answer for the selected
// object to Markdown (:report [file]).
func (a *App) exportReport(path string) tea.Cmd {
	row, ok := a.selected()
	kind := a.res.Kind
	if !ok || !graphKinds[kind] {
		a.setStatus("select an ingress, service, workload or pod first, then :report [file]", true)
		return nil
	}
	c := a.client
	ns, name := row.Namespace, row.Name
	if path == "" {
		path = fmt.Sprintf("kubeweave-report-%s-%s-%s.md", strings.ToLower(kind), name, time.Now().Format("20060102-1504"))
	}
	lastAI := ""
	for i := len(a.chat) - 1; i >= 0; i-- {
		if a.chat[i].role == "assistant" && strings.TrimSpace(a.chat[i].content) != "" {
			lastAI = a.chat[i].content
			break
		}
	}
	a.setStatus("writing report…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		var b strings.Builder
		fmt.Fprintf(&b, "# Incident report: %s %s/%s\n\n", strings.ToLower(kind), ns, name)
		fmt.Fprintf(&b, "- Cluster: `%s`\n- Generated: %s by kubeweave\n\n", c.Context, time.Now().Format(time.RFC1123))
		if root, front, err := c.Relations(ctx, kind, ns, name); err == nil {
			b.WriteString("## Relationships\n\n```\n")
			if front != nil {
				for _, f := range front.Children {
					b.WriteString(plainGraph(f, "") + "")
				}
			}
			b.WriteString(plainGraph(root, ""))
			b.WriteString("```\n\n")
		}
		if tl, err := c.Timeline(ctx, kind, ns, name); err == nil {
			b.WriteString("## Timeline (newest first)\n\n| Time | Source | Object | What |\n|---|---|---|---|\n")
			for i, e := range tl {
				if i >= 80 {
					break
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", e.Time.Local().Format("2006-01-02 15:04:05"), e.Source, e.Object, strings.ReplaceAll(e.Text, "|", "\\|"))
			}
			b.WriteString("\n")
		}
		if issues, err := c.WithNamespace(ns).Scan(ctx); err == nil && len(issues) > 0 {
			b.WriteString("## Open issues in the namespace\n\n```\n" + kube.FormatIssues(issues) + "```\n\n")
		}
		if lastAI != "" {
			b.WriteString("## AI analysis\n\n" + lastAI + "\n")
		}
		err := os.WriteFile(path, []byte(b.String()), 0o644)
		abs, _ := filepath.Abs(path)
		return actionMsg{status: "report written to " + abs, err: err}
	}
}

func plainGraph(n *kube.GraphNode, indent string) string {
	s := fmt.Sprintf("%s%s %s", indent, n.Kind, n.Name)
	if n.Status != "" {
		s += "  " + n.Status
	}
	if n.Note != "" {
		s += "  (" + n.Note + ")"
	}
	s += "\n"
	for _, c := range n.Children {
		s += plainGraph(c, indent+"  ")
	}
	return s
}
