package ui

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

const schedulesRes = "schedules"

func scheduleWindow(s config.Schedule) string {
	days := "mon-fri"
	if len(s.Days) > 0 {
		days = strings.Join(s.Days, ",")
	}
	return fmt.Sprintf("down %s · up %s · %s", s.Down, s.Up, days)
}

// scheduleBlocked explains why a schedule may not touch a context ("" = allowed).
func scheduleBlocked(cfg *config.Config, s config.Schedule, kctx string) string {
	if s.Namespace == "" || s.Namespace == "all" {
		return "namespace is required"
	}
	if !s.AllowDanger && cfg.DangerContexts != "" {
		if re, err := regexp.Compile(cfg.DangerContexts); err == nil && re.MatchString(kctx) {
			return "production-like context (set allow_danger: true to allow)"
		}
	}
	return ""
}

// RunSchedules applies every schedule to matching contexts: scale to zero or restore, as the time says.
// It is what `kubesage schedule run` does (run it from cron every 5–15 minutes).
func RunSchedules(cfg *config.Config, kubeconfig string, now time.Time, dryRun bool, out io.Writer) error {
	if len(cfg.Schedules) == 0 {
		return fmt.Errorf("no schedules in the config (schedules:)")
	}
	contexts := []string{}
	if c, err := kube.KubeContexts(kubeconfig); err == nil {
		for _, kc := range c {
			contexts = append(contexts, kc.Name)
		}
	} else {
		return err
	}
	var firstErr error
	for _, s := range cfg.Schedules {
		re, err := regexp.Compile(s.Context)
		if err != nil || s.Context == "" {
			fmt.Fprintf(out, "✗ %s: context must be a regular expression\n", s.Name)
			continue
		}
		down, err := kube.ShouldBeDown(now, s.Down, s.Up, s.Days)
		if err != nil {
			fmt.Fprintf(out, "✗ %s: %v\n", s.Name, err)
			continue
		}
		for _, kc := range contexts {
			if !re.MatchString(kc) {
				continue
			}
			if why := scheduleBlocked(cfg, s, kc); why != "" {
				fmt.Fprintf(out, "– %s @ %s: skipped, %s\n", s.Name, kc, why)
				continue
			}
			c, err := kube.New(kubeconfig, kc, s.Namespace)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				var ws []kube.ScheduledWorkload
				var done []string
				if ws, err = c.ScheduleTargets(ctx, s.Namespace, s.Selector); err == nil {
					if down {
						done, err = c.ScaleToZero(ctx, ws, dryRun)
					} else {
						done, err = c.RestoreScale(ctx, ws, dryRun)
					}
				}
				cancel()
				verb := "up"
				if down {
					verb = "down"
				}
				prefix := ""
				if dryRun {
					prefix = "(dry run) "
				}
				fmt.Fprintf(out, "%s%s @ %s/%s should be %s: %d change(s)\n", prefix, s.Name, kc, s.Namespace, verb, len(done))
				for _, d := range done {
					fmt.Fprintln(out, "    "+d)
				}
			}
			if err != nil {
				fmt.Fprintf(out, "✗ %s @ %s: %v\n", s.Name, kc, err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// schedulesResource lists the schedules that apply to the focused context.
func (a *App) schedulesResource() kube.Resource {
	cfg, c := a.cfg, a.client
	return kube.VirtualResource(schedulesRes, "Schedule", "Schedules", func() kube.Table {
		t := kube.Table{Columns: []string{"NAME", "NAMESPACE", "SELECTOR", "WINDOW", "NOW", "WORKLOADS", "STATUS"}}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		n := 0
		for i, s := range cfg.Schedules {
			re, err := regexp.Compile(s.Context)
			if err != nil || s.Context == "" || !re.MatchString(c.Context) {
				continue
			}
			n++
			want, h := "up", kube.HealthOK
			if d, err := kube.ShouldBeDown(time.Now(), s.Down, s.Up, s.Days); err != nil {
				want, h = "✗ "+err.Error(), kube.HealthErr
			} else if d {
				want = "down"
			}
			status, wl := "", ""
			if why := scheduleBlocked(cfg, s, c.Context); why != "" {
				status, h = "blocked: "+why, kube.HealthWarn
			} else if ws, err := c.ScheduleTargets(ctx, s.Namespace, s.Selector); err != nil {
				status, h = "✗ "+firstWords(err.Error(), 8), kube.HealthErr
			} else {
				running, parked := 0, 0
				for _, w := range ws {
					if w.Saved > 0 {
						parked++
					} else if w.Replicas > 0 {
						running++
					}
				}
				wl = fmt.Sprintf("%d running · %d scaled down", running, parked)
				switch {
				case want == "down" && running > 0:
					status, h = "▲ should be down (run kubesage schedule run, or d)", kube.HealthWarn
				case want == "up" && parked > 0:
					status, h = "▲ should be up (u restores)", kube.HealthWarn
				default:
					status = "✓ in step"
				}
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: s.Namespace, Name: fmt.Sprint(i), Health: h,
				Cells: []string{s.Name, s.Namespace, s.Selector, scheduleWindow(s), want, wl, status}})
		}
		t.Note = fmt.Sprintf("%d schedules for %s · applied by `kubesage schedule run` (cron)", n, c.Context)
		if n == 0 {
			t.Note = "no schedules match " + c.Context + " · add schedules: to the config"
		}
		return t
	})
}

func (a *App) openSchedules() tea.Cmd {
	return a.openVirtual(a.schedulesResource(), "scale-to-zero schedules · d scale down now · u restore now")
}

func (a *App) scheduleNow(row kube.Row, down bool) tea.Cmd {
	var idx int
	if _, err := fmt.Sscan(row.Name, &idx); err != nil || idx >= len(a.cfg.Schedules) {
		return nil
	}
	s := a.cfg.Schedules[idx]
	if why := scheduleBlocked(a.cfg, s, a.client.Context); why != "" {
		a.setStatus("schedule blocked: "+why, true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	c := a.client
	verb := "restore saved replicas of"
	if down {
		verb = "scale to zero"
	}
	a.askConfirm(fmt.Sprintf("%s every workload of schedule %s in %s? (y/n)", verb, s.Name, s.Namespace), "", func() tea.Cmd {
		return runAction("schedule "+s.Name+": "+verb, func(ctx context.Context) error {
			ws, err := c.ScheduleTargets(ctx, s.Namespace, s.Selector)
			if err == nil {
				if down {
					_, err = c.ScaleToZero(ctx, ws, false)
				} else {
					_, err = c.RestoreScale(ctx, ws, false)
				}
			}
			return err
		})
	})
	return nil
}

func init() {
	registerScreen(schedulesRes, screenDef{
		title: func(a *App) string { return "Schedules · " + a.table.Note },
		hints: []kv{{"d", "scale down now"}, {"u", "restore now"}, {"r", "refresh"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			switch key {
			case "d":
				return a.scheduleNow(row, true), true
			case "u":
				return a.scheduleNow(row, false), true
			}
			return nil, false
		},
	})
}
