package ui

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// ---------- Loki ----------

// openLoki searches logs kept by Loki, also after pods are gone (:loki [6h] [LogQL or text]).
// Without LogQL it searches the selected pod/workload (or the namespace) for the text.
func (a *App) openLoki(arg string) tea.Cmd {
	since := 24 * time.Hour
	f := strings.Fields(arg)
	if len(f) > 0 {
		if d, err := time.ParseDuration(f[0]); err == nil {
			since, f = d, f[1:]
		} else if strings.HasSuffix(f[0], "d") {
			if n, err := strconv.Atoi(strings.TrimSuffix(f[0], "d")); err == nil {
				since, f = time.Duration(n)*24*time.Hour, f[1:]
			}
		}
	}
	q := strings.Join(f, " ")
	if !strings.HasPrefix(q, "{") {
		ns, pod := a.client.Namespace, ""
		switch {
		case a.mode == modeText && a.textKind == "logs":
			ns, pod = a.logSpec.ns, a.logSpec.pods[0]
		case a.mode == modeTable:
			if row, ok := a.selected(); ok && (a.res.Name == "pods" || a.res.HasSelector()) {
				ns, pod = row.Namespace, row.Name
				if a.res.Name != "pods" {
					pod = row.Name + "-.*"
				}
			}
		}
		if ns == kube.AllNamespaces {
			a.setStatus("pick a namespace first, or give a LogQL selector like {app=\"web\"}", true)
			return nil
		}
		sel := kube.LokiSelector(ns, pod)
		if q != "" {
			sel += fmt.Sprintf(" |= %q", q)
		}
		q = sel
	}
	c, url := a.client, a.cfg.Loki.URL
	return a.openReport("loki", "searching Loki…", func(context.Context) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		lines, src, err := c.LokiQuery(ctx, url, q, since, 2000)
		if err != nil {
			return "", "", err
		}
		var b strings.Builder
		for _, l := range lines {
			who := l.Labels["pod"]
			fmt.Fprintf(&b, "%s %s %s\n", l.Time.Format("01-02 15:04:05"), a.st.Muted.Render("["+podTag(who)+"]"), l.Line)
		}
		if len(lines) == 0 {
			b.WriteString(a.st.Muted.Render("no log lines matched in the last " + shortDuration(since)))
		}
		return fmt.Sprintf("loki %s · last %s · %d lines · %s · / search · :trace", q, shortDuration(since), len(lines), src), b.String(), nil
	})
}

// ---------- traces ----------

var traceRe = regexp.MustCompile(`(?i)(?:trace[_-]?id|traceparent|x-b3-traceid)["':= ]+(?:00-)?([0-9a-f]{16,32})`)

// openTrace finds trace IDs in the text on screen (logs, Loki results) and opens one in the tracing UI (:trace).
func (a *App) openTrace() tea.Cmd {
	if a.mode != modeText {
		a.setStatus(":trace works in logs and Loki results", true)
		return nil
	}
	seen, ids := map[string]bool{}, []string{}
	for i := len(a.textLines) - 1; i >= 0 && len(ids) < 30; i-- {
		for _, m := range traceRe.FindAllStringSubmatch(ansi.Strip(a.textLines[i]), -1) {
			id := strings.ToLower(m[1])
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		a.setStatus("no trace IDs (trace_id=…, traceparent) in these lines", true)
		return nil
	}
	tmpl := a.cfg.Traces.URL
	a.openPicker("Open trace (newest first)", ids, func(id string) tea.Cmd {
		if tmpl == "" {
			a.setStatus("trace "+id+" · set traces.url in the config to open it, e.g. https://grafana/explore?…{trace_id}", false)
			return nil
		}
		u := strings.ReplaceAll(tmpl, "{trace_id}", id)
		if err := openBrowser(u); err != nil {
			a.setStatus("open "+u+": "+err.Error(), true)
		} else {
			a.setStatus("opened trace "+id, false)
		}
		return nil
	})
	return nil
}

// ---------- node pools ----------

const nodePoolsRes = "nodepools"

func (a *App) nodePoolsResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(nodePoolsRes, "Node pool", "Pools", func() kube.Table {
		t := kube.Table{Columns: []string{"POOL", "CLOUD", "NODES", "READY", "SPOT", "INSTANCE TYPES", "ZONES", "KUBELET", "EVICTIONS"}}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		pools, err := c.NodePools(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		ev := c.SpotEvictions(ctx)
		nodes, _ := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		perPool := map[string]int{}
		for _, n := range nodes.Items {
			for _, l := range []string{"kubernetes.azure.com/agentpool", "agentpool", "eks.amazonaws.com/nodegroup", "cloud.google.com/gke-nodepool", "karpenter.sh/nodepool"} {
				if p := n.Labels[l]; p != "" {
					perPool[p] += ev[n.Name]
					break
				}
			}
		}
		for _, p := range pools {
			h := kube.HealthOK
			if p.Ready < p.Nodes {
				h = kube.HealthWarn
			}
			spot := ""
			if p.Spot {
				spot = "spot"
			}
			evs := ""
			if n := perPool[p.Name]; n > 0 {
				evs, h = fmt.Sprint(n), kube.HealthWarn
			}
			t.Rows = append(t.Rows, kube.Row{Name: p.Cloud + "/" + p.Name, Health: h, Cells: []string{p.Name, p.Cloud, fmt.Sprint(p.Nodes),
				fmt.Sprintf("%d/%d", p.Ready, p.Nodes), spot, strings.Join(p.InstanceTypes, ","), strings.Join(p.Zones, ","), strings.Join(p.Versions, ","), evs}})
		}
		t.Note = fmt.Sprintf("%d pools · S scales (AKS via az, EKS via aws) · :upgrade for versions", len(pools))
		return t
	})
}

func (a *App) openNodePools() tea.Cmd {
	return a.openVirtual(a.nodePoolsResource(), "node pools from node labels · S scale · enter: its nodes")
}

type poolScaledMsg struct {
	desc string
	out  string
	err  error
}

func (a *App) scalePool(row kube.Row) tea.Cmd {
	if !a.guardWrite() {
		return nil
	}
	c := a.client
	cloud, name, _ := strings.Cut(row.Name, "/")
	a.openInput(ovInput, "nodes in "+name+" › ", func(v string) tea.Cmd {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 || n > 1000 {
			a.setStatus("enter a node count from 0 to 1000", true)
			return nil
		}
		a.askConfirm(fmt.Sprintf("Scale %s node pool %s on %s to %d nodes?", cloud, name, c.Context, n), name, func() tea.Cmd {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				pools, err := c.NodePools(ctx)
				if err != nil {
					return poolScaledMsg{err: err}
				}
				for _, p := range pools {
					if p.Cloud == cloud && p.Name == name {
						argv, err := c.ScalePoolCommand(ctx, p, n)
						if err != nil {
							return poolScaledMsg{err: err}
						}
						out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
						return poolScaledMsg{desc: strings.Join(argv, " "), out: string(out), err: err}
					}
				}
				return poolScaledMsg{err: fmt.Errorf("pool %s not found", name)}
			}
		})
		return nil
	})
	return nil
}

