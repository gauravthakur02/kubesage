package kube

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SLOResult is the state of one service level objective.
type SLOResult struct {
	Availability float64 // good / total over the window, 0..1
	BudgetLeft   float64 // share of the error budget still unspent (can go below 0)
	Burn1h       float64 // error-budget burn rate over the last hour (1 = exactly on budget)
	Total        float64 // number of events in the window
}

func promDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func (p *Prom) scalar(ctx context.Context, q string) (float64, error) {
	s, err := p.Query(ctx, q)
	if err != nil {
		return 0, err
	}
	if len(s) == 0 {
		return 0, nil
	}
	var sum float64
	for _, x := range s {
		sum += x.Value
	}
	return sum, nil
}

// EvalSLO computes an SLO from two PromQL counters: good and total events. {{window}} in the queries is replaced by
// the window (e.g. 30d), so a query looks like sum(increase(http_requests_total{code!~"5.."}[{{window}}])).
func (p *Prom) EvalSLO(ctx context.Context, good, total string, objective float64, window time.Duration) (SLOResult, error) {
	var r SLOResult
	if objective <= 0 || objective >= 1 {
		return r, fmt.Errorf("objective must be between 0 and 1 (e.g. 0.999)")
	}
	q := func(s string, w time.Duration) string { return strings.ReplaceAll(s, "{{window}}", promDuration(w)) }
	g, err := p.scalar(ctx, q(good, window))
	if err != nil {
		return r, err
	}
	t, err := p.scalar(ctx, q(total, window))
	if err != nil {
		return r, err
	}
	r.Total = t
	if t <= 0 {
		r.Availability, r.BudgetLeft = 1, 1
		return r, nil
	}
	r.Availability = g / t
	budget := 1 - objective
	r.BudgetLeft = 1 - (1-r.Availability)/budget
	g1, err1 := p.scalar(ctx, q(good, time.Hour))
	t1, err2 := p.scalar(ctx, q(total, time.Hour))
	if err1 == nil && err2 == nil && t1 > 0 {
		r.Burn1h = (1 - g1/t1) / budget
	}
	return r, nil
}
