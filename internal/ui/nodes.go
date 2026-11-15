package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	corev1 "k8s.io/api/core/v1"

	"kubesage/internal/kube"
)

type nodeHealthMsg struct {
	pane    *pane
	report  *kube.NodeReport
	err     error
	refresh bool
}

func (m nodeHealthMsg) targetPane() *pane { return m.pane }

type drainPlanMsg struct {
	pane *pane
	plan *kube.DrainPlan
	err  error
}

func (m drainPlanMsg) targetPane() *pane { return m.pane }

type drainDoneMsg struct {
	pane *pane
	res  kube.DrainResult
	err  error
}

func (m drainDoneMsg) targetPane() *pane { return m.pane }

// askConfirmTyped always requires typing text (for disruptive actions like drain).
func (a *App) askConfirmTyped(question, text string, fn func() tea.Cmd) {
	a.askConfirm(question, text, fn)
	a.inputHint = text
	a.input.Focus()
}

func fetchNodeHealth(c *kube.Client, node string, refresh bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		r, err := c.NodeHealth(ctx, node)
		return nodeHealthMsg{report: r, err: err, refresh: refresh}
	}
}

func (a *App) openNodeHealth(node string) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode = modeText
	nodeRes, _ := kube.Lookup("nodes")
	a.target = targetRef{nodeRes, "", node}
	a.textKind, a.textTitle, a.textLines = "nodehealth", "node "+node, nil
	a.renderText(true)
	return tagPane(a.pane, fetchNodeHealth(a.client, node, false))
}

func pctOf(v, total int64) int {
	if total <= 0 {
		return -1
	}
	return int(v * 100 / total)
}

