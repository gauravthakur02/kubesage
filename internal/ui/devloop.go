package ui

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// ---------- dev loop ----------

const devEvery = 2 * time.Second

type devState struct {
	path, ns, ctx string
	stamp         string
	runs          int
	busy          bool
}

type devTickMsg struct{ gen int }

type devAppliedMsg struct {
	gen     int
	applied int
	same    int
	errs    []string
	err     error
}

// stampOf is a cheap fingerprint of a file or folder: names, sizes and modification times.
func stampOf(path string) string {
	var b strings.Builder
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			fmt.Fprintf(&b, "%s:%d:%d;", p, info.Size(), info.ModTime().UnixNano())
		}
		return nil
	})
	return b.String()
}

// devCommand watches local manifests and applies them to the current namespace on every save (:dev <path>, :dev stop).
// It refuses production-like contexts and "all namespaces".
func (a *App) devCommand(arg string) tea.Cmd {
	arg = strings.TrimSpace(arg)
	if arg == "stop" || arg == "off" {
		if a.dev == nil {
			a.setStatus("dev loop is not running", false)
			return nil
		}
		a.setStatus(fmt.Sprintf("dev loop stopped after %d applies", a.dev.runs), false)
		a.dev = nil
		a.devGen++
		return nil
	}
	if arg == "" {
		if a.dev != nil {
			a.setStatus(fmt.Sprintf("dev: watching %s → %s/%s · %d applies · :dev stop", a.dev.path, a.dev.ctx, a.dev.ns, a.dev.runs), false)
			return nil
		}
		a.setStatus("usage: :dev <file or folder> (kustomize, Helm chart or YAML) · :dev stop", true)
		return nil
	}
	if a.dangerFor(a.client.Context) {
		a.setStatus("the dev loop is disabled on production-like contexts (danger_contexts)", true)
		return nil
	}
	if a.client.Namespace == kube.AllNamespaces {
		a.setStatus("pick a namespace first (n): the dev loop applies into one namespace", true)
		return nil
	}
	if _, err := os.Stat(arg); err != nil {
		a.setStatus(err.Error(), true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	a.writeIntent = false
	abs, _ := filepath.Abs(arg)
	a.dev = &devState{path: abs, ns: a.client.Namespace, ctx: a.client.Context}
	a.devGen++
	a.audit("dev loop start "+abs, a.client.Context, a.client.Namespace, "", nil)
	a.setStatus("● dev: watching "+abs+" → "+a.client.Namespace+" (applies on every save) · :dev stop", false)
	return a.devTick()
}

func (a *App) devTick() tea.Cmd {
	gen := a.devGen
	return tea.Tick(devEvery, func(time.Time) tea.Msg { return devTickMsg{gen: gen} })
}

func (a *App) handleDevTick(m devTickMsg) tea.Cmd {
	d := a.dev
	if d == nil || m.gen != a.devGen {
		return nil
	}
	st := stampOf(d.path)
	if st == d.stamp || d.busy {
		return a.devTick()
	}
	d.stamp, d.busy = st, true
	client, ok := a.cl.client(d.ctx)
	if !ok {
		client = a.client
	}
	c, path, ns, gen := client.WithNamespace(d.ns), d.path, d.ns, a.devGen
	return tea.Batch(a.devTick(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		y, _, err := kube.RenderLocal(ctx, path)
		if err != nil {
			return devAppliedMsg{gen: gen, err: err}
		}
		objs, err := kube.ParseManifests(y)
		if err != nil {
			return devAppliedMsg{gen: gen, err: err}
		}
		plan, err := c.PlanApply(ctx, objs, ns)
		if err != nil {
			return devAppliedMsg{gen: gen, err: err}
		}
		same := 0
		var errs []string
		for _, it := range plan {
			switch it.State {
			case "unchanged":
				same++
			case "error":
				errs = append(errs, it.Kind+"/"+it.Name+": "+firstLineStr(fmt.Sprint(it.Err), 160))
			}
		}
		n, aerrs := c.ExecuteApply(ctx, plan)
		return devAppliedMsg{gen: gen, applied: n, same: same, errs: append(errs, aerrs...)}
	})
}

func (a *App) handleDevApplied(m devAppliedMsg) tea.Cmd {
	if a.dev == nil || m.gen != a.devGen {
		return nil
	}
	a.dev.busy = false
	if m.err != nil {
		a.setStatus("dev: "+m.err.Error(), true)
		return nil
	}
	a.dev.runs++
	if m.applied > 0 {
		a.audit(fmt.Sprintf("dev loop apply (%d objects)", m.applied), a.dev.ctx, a.dev.ns, a.dev.path, nil)
	}
	msg := fmt.Sprintf("dev %s: %d applied · %d unchanged", time.Now().Format("15:04:05"), m.applied, m.same)
	if len(m.errs) > 0 {
		a.setStatus(msg+" · ✗ "+m.errs[0], true)
		return nil
	}
	a.setStatus("● "+msg+" · :dev stop", false)
	if a.mode == modeTable {
		return a.refresh()
	}
	return nil
}

// ---------- namespace clones ----------

const clonesRes = "clones"

// cloneCommand copies the current namespace into a new one for a preview environment (:clone <new-ns> [ttl]).
func (a *App) cloneCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	src := a.client.Namespace
	if len(f) == 0 {
		return a.openVirtual(a.clonesResource(), "namespace clones · D delete · :clone <new-namespace> [24h]")
	}
	if a.dangerFor(a.client.Context) {
		a.setStatus("cloning namespaces is disabled on production-like contexts", true)
		return nil
	}
	if src == kube.AllNamespaces {
		a.setStatus("pick the namespace to clone first (n)", true)
		return nil
	}
	ttl := 24 * time.Hour
	if len(f) > 1 {
		d, err := time.ParseDuration(f[1])
		if err != nil {
			a.setStatus("ttl: "+err.Error(), true)
			return nil
		}
		ttl = d
	}
	if !a.guardWrite() {
		return nil
	}
	dst, c := f[0], a.client
	a.askConfirm(fmt.Sprintf("Copy deployments, statefulsets, cronjobs, services, configmaps and secrets of %s into a new namespace %s (expires in %s)? Ingresses and volumes are not copied.",
		src, dst, shortDuration(ttl)), "", func() tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			res, err := c.CloneNamespace(ctx, src, dst, ttl)
			desc := "clone " + src + " → " + dst
			if err == nil {
				var parts []string
				for k, n := range res.Copied {
					parts = append(parts, fmt.Sprintf("%d %s", n, k))
				}
				desc += ": " + strings.Join(parts, ", ")
				if len(res.Skipped) > 0 {
					desc += fmt.Sprintf(" · %d skipped (%s)", len(res.Skipped), firstWords(res.Skipped[0], 10))
				}
			}
			return actionMsg{status: desc, err: err}
		}
	})
	return nil
}

