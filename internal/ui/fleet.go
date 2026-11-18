package ui

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

const fleetRes = "fleet"

// fleetContexts are the contexts in the fleet view: config `fleet` patterns, else every connected cluster.
func (a *App) fleetContexts() []string {
	if len(a.cfg.Fleet) == 0 {
		return a.cl.connected()
	}
	var res []*regexp.Regexp
	for _, p := range a.cfg.Fleet {
		if re, err := regexp.Compile(p); err == nil {
			res = append(res, re)
		}
	}
	var out []string
	for _, c := range a.client.Contexts() {
		for _, re := range res {
			if re.MatchString(c) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func (a *App) fleetResource() kube.Resource {
	contexts, kubeconfig, store, danger := a.fleetContexts(), a.client.Kubeconfig, a.cl, a.dangerFor
	return kube.VirtualResource(fleetRes, "Cluster", "Fleet", func() kube.Table {
		t := kube.Table{Columns: []string{"CONTEXT", "STATUS", "VERSION", "API", "NODES", "PODS", "ISSUES", "WARN EVENTS"}}
		type result struct {
			s   kube.FleetStat
			err error
		}
		res := make([]result, len(contexts))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		for i, name := range contexts {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				c, ok := store.client(name)
				if !ok {
					nc, err := kube.New(kubeconfig, name, "")
					if err != nil {
						res[i].err = err
						return
					}
					store.add(nc)
					c, _ = store.client(name)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				defer cancel()
				res[i].s, res[i].err = c.Fleet(ctx)
			}(i, name)
		}
		wg.Wait()
		bad := 0
		for i, name := range contexts {
			r := res[i]
			label := name
			if danger(name) {
				label += "  ⚠ prod"
			}
			if r.err != nil {
				bad++
				t.Rows = append(t.Rows, kube.Row{Name: name, Health: kube.HealthErr, Cells: []string{label, "✗ " + firstWords(r.err.Error(), 8), "", "", "", "", "", ""}})
				continue
			}
			s := r.s
			nodes := "no access"
			if s.Nodes >= 0 {
				nodes = fmt.Sprintf("%d/%d", s.Ready, s.Nodes)
			}
			status := "✓ healthy"
			switch s.Health() {
			case kube.HealthErr:
				status, bad = "✗ problems", bad+1
			case kube.HealthWarn:
				status = "▲ attention"
			}
			issues := fmt.Sprint(s.Issues)
			if s.Errors > 0 {
				issues = fmt.Sprintf("%d (%d critical)", s.Issues, s.Errors)
			}
			t.Rows = append(t.Rows, kube.Row{Name: name, Health: s.Health(), Cells: []string{label, status, s.Version,
				s.Latency.Round(time.Millisecond).String(), nodes, fmt.Sprintf("%d/%d", s.Running, s.Pods), issues, fmt.Sprint(s.Warnings)}})
		}
		sort.SliceStable(t.Rows, func(i, j int) bool { return t.Rows[i].Health == kube.HealthErr && t.Rows[j].Health != kube.HealthErr })
		t.Note = fmt.Sprintf("%d clusters · %d with problems · each in its default namespace", len(contexts), bad)
		if len(contexts) == 0 {
			t.Note = "no clusters: connect some (C) or list them under fleet: in the config"
		}
		return t
	})
}

func (a *App) openFleet() tea.Cmd {
	return a.openVirtual(a.fleetResource(), "fleet health of "+fmt.Sprint(len(a.fleetContexts()))+" clusters · enter: open in a new tab · r refresh")
}

func init() {
	registerScreen(fleetRes, screenDef{
		title: func(a *App) string { return "Fleet · " + a.table.Note },
		hints: []kv{{"enter", "open in new tab"}, {"r", "refresh"}, {"/", "filter"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			if key != "enter" {
				return nil, false
			}
			return a.connect(row.Name, "", "newtab"), true
		},
	})
}
