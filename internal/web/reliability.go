package web

import (
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/gauravthakur02/kubeweave/internal/config"
	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// changes lists what changed recently in a namespace (rollouts, config, scaling, Helm, GitOps).
func (a *App) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	since := kube.ParseSince(q.Get("since"), 2*time.Hour)
	ctx, cancel := reqCtx(r, 40*time.Second)
	defer cancel()
	cs, err := c.Changes(ctx, time.Now().Add(-since))
	if err != nil {
		fail(w, 502, err)
		return
	}
	type ch struct{ Time, Source, Kind, NS, Name, What string }
	out := make([]ch, 0, len(cs))
	for _, x := range cs {
		out = append(out, ch{x.Time.Format(time.RFC3339), x.Source, x.Kind, x.Namespace, x.Name, x.What})
	}
	writeJSON(w, out)
}

// slos evaluates the configured SLOs for a context.
func (a *App) slos(w http.ResponseWriter, r *http.Request) {
	c, err := a.scopedFor(r, r.URL.Query().Get("ctx"), "all")
	if err != nil {
		fail(w, 400, err)
		return
	}
	var mine []config.SLO
	for _, s := range a.SLOs {
		if s.Context == "" {
			mine = append(mine, s)
		} else if m, err := regexp.MatchString(s.Context, c.Context); err == nil && m {
			mine = append(mine, s)
		}
	}
	type res struct {
		Name                                   string
		Objective, Availability, Budget, Burn1 float64
		Window, Error                          string
	}
	out := []res{}
	if len(mine) == 0 {
		writeJSON(w, out)
		return
	}
	u := ""
	if a.PromURL != nil {
		u = a.PromURL(c.Context)
	}
	ctx, cancel := reqCtx(r, 40*time.Second)
	defer cancel()
	p, err := c.NewProm(ctx, u, a.PromToken)
	if err != nil {
		fail(w, 502, fmt.Errorf("Prometheus: %w", err))
		return
	}
	for _, s := range mine {
		win := kube.ParseSince(s.Window, 30*24*time.Hour)
		v, err := p.EvalSLO(ctx, s.Good, s.Total, s.Objective, win)
		x := res{Name: s.Name, Objective: s.Objective, Window: s.Window, Availability: v.Availability, Budget: v.BudgetLeft, Burn1: v.Burn1h}
		if x.Window == "" {
			x.Window = "30d"
		}
		if err != nil {
			x.Error = err.Error()
		}
		out = append(out, x)
	}
	writeJSON(w, out)
}

// snapshots lists saved snapshots (GET) or saves one (POST {Ctx, NS, Name}).
func (a *App) snapshots(w http.ResponseWriter, r *http.Request) {
	if a.SnapshotBase == "" {
		fail(w, 501, fmt.Errorf("snapshots are not configured"))
		return
	}
	if r.Method == http.MethodPost {
		req, c, _, ok := a.decode(w, r)
		if !ok {
			return
		}
		ctx, cancel := reqCtx(r, time.Minute)
		defer cancel()
		s, err := c.TakeSnapshot(ctx, req.NS, req.Name)
		if err == nil {
			_, err = s.Save(a.SnapshotBase)
		}
		a.audit(r, c.Context, req.NS, "snapshot "+s.Name, "", err)
		if err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, map[string]string{"status": "saved snapshot " + s.Name})
		return
	}
	q := r.URL.Query()
	c, err := a.client(q.Get("ctx"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	ss, err := kube.ListSnapshots(a.SnapshotBase, c.Context, q.Get("ns"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	type snap struct{ Name, Taken, Objects string }
	out := []snap{}
	for _, s := range ss {
		out = append(out, snap{s.Name, s.Taken.Format(time.RFC3339), s.Objects[""]})
	}
	writeJSON(w, out)
}

// snapshotDiff compares a saved snapshot with the namespace now.
func (a *App) snapshotDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	old, err := kube.LoadSnapshot(a.SnapshotBase, c.Context, q.Get("ns"), q.Get("name"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	ctx, cancel := reqCtx(r, time.Minute)
	defer cancel()
	cur, err := c.TakeSnapshot(ctx, q.Get("ns"), "now")
	if err != nil {
		fail(w, 502, err)
		return
	}
	type change struct {
		Key, State string
		Diff       []string
	}
	out := []change{}
	for _, ch := range kube.DiffSnapshots(old, cur) {
		out = append(out, change{ch.Key, ch.State, diffOf(ch.Before, ch.After)})
	}
	writeJSON(w, map[string]any{"taken": old.Taken.Format(time.RFC3339), "changes": out})
}