func (a *App) clonesResource() kube.Resource {
	c := a.client
	return kube.VirtualResource(clonesRes, "Clone", "Clones", func() kube.Table {
		t := kube.Table{Columns: []string{"NAMESPACE", "CLONE OF", "AGE", "EXPIRES"}}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cl, err := c.Clones(ctx)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		expired := 0
		for _, x := range cl {
			h, exp := kube.HealthOK, "in "+shortDuration(time.Until(x.Expires).Round(time.Minute))
			if !x.Expires.IsZero() && time.Now().After(x.Expires) {
				h, exp, expired = kube.HealthWarn, "expired "+kube.Age(metav1.NewTime(x.Expires))+" ago", expired+1
			}
			t.Rows = append(t.Rows, kube.Row{Name: x.Name, Health: h, Cells: []string{x.Name, x.Of, kube.Age(metav1.NewTime(x.Created)), exp}})
		}
		t.Note = fmt.Sprintf("%d clones · %d expired · D deletes one · enter opens it", len(cl), expired)
		return t
	})
}

func init() {
	registerScreen(clonesRes, screenDef{
		title: func(a *App) string { return "Namespace clones · " + a.client.Context + " · " + a.table.Note },
		hints: []kv{{"enter", "open"}, {"D", "delete"}, {"r", "refresh"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			switch key {
			case "enter":
				return a.setNamespace(row.Name), true
			case "D":
				if !a.guardWrite() {
					return nil, true
				}
				c := a.client
				a.askConfirm("Delete clone namespace "+row.Name+" and everything in it?", row.Name, func() tea.Cmd {
					return runAction("delete clone "+row.Name, func(ctx context.Context) error { return c.DeleteClone(ctx, row.Name) })
				})
				return nil, true
			}
			return nil, false
		},
	})
}
