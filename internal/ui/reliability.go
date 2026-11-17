package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

const (
	changesRes   = "changes"
	sloRes       = "slos"
	snapshotsRes = "snapshots"
)

// ---------- what changed? ----------

func (a *App) changesResource(since time.Duration) kube.Resource {
	c := a.client
	return kube.VirtualResource(changesRes, "Change", "Changes", func() kube.Table {
		t := kube.Table{Columns: []string{"WHEN", "SOURCE", "KIND", "NAMESPACE", "NAME", "WHAT"}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		cs, err := c.Changes(ctx, time.Now().Add(-since))
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		for _, ch := range cs {
			h := kube.HealthNeutral
			if ch.Source == "rollout" || ch.Source == "helm" || ch.Source == "gitops" {
				h = kube.HealthWarn
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: ch.Namespace, Name: ch.Kind + "/" + ch.Name, Health: h,
				Cells: []string{kube.Age(metav1.NewTime(ch.Time)) + " ago", ch.Source, ch.Kind, ch.Namespace, ch.Name, ch.What}})
		}
		t.Note = fmt.Sprintf("%d changes in the last %s · rollouts, config, scaling, Helm, GitOps", len(cs), shortDuration(since))
		return t
	})
}

// openChanges answers "what changed?" (:changes [2h]).
func (a *App) openChanges(arg string) tea.Cmd {
	since := kube.ParseSince(arg, 2*time.Hour)
	return a.openVirtual(a.changesResource(since), "what changed in "+a.client.NamespaceLabel()+" · enter: timeline of the object")
}

// ---------- SLOs ----------

