package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/config"
	"github.com/gauravthakur02/kubeweave/internal/kube"
)

const (
	historyRes   = "history"
	historyEvery = time.Minute
)

// eventsRecordedMsg reports one background recording of events into the local store.
type eventsRecordedMsg struct {
	ctx string
	n   int
	err error
}

func (a *App) historyOn() bool { return !a.cfg.EventHistory.Off }

func (a *App) historyDays() int {
	if a.cfg.EventHistory.Days <= 0 {
		return 7
	}
	return a.cfg.EventHistory.Days
}

func (a *App) eventStore() *kube.EventStore {
	if a.evStore == nil {
		p := a.cfg.Path()
		if p == "" {
			p = config.DefaultPath()
		}
		a.evStore = &kube.EventStore{Dir: filepath.Join(filepath.Dir(p), "events")}
	}
	return a.evStore
}

// recordEvents stores the current events of every cluster shown in a pane (once a minute, in the background).
func (a *App) recordEvents() tea.Cmd {
	if !a.historyOn() || a.evBusy || time.Since(a.evAt) < historyEvery || a.launcher {
		return nil
	}
	a.evBusy, a.evAt = true, time.Now()
	clients := map[string]*kube.Client{}
	for _, p := range a.panes() {
		if p.client != nil {
			key := p.client.Context + "\x00" + p.client.Namespace
			clients[key] = p.client
		}
	}
	store, keep := a.eventStore(), time.Duration(a.historyDays())*24*time.Hour
	prune := map[string]bool{}
	if a.evPruned == nil {
		a.evPruned = map[string]bool{}
	}
	for _, c := range clients {
		if !a.evPruned[c.Context] {
			a.evPruned[c.Context], prune[c.Context] = true, true
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out := eventsRecordedMsg{}
		for _, c := range clients {
			if prune[c.Context] {
				_ = store.Prune(c.Context, keep)
			}
			evs, err := c.CollectEvents(ctx)
			if err != nil {
				out.err = err
				continue
			}
			n, err := store.Record(c.Context, evs)
			out.ctx, out.n = c.Context, out.n+n
			if err != nil {
				out.err = err
			}
		}
		return out
	}
}

func (a *App) handleEventsRecorded(m eventsRecordedMsg) tea.Cmd {
	a.evBusy = false
	if a.res.Name == historyRes && a.mode == modeTable && m.n > 0 {
		return a.refresh()
	}
	return nil
}

// storedHistory loads the local event history of the focused cluster.
func (a *App) storedHistory() []kube.StoredEvent {
	if !a.historyOn() {
		return nil
	}
	evs, _ := a.eventStore().Load(a.client.Context, time.Now().Add(-time.Duration(a.historyDays())*24*time.Hour))
	return evs
}

func (a *App) historyResource() kube.Resource {
	store, kctx, ns := a.eventStore(), a.client.Context, a.client.Namespace
	days, on := a.historyDays(), a.historyOn()
	return kube.LocalResource(historyRes, "Stored event", "History", func() kube.Table {
		t := kube.Table{Columns: []string{"LAST SEEN", "TYPE", "NAMESPACE", "OBJECT", "REASON", "COUNT", "MESSAGE"}}
		if !on {
			t.Note = "event history is off (event_history.off in the config)"
			return t
		}
		evs, err := store.Load(kctx, time.Now().Add(-time.Duration(days)*24*time.Hour))
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		warn := 0
		for _, e := range evs {
			if ns != kube.AllNamespaces && e.Namespace != ns {
				continue
			}
			h := kube.EventHealth(e.Type, e.Reason)
			if h != kube.HealthNeutral {
				warn++
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: e.Namespace, Name: e.Kind + "/" + e.Name, Health: h,
				Cells: []string{kube.Age(metav1.NewTime(e.Last)) + " ago", e.Type, e.Namespace, e.Kind + "/" + e.Name, e.Reason,
					fmt.Sprint(e.Count), e.Message}})
		}
		t.Note = fmt.Sprintf("%d events kept locally (last %d days) · %d warnings · recorded every minute while kubeweave runs", len(t.Rows), days, warn)
		return t
	})
}

func (a *App) openHistory() tea.Cmd {
	return a.openVirtual(a.historyResource(), "events kept on this computer · enter: timeline of the object · / filter")
}

func init() {
	registerScreen(historyRes, screenDef{
		title: func(a *App) string {
			return "Event history · " + a.client.Context + " · " + a.client.NamespaceLabel() + " · " + a.table.Note
		},
		hints: []kv{{"enter", "timeline"}, {"/", "filter"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			if key != "enter" {
				return nil, false
			}
			kind, name, _ := strings.Cut(row.Name, "/")
			return a.openTimeline(kind, row.Namespace, name), true
		},
	})
}
