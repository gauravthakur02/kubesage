package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// History keeps ~15 minutes of CPU/memory samples per pod (one sample per 10 s at most),
// so tables and the health report can show trends instead of a single number.
const (
	histKeep     = 90
	histInterval = 10 * time.Second
)

type sample struct {
	at       time.Time
	cpu, mem int64
}

type metricHistory struct {
	mu   sync.Mutex
	data map[string][]sample
}

func newHistory() *metricHistory { return &metricHistory{data: map[string][]sample{}} }

func histKey(ctx, ns, pod string) string { return ctx + "|" + ns + "|" + pod }

func (h *metricHistory) record(key string, u kube.Usage, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.data[key]
	if n := len(s); n > 0 && at.Sub(s[n-1].at) < histInterval {
		return
	}
	s = append(s, sample{at: at, cpu: u.CPUMilli, mem: u.MemBytes})
	if len(s) > histKeep {
		s = s[len(s)-histKeep:]
	}
	h.data[key] = s
}

func (h *metricHistory) get(key string) []sample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]sample(nil), h.data[key]...)
}

var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkline draws the last width values scaled between their own min and max.
func sparkline(vals []int64, width int) string {
	if len(vals) == 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = min(lo, v), max(hi, v)
	}
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if hi > lo {
			i = int((v - lo) * int64(len(sparkBlocks)-1) / (hi - lo))
		}
		b.WriteRune(sparkBlocks[i])
	}
	return b.String()
}

func series(s []sample, mem bool) []int64 {
	out := make([]int64, len(s))
	for i, x := range s {
		if mem {
			out[i] = x.mem
		} else {
			out[i] = x.cpu
		}
	}
	return out
}

// recordTable stores samples from a freshly listed pods table.
func (a *App) recordTable(ctx string, t kube.Table) {
	now := time.Now()
	for _, r := range t.Rows {
		if r.Usage != nil {
			a.hist.record(histKey(ctx, r.Namespace, r.Name), *r.Usage, now)
		}
	}
}

// trendSummary describes a series for the health report, e.g. "12m → 85m (min 9m, max 120m)".
func trendSummary(vals []int64, format func(int64) string) string {
	if len(vals) == 0 {
		return ""
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = min(lo, v), max(hi, v)
	}
	return fmt.Sprintf("%s → %s  (min %s, max %s)", format(vals[0]), format(vals[len(vals)-1]), format(lo), format(hi))
}