func (a *App) handleNodeHealth(m nodeHealthMsg) {
	if a.mode != modeText || a.textKind != "nodehealth" {
		return
	}
	s := a.st
	if m.err != nil {
		a.textLines = []string{s.ErrText.Render("✗ " + m.err.Error())}
		a.renderText(!m.refresh)
		return
	}
	r, n := m.report, m.report.Node
	label := func(l string) string { return s.YAMLKey.Render(fmt.Sprintf("%-14s", l)) }
	verdict := s.OKText.Bold(true).Render("✓ Ready")
	if !kube.NodeReady(n) {
		verdict = s.ErrText.Bold(true).Render("✗ NotReady")
	}
	if n.Spec.Unschedulable {
		verdict += "  " + s.WarnText.Bold(true).Render("▲ cordoned (no new pods)")
	}
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady && c.Status == corev1.ConditionTrue {
			verdict += "  " + s.WarnText.Render("▲ "+string(c.Type))
		}
	}
	lb := n.Labels
	pool := lb["kubernetes.azure.com/agentpool"]
	if pool == "" {
		pool = lb["eks.amazonaws.com/nodegroup"] + lb["cloud.google.com/gke-nodepool"]
	}
	out := []string{"", "  " + verdict, "",
		"  " + label("Node") + s.Base.Render(n.Name),
		"  " + label("Pool / zone") + s.Base.Render(fmt.Sprintf("%s · %s · %s", nonEmptyStr(pool, "-"), lb["topology.kubernetes.io/zone"], lb["node.kubernetes.io/instance-type"])),
		"  " + label("Software") + s.Base.Render(fmt.Sprintf("kubelet %s · %s · %s", n.Status.NodeInfo.KubeletVersion, n.Status.NodeInfo.OSImage, n.Status.NodeInfo.ContainerRuntimeVersion)),
		"  " + label("Age") + s.Base.Render(kube.Age(n.CreationTimestamp)), ""}
	alloc := n.Status.Allocatable
	cpuA, memA, podsA := alloc.Cpu().MilliValue(), alloc.Memory().Value(), alloc.Pods().Value()
	running := 0
	for i := range r.Pods {
		if r.Pods[i].Status.Phase == corev1.PodRunning || r.Pods[i].Status.Phase == corev1.PodPending {
			running++
		}
	}
	row := func(name string, pct int, detail string) string {
		return "  " + label(name) + a.meter(pct, 24) + " " + s.Base.Render(fmt.Sprintf("%4s", pctStr(pct))) + "  " + s.Muted.Render(detail)
	}
	milli := func(v int64) string { return fmt.Sprintf("%dm", v) }
	out = append(out, "  "+s.Title.Render("Capacity (of allocatable)"))
	out = append(out,
		row("CPU requests", pctOf(r.CPUReq, cpuA), milli(r.CPUReq)+" of "+milli(cpuA)),
		row("CPU limits", pctOf(r.CPULim, cpuA), milli(r.CPULim)+" (over 100% = overcommitted)"),
		row("Mem requests", pctOf(r.MemReq, memA), kube.FormatBytes(r.MemReq)+" of "+kube.FormatBytes(memA)),
		row("Mem limits", pctOf(r.MemLim, memA), kube.FormatBytes(r.MemLim)),
		row("Pods", pctOf(int64(running), podsA), fmt.Sprintf("%d of %d", running, podsA)))
	if r.HasUsage {
		out = append(out, row("CPU used", pctOf(r.CPUUse, cpuA), milli(r.CPUUse)+" live"), row("Mem used", pctOf(r.MemUse, memA), kube.FormatBytes(r.MemUse)+" live"))
	}
	out = append(out, "", "  "+s.Title.Render("Conditions"))
	for _, c := range n.Status.Conditions {
		ok := (c.Type == corev1.NodeReady) == (c.Status == corev1.ConditionTrue)
		mark := s.OKText.Render("✓")
		if !ok {
			mark = s.ErrText.Render("✗")
		}
		out = append(out, fmt.Sprintf("  %s %-22s %s", mark, s.Base.Render(string(c.Type)), s.Muted.Render(kube.Age(c.LastTransitionTime)+" · "+strings.TrimSpace(c.Reason))))
	}
	out = append(out, "", "  "+s.Title.Render("Taints"))
	if len(n.Spec.Taints) == 0 {
		out = append(out, "  "+s.Muted.Render("none"))
	}
	for _, t := range n.Spec.Taints {
		v := t.Key
		if t.Value != "" {
			v += "=" + t.Value
		}
		out = append(out, "  "+s.WarnText.Render(v+":"+string(t.Effect)))
	}
	// Biggest memory requesters on the node.
	pods := append([]corev1.Pod(nil), r.Pods...)
	sort.Slice(pods, func(i, j int) bool {
		_, _, mi, _ := podReq(&pods[i])
		_, _, mj, _ := podReq(&pods[j])
		return mi > mj
	})
	out = append(out, "", "  "+s.Title.Render(fmt.Sprintf("Pods on this node (%d) · largest memory requests first", len(r.Pods))))
	for i, p := range pods {
		if i == 12 {
			out = append(out, "  "+s.Muted.Render(fmt.Sprintf("… %d more", len(pods)-12)))
			break
		}
		cr, _, mr, _ := podReq(&p)
		out = append(out, fmt.Sprintf("  %-60s %s", s.Base.Render(p.Namespace+"/"+p.Name), s.Muted.Render(fmt.Sprintf("cpu %dm · mem %s · %s", cr, kube.FormatBytes(mr), kube.PodStatus(&p)))))
	}
	out = append(out, "", "  "+s.Muted.Render("u: cordon/uncordon · X: drain preview · :taint key=value:NoSchedule · esc: back"))
	a.textTitle = "node " + n.Name + " · refreshes every " + fmt.Sprint(a.cfg.RefreshSeconds) + "s"
	a.textLines = out
	a.renderText(!m.refresh)
}

func podReq(p *corev1.Pod) (cpuReq, cpuLim, memReq, memLim int64) {
	for _, c := range p.Spec.Containers {
		cpuReq += c.Resources.Requests.Cpu().MilliValue()
		cpuLim += c.Resources.Limits.Cpu().MilliValue()
		memReq += c.Resources.Requests.Memory().Value()
		memLim += c.Resources.Limits.Memory().Value()
	}
	return
}

func pctStr(p int) string {
	if p < 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", p)
}

func nonEmptyStr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ---------- cordon / drain / taint ----------

func (a *App) toggleCordon(node string, cordoned bool) tea.Cmd {
	if !a.guardWrite() {
		return nil
	}
	verb, on := "Cordon", true
	if cordoned {
		verb, on = "Uncordon", false
	}
	a.askConfirm(fmt.Sprintf("%s node %s on %s?", verb, node, a.client.Context), node, func() tea.Cmd {
		return runAction(strings.ToLower(verb)+"ed "+node, func(ctx context.Context) error { return a.client.Cordon(ctx, node, on) })
	})
	return nil
}