func slosFor(cfg *config.Config, kctx string) []config.SLO {
	var out []config.SLO
	for _, s := range cfg.SLOs {
		if s.Context != "" {
			if m, err := regexp.MatchString(s.Context, kctx); err != nil || !m {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

func (a *App) sloResource() kube.Resource {
	c, cfg := a.client, a.cfg
	return kube.VirtualResource(sloRes, "SLO", "SLOs", func() kube.Table {
		t := kube.Table{Columns: []string{"SLO", "OBJECTIVE", "WINDOW", "AVAILABILITY", "BUDGET LEFT", "BURN (1h)", "STATUS"}}
		slos := slosFor(cfg, c.Context)
		if len(slos) == 0 {
			t.Note = "no SLOs for " + c.Context + " · add slos: to the config"
			return t
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		u := cfg.Prometheus.URLs[c.Context]
		if u == "" {
			u = cfg.Prometheus.URLs["*"]
		}
		p, err := c.NewProm(ctx, u, cfg.Prometheus.BearerTokenCommand)
		if err != nil {
			t.Note = "✗ Prometheus: " + err.Error()
			return t
		}
		bad := 0
		for _, s := range slos {
			w := kube.ParseSince(s.Window, 30*24*time.Hour)
			r, err := p.EvalSLO(ctx, s.Good, s.Total, s.Objective, w)
			row := kube.Row{Name: s.Name, Health: kube.HealthOK}
			status := "✓ within budget"
			switch {
			case err != nil:
				row.Health, status = kube.HealthErr, "✗ "+firstWords(err.Error(), 8)
			case r.BudgetLeft < 0:
				row.Health, status, bad = kube.HealthErr, "✗ budget spent", bad+1
			case r.Burn1h > 14.4:
				row.Health, status, bad = kube.HealthErr, "✗ burning fast (page)", bad+1
			case r.Burn1h > 1 || r.BudgetLeft < 0.25:
				row.Health, status = kube.HealthWarn, "▲ at risk"
			}
			row.Cells = []string{s.Name, fmt.Sprintf("%.3f%%", s.Objective*100), shortDuration(w), fmt.Sprintf("%.3f%%", r.Availability*100),
				fmt.Sprintf("%.0f%%", r.BudgetLeft*100), fmt.Sprintf("%.1fx", r.Burn1h), status}
			t.Rows = append(t.Rows, row)
		}
		t.Note = fmt.Sprintf("%d SLOs · %d in trouble · burn 1x = spending the budget exactly over the window", len(slos), bad)
		return t
	})
}

// ---------- snapshots ----------

func (a *App) snapshotBase() string {
	p := a.cfg.Path()
	if p == "" {
		p = config.DefaultPath()
	}
	return filepath.Join(filepath.Dir(p), "snapshots")
}

// snapshotCommand: :snapshot [save] [name] · :snapshot diff <name> · :snapshots
func (a *App) snapshotCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	ns := a.client.Namespace
	if ns == kube.AllNamespaces {
		a.setStatus("pick a namespace first (n): snapshots are per namespace", true)
		return nil
	}
	c, base := a.client, a.snapshotBase()
	switch {
	case len(f) == 0 || f[0] == "list":
		return a.openVirtual(a.snapshotsResource(), "snapshots of "+ns+" · enter: compare with now")
	case f[0] == "diff" && len(f) > 1:
		return a.snapshotDiff(f[1])
	default:
		name := ""
		if f[0] == "save" && len(f) > 1 {
			name = f[1]
		} else if f[0] != "save" {
			name = f[0]
		}
		return runAction("snapshot "+ns, func(ctx context.Context) error {
			s, err := c.TakeSnapshot(ctx, ns, name)
			if err != nil {
				return err
			}
			_, err = s.Save(base)
			return err
		})
	}
}

func (a *App) snapshotsResource() kube.Resource {
	c, ns, base := a.client, a.client.Namespace, a.snapshotBase()
	return kube.LocalResource(snapshotsRes, "Snapshot", "Snapshots", func() kube.Table {
		t := kube.Table{Columns: []string{"NAME", "TAKEN", "OBJECTS"}}
		ss, err := kube.ListSnapshots(base, c.Context, ns)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		for _, s := range ss {
			t.Rows = append(t.Rows, kube.Row{Name: s.Name, Health: kube.HealthNeutral, Cells: []string{s.Name, kube.Age(metav1.NewTime(s.Taken)) + " ago", s.Objects[""]}})
		}
		t.Note = fmt.Sprintf("%d snapshots of %s · :snapshot save [name] · kubesage snapshot save from cron", len(ss), ns)
		return t
	})
}

// snapshotDiff compares a saved snapshot with the namespace now.
func (a *App) snapshotDiff(name string) tea.Cmd {
	c, ns, base := a.client, a.client.Namespace, a.snapshotBase()
	return a.openReport("snapshotdiff", "comparing with snapshot "+name+"…", func(context.Context) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		old, err := kube.LoadSnapshot(base, c.Context, ns, name)
		if err != nil {
			return "", "", err
		}
		cur, err := c.TakeSnapshot(ctx, ns, "now")
		if err != nil {
			return "", "", err
		}
		changes := kube.DiffSnapshots(old, cur)
		s := a.st
		var lines []string
		for _, ch := range changes {
			switch ch.State {
			case "added":
				lines = append(lines, s.OKText.Bold(true).Render("+ added   "+ch.Key))
			case "removed":
				lines = append(lines, s.ErrText.Bold(true).Render("- removed "+ch.Key))
			default:
				lines = append(lines, s.WarnText.Bold(true).Render("~ changed "+ch.Key))
				lines = append(lines, a.renderDiffLines(diffLines(strings.Split(ch.Before, "\n"), strings.Split(ch.After, "\n"), 2))...)
			}
			lines = append(lines, "")
		}
		if len(changes) == 0 {
			lines = append(lines, s.OKText.Render("✓ nothing changed since "+old.Taken.Local().Format("2006-01-02 15:04")))
		}
		return fmt.Sprintf("snapshot %s (%s) → now · %d changes in %s", name, kube.Age(metav1.NewTime(old.Taken))+" ago", len(changes), ns), strings.Join(lines, "\n"), nil
	})
}

func init() {
	registerScreen(changesRes, screenDef{
		title: func(a *App) string { return "What changed · " + a.client.NamespaceLabel() + " · " + a.table.Note },
		hints: []kv{{"enter", "timeline"}, {"/", "filter"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			if key != "enter" {
				return nil, false
			}
			kind, name, _ := strings.Cut(row.Name, "/")
			return a.openTimeline(kind, row.Namespace, name), true
		},
	})
	registerScreen(sloRes, screenDef{
		title: func(a *App) string { return "SLOs · " + a.client.Context + " · " + a.table.Note },
		hints: []kv{{"r", "refresh"}, {"esc", "back"}},
	})
	registerScreen(snapshotsRes, screenDef{
		title: func(a *App) string { return "Snapshots · " + a.table.Note },
		hints: []kv{{"enter", "compare with now"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			if key != "enter" {
				return nil, false
			}
			return a.snapshotDiff(row.Name), true
		},
	})
}