func (a *App) handlePoolScaled(m poolScaledMsg) tea.Cmd {
	a.audit("nodepool scale: "+m.desc, a.client.Context, "", "", m.err)
	if m.err != nil {
		a.setStatus("scale failed: "+m.err.Error()+" "+firstWords(m.out, 20), true)
		return nil
	}
	a.setStatus("✓ scaling started: "+m.desc, false)
	return a.refresh()
}

// ---------- in-place resize ----------

// resizePod changes CPU/memory of the selected pod without restarting it (:resize cpu=250m memory=512Mi
// [limit-cpu=1 limit-memory=1Gi] [container=name]). Needs Kubernetes 1.33+.
func (a *App) resizePod(arg string) tea.Cmd {
	row, ok := a.selected()
	if a.mode != modeTable || !ok || a.res.Name != "pods" {
		a.setStatus("select a pod first (:resize cpu=250m memory=512Mi)", true)
		return nil
	}
	vals := map[string]string{}
	for _, kv := range strings.Fields(arg) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			a.setStatus("use key=value: cpu, memory, limit-cpu, limit-memory, container", true)
			return nil
		}
		vals[strings.ToLower(k)] = v
	}
	if len(vals) == 0 || (vals["cpu"] == "" && vals["memory"] == "" && vals["limit-cpu"] == "" && vals["limit-memory"] == "") {
		a.setStatus("usage: :resize cpu=250m memory=512Mi [limit-cpu=1 limit-memory=1Gi] [container=name]", true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	c, ns, pod := a.client, row.Namespace, row.Name
	ctr := vals["container"]
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if ctr == "" {
			p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
			if err != nil {
				return resizeCheckedMsg{err: err}
			}
			ctr = p.Spec.Containers[0].Name
		}
		err := c.ResizePod(ctx, ns, pod, ctr, vals["cpu"], vals["memory"], vals["limit-cpu"], vals["limit-memory"], true)
		return resizeCheckedMsg{ns: ns, pod: pod, ctr: ctr, vals: vals, err: err}
	}
}

type resizeCheckedMsg struct {
	ns, pod, ctr string
	vals         map[string]string
	err          error
}

func (a *App) handleResizeChecked(m resizeCheckedMsg) tea.Cmd {
	if m.err != nil {
		a.setStatus("resize rejected: "+m.err.Error(), true)
		return nil
	}
	c := a.client
	var parts []string
	for _, k := range []string{"cpu", "memory", "limit-cpu", "limit-memory"} {
		if v := m.vals[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	a.writeIntent = true
	a.askConfirm(fmt.Sprintf("Dry-run passed. Resize %s/%s [%s] in place to %s? (the workload template is not changed)", m.ns, m.pod, m.ctr, strings.Join(parts, " ")), m.pod, func() tea.Cmd {
		return runAction("resize "+m.pod+" "+strings.Join(parts, " "), func(ctx context.Context) error {
			return c.ResizePod(ctx, m.ns, m.pod, m.ctr, m.vals["cpu"], m.vals["memory"], m.vals["limit-cpu"], m.vals["limit-memory"], false)
		})
	})
	return nil
}

func init() {
	registerScreen(nodePoolsRes, screenDef{
		title: func(a *App) string { return "Node pools · " + a.client.Context + " · " + a.table.Note },
		hints: []kv{{"S", "scale"}, {"enter", "nodes"}, {"r", "refresh"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			switch key {
			case "S":
				return a.scalePool(row), true
			case "enter":
				nodes, _ := kube.Lookup("nodes")
				cmd := a.switchResource(nodes)
				a.filter = row.Cells[0]
				return cmd, true
			}
			return nil, false
		},
	})
}