func (a *App) openDrainPreview(node string) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode = modeText
	nodeRes, _ := kube.Lookup("nodes")
	a.target = targetRef{nodeRes, "", node}
	a.textKind, a.textTitle, a.textLines = "drain", "drain preview "+node, nil
	a.drain = nil
	a.renderText(true)
	c := a.client
	return tagPane(a.pane, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		p, err := c.PlanDrain(ctx, node)
		return drainPlanMsg{plan: p, err: err}
	})
}

func (a *App) handleDrainPlan(m drainPlanMsg) {
	if a.mode != modeText || a.textKind != "drain" {
		return
	}
	s := a.st
	if m.err != nil {
		a.textLines = []string{s.ErrText.Render("✗ " + m.err.Error())}
		a.renderText(true)
		return
	}
	a.drain = m.plan
	p := m.plan
	out := []string{"", "  " + s.Title.Render("Drain preview for "+p.Node+" — nothing has been changed"), "",
		fmt.Sprintf("  %s   %s   %s", s.OKText.Render(fmt.Sprintf("%d will be evicted", p.Evict)), s.Muted.Render(fmt.Sprintf("%d skipped", p.Skip)),
			s.ErrText.Render(fmt.Sprintf("%d blocked", p.Blocked))), ""}
	for _, it := range p.Items {
		mark, st := "↻ evict  ", s.Base
		switch it.Action {
		case "skip":
			mark, st = "· skip   ", s.Muted
		case "blocked":
			mark, st = "✗ blocked", s.ErrText
		}
		line := fmt.Sprintf("  %s  %-56s %s", st.Render(mark), st.Render(it.Namespace+"/"+it.Name), s.Muted.Render(it.Owner))
		out = append(out, line)
		if it.Reason != "" {
			out = append(out, "             "+s.Muted.Render(it.Reason))
		}
	}
	out = append(out, "")
	if p.Blocked > 0 {
		out = append(out, "  "+s.WarnText.Render("Blocked pods are left in place; resolve them (scale up the PDB's workload, or move unmanaged pods) for a full drain."))
	}
	out = append(out, "  "+s.Muted.Render("A: cordon + evict the pods marked evict · esc: cancel"))
	a.textLines = out
	a.renderText(true)
}

func (a *App) startDrain() tea.Cmd {
	p := a.drain
	if p == nil || !a.guardWrite() {
		return nil
	}
	c := a.client
	a.askConfirmTyped(fmt.Sprintf("Drain node %s on %s: cordon it and evict %d pod(s)?", p.Node, c.Context, p.Evict), p.Node, func() tea.Cmd {
		a.setStatus(fmt.Sprintf("draining %s (evicting %d pods; PDB-limited evictions are retried)…", p.Node, p.Evict), false)
		return tagPane(a.pane, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			res, err := c.Drain(ctx, p)
			return drainDoneMsg{res: res, err: err}
		})
	})
	return nil
}

func (a *App) handleDrainDone(m drainDoneMsg) tea.Cmd {
	switch {
	case m.err != nil:
		a.setStatus("drain failed: "+m.err.Error(), true)
	case len(m.res.Failed) > 0:
		a.setStatus(fmt.Sprintf("drain: %d evicted, %d failed (%s)", len(m.res.Evicted), len(m.res.Failed), m.res.Failed[0]), true)
	default:
		a.setStatus(fmt.Sprintf("✓ node cordoned and %d pod(s) evicted", len(m.res.Evicted)), false)
	}
	return nil
}

func (a *App) taintCommand(spec string) tea.Cmd {
	row, ok := a.selected()
	if a.res.Name != "nodes" || !ok {
		a.setStatus("select a node first, then :taint key=value:NoSchedule (or key- to remove)", true)
		return nil
	}
	t, remove, err := kube.ParseTaint(spec)
	if err != nil {
		a.setStatus(err.Error(), true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	verb := "Add taint"
	if remove {
		verb = "Remove taint"
	}
	node := row.Name
	a.askConfirm(fmt.Sprintf("%s %s on node %s?", verb, strings.TrimSpace(spec), node), node, func() tea.Cmd {
		return runAction(strings.ToLower(verb)+" "+spec+" on "+node, func(ctx context.Context) error { return a.client.SetTaint(ctx, node, t, remove) })
	})
	return nil
}
