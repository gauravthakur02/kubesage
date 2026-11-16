package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// prometheusFor returns a Prometheus client for the context: configured URL, or an auto-detected
// in-cluster service (only when prometheus.urls has "auto" or is empty and auto-detection finds one).
func (a *App) prometheusFor(ctx context.Context, c *kube.Client) (*kube.Prom, error) {
	pc := a.cfg.Prometheus
	u := pc.URLs[c.Context]
	if u == "" {
		u = pc.URLs["*"]
	}
	if u == "off" {
		return nil, fmt.Errorf("prometheus disabled for this context")
	}
	if u == "auto" {
		u = ""
	}
	return c.NewProm(ctx, u, pc.BearerTokenCommand)
}

func (a *App) promWindow() string {
	if w := a.cfg.Prometheus.Window; w != "" {
		return w
	}
	return "7d"
}

// openPromQuery runs a PromQL query and shows the top series with a 1-hour sparkline (:prom <query>).
func (a *App) openPromQuery(q string) tea.Cmd {
	if strings.TrimSpace(q) == "" {
		a.setStatus(`usage: :prom <PromQL>   e.g. :prom sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="shop"}[5m]))`, true)
		return nil
	}
	c := a.client
	return a.openReport("prom", "querying Prometheus…", func(ctx context.Context) (string, string, error) {
		p, err := a.prometheusFor(ctx, c)
		if err != nil {
			return "", "", err
		}
		series, err := p.QueryRange(ctx, q, time.Hour, time.Minute)
		if err != nil {
			return "", "", err
		}
		s := a.st
		var lines []string
		for i, x := range series {
			if i >= 40 {
				lines = append(lines, s.Muted.Render(fmt.Sprintf("… %d more series", len(series)-40)))
				break
			}
			keys := make([]string, 0, len(x.Labels))
			for k := range x.Labels {
				if k != "__name__" {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			var lbl []string
			for _, k := range keys {
				lbl = append(lbl, k+"="+x.Labels[k])
			}
			vals := make([]int64, len(x.Values))
			for j, v := range x.Values {
				vals[j] = int64(v * 1000)
			}
			lines = append(lines, fmt.Sprintf("%s  %12s  %s", s.OKText.Render(sparkline(vals, 30)), formatProm(x.Value), strings.Join(lbl, " ")))
		}
		if len(lines) == 0 {
			lines = []string{s.Muted.Render("no data")}
		}
		lines = append(lines, "", s.Muted.Render("last hour, 1-minute steps · source: "+p.Desc()))
		return fmt.Sprintf("PromQL · %d series · %s", len(series), q), strings.Join(lines, "\n"), nil
	})
}

func formatProm(v float64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.2fGi", v/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.1fMi", v/(1<<20))
	case v >= 1000:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.4g", v)
}

type peaksEntry struct {
	at    time.Time
	peaks kube.WorkloadPeaks
	note  string
}

var (
	peaksMu    sync.Mutex
	peaksCache = map[string]peaksEntry{}
)

// cachedPeaks returns long-window workload peaks from Prometheus, refreshed every 10 minutes
// (tables refresh every few seconds; the 7-day subqueries are expensive).
func (a *App) cachedPeaks(ctx context.Context, c *kube.Client) (kube.WorkloadPeaks, string) {
	key := c.Context + "|" + c.Namespace + "|" + a.promWindow()
	peaksMu.Lock()
	e, ok := peaksCache[key]
	peaksMu.Unlock()
	if ok && time.Since(e.at) < 10*time.Minute {
		return e.peaks, e.note
	}
	e = peaksEntry{at: time.Now()}
	if p, err := a.prometheusFor(ctx, c); err == nil {
		if lp, err := p.WorkloadPeaks(ctx, c.Namespace, a.promWindow()); err == nil {
			e.peaks, e.note = lp, " · peaks over "+a.promWindow()+" from Prometheus"
		} else {
			e.note = " · Prometheus: " + firstLineStr(err.Error(), 60)
		}
	}
	peaksMu.Lock()
	peaksCache[key] = e
	peaksMu.Unlock()
	return e.peaks, e.note
}
