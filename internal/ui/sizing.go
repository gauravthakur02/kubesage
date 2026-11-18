package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/config"
	"github.com/gauravthakur02/kubeweave/internal/kube"
)

const sizingRes = "rightsizing"

func milli(v int64) string {
	if v == 0 {
		return "-"
	}
	if v >= 1000 && v%100 == 0 {
		return fmt.Sprintf("%.1f", float64(v)/1000)
	}
	return fmt.Sprintf("%dm", v)
}

func bytesOrDash(v int64) string {
	if v == 0 {
		return "-"
	}
	return kube.FormatBytes(v)
}

// sizingResource compares requests with peak usage per workload.
func (a *App) sizingResource() kube.Resource {
	c, hist, cost := a.client, a.hist, a.cfg.Cost
	return kube.VirtualResource(sizingRes, "Workload sizing", "Sizing", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"NAMESPACE", "WORKLOAD", "PODS", "CPU REQ", "CPU PEAK", "→ CPU", "MEM REQ", "MEM PEAK", "→ MEM", "VPA", "VERDICT", "WASTE"}}
		vpa := c.VPARecommendations(ctx)
		peak := func(ns, pod string) (int64, int64, bool) {
			s := hist.get(histKey(c.Context, ns, pod))
			if len(s) == 0 {
				return 0, 0, false
			}
			var pc, pm int64
			for _, x := range s {
				pc, pm = max(pc, x.cpu), max(pm, x.mem)
			}
			return pc, pm, true
		}
		long, longNote := a.cachedPeaks(ctx, c)
		rows, err := c.RightSizing(ctx, peak, long)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		var wasteCPU, wasteMem int64
		over := 0
		for _, s := range rows {
			h := kube.HealthOK
			verdict := "ok"
			switch s.Verdict {
			case "over":
				h, verdict = kube.HealthWarn, "over-provisioned"
				over++
			case "under":
				h, verdict = kube.HealthErr, "under-provisioned"
			case "no-requests":
				h, verdict = kube.HealthWarn, "no requests set"
			}
			wasteCPU += s.WasteCPU
			wasteMem += s.WasteMem
			waste := ""
			if s.WasteCPU > 0 || s.WasteMem > 0 {
				waste = strings.TrimSpace(fmt.Sprintf("%s CPU %s", milli(s.WasteCPU), bytesOrDash(s.WasteMem)))
				if m := monthly(cost, s.WasteCPU, s.WasteMem); m != "" {
					waste += " · " + m
				}
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: s.Namespace, Name: s.Workload, Health: h, Cells: []string{
				s.Namespace, s.Workload, fmt.Sprint(s.Pods), milli(s.CPUReq), milli(s.CPUPeak), milli(s.CPUSuggest),
				bytesOrDash(s.MemReq), bytesOrDash(s.MemPeak), bytesOrDash(s.MemSuggest), vpaCell(vpa, s.Namespace, s.Workload), verdict, waste}})
		}
		t.Note = fmt.Sprintf("%d workloads · %d over-provisioned · reclaimable %s CPU, %s memory", len(rows), over, milli(wasteCPU), bytesOrDash(wasteMem))
		if m := monthly(cost, wasteCPU, wasteMem); m != "" {
			t.Note += " ≈ " + m
		}
		if long != nil {
			t.Note += longNote
		} else {
			t.Note += " · peaks from metrics now + up to 15m of history" + longNote
		}
		return t
	})
}

// monthly prices wasted requests per month (730 h) when cost is configured.
func monthly(c config.Cost, cpu, mem int64) string {
	if c.CPUCoreHour == 0 && c.MemGiBHour == 0 {
		return ""
	}
	v := (float64(cpu)/1000*c.CPUCoreHour + float64(mem)/(1<<30)*c.MemGiBHour) * 730
	cur := c.Currency
	if cur == "" {
		cur = "$"
	}
	return fmt.Sprintf("%.0f %s/month", v, cur)
}

func (a *App) openSizing() tea.Cmd {
	back := a.res
	cmd := a.switchResource(a.sizingResource())
	if back.Name != sizingRes {
		a.backTo = &back
	}
	a.setStatus("enter: suggested change · e: safe edit the workload · y: YAML · peaks improve the longer kubeweave watches pods", false)
	return cmd
}

func (a *App) sizingRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	r, name, ok := policyTarget(row)
	if !ok {
		return nil, false
	}
	switch key {
	case "enter":
		cells := row.Cells
		cpu, mem := cells[5], cells[8]
		cmd := fmt.Sprintf("kubectl --context %s -n %s set resources %s/%s --requests=cpu=%s,memory=%s",
			a.client.Context, row.Namespace, strings.ToLower(r.Kind), name, cpu, mem)
		lines := []string{
			a.st.Title.Render(row.Name + " in " + row.Namespace),
			"",
			fmt.Sprintf("  CPU     request %-8s peak %-8s → suggest %s", cells[3], cells[4], cpu),
			fmt.Sprintf("  memory  request %-8s peak %-8s → suggest %s", cells[6], cells[7], mem),
			vpaLine(cells[9]),
			"",
			a.st.Muted.Render("Suggestion: CPU = peak × 1.5, memory = peak × 1.3 (memory is not compressible: keep headroom)."),
			a.st.Muted.Render("Peaks come from metrics-server now plus what kubeweave has seen in the last 15 minutes; check a busy period before lowering."),
			"",
			"Apply with:",
			"  " + a.st.Code.Render(cmd),
			"",
			a.st.Muted.Render("or press e on the table row to edit the workload safely (diff + dry-run). Limits are not changed by this command."),
		}
		a.prevMode, a.mode = a.mode, modeText
		a.textKind, a.textTitle, a.textLines = "sizing", "right-sizing "+row.Name, lines
		a.target = targetRef{r, row.Namespace, name}
		a.renderText(true)
		return nil, true
	case "e":
		return a.openEdit(r, row.Namespace, name), true
	case "y":
		return a.openText(r, row.Namespace, name, "yaml"), true
	}
	return nil, false
}

func vpaCell(vpa map[string][2]int64, ns, workload string) string {
	if r, ok := vpa[ns+"/"+workload]; ok {
		return milli(r[0]) + " " + bytesOrDash(r[1])
	}
	return "-"
}

func vpaLine(v string) string {
	if v == "-" {
		return ""
	}
	return "  VPA     recommends " + v + " per pod"
}
