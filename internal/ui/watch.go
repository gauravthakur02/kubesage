package ui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"kubesage/internal/kube"
)

const (
	watchDebounce = 1200 * time.Millisecond // at most one refresh per pane per this interval
	watchResync   = 30 * time.Second        // full re-list even while a watch is healthy
	watchRetry    = 30 * time.Second        // after a failed watch, poll for this long before retrying
)

// paneWatch is a live watch feeding one table pane.
type paneWatch struct {
	key     string
	cancel  context.CancelFunc
	events  chan struct{} // buffered(1): coalesces bursts of changes
	done    chan error
	failed  bool
	err     error
	retryAt time.Time
}

type watchEventMsg struct {
	pane *pane
	w    *paneWatch
}

type watchEndMsg struct {
	pane *pane
	w    *paneWatch
	err  error
}

func (a *App) liveWatch() bool { return a.cfg.LiveUpdates != "poll" }

func watchKey(p *pane) string { return p.client.Context + "|" + p.client.Namespace + "|" + p.res.Name }

func (a *App) wantsWatch(p *pane) bool {
	return a.liveWatch() && p.mode == modeTable && p.client != nil && p.res.Watchable() && !(a.zoomed && p != a.pane)
}

// ensureWatches starts watches for visible table panes and stops stale ones.
func (a *App) ensureWatches() []tea.Cmd {
	if a.watches == nil {
		a.watches = map[*pane]*paneWatch{}
	}
	visible := map[*pane]bool{}
	var cmds []tea.Cmd
	for _, p := range a.panes() {
		visible[p] = true
		w := a.watches[p]
		if !a.wantsWatch(p) {
			a.stopWatch(p)
			continue
		}
		key := watchKey(p)
		if w != nil && w.key != key {
			a.stopWatch(p)
			w = nil
		}
		if w != nil && (!w.failed || time.Now().Before(w.retryAt)) {
			continue
		}
		cmds = append(cmds, a.startWatch(p, key))
	}
	for p := range a.watches {
		if !visible[p] {
			a.stopWatch(p)
		}
	}
	return cmds
}

func (a *App) stopWatch(p *pane) {
	if w := a.watches[p]; w != nil {
		if w.cancel != nil {
			w.cancel()
		}
		delete(a.watches, p)
	}
}

func (a *App) startWatch(p *pane, key string) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	w := &paneWatch{key: key, cancel: cancel, events: make(chan struct{}, 1), done: make(chan error, 1)}
	a.watches[p] = w
	c, r := p.client, p.res
	go func() {
		w.done <- c.WatchChanges(ctx, r, func() {
			select {
			case w.events <- struct{}{}:
			default:
			}
		})
	}()
	return waitWatch(p, w)
}

func waitWatch(p *pane, w *paneWatch) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-w.events:
			return watchEventMsg{pane: p, w: w}
		case err := <-w.done:
			return watchEndMsg{pane: p, w: w, err: err}
		}
	}
}

// watching reports whether p is currently fed by a healthy watch.
func (a *App) watching(p *pane) bool {
	w := a.watches[p]
	return w != nil && !w.failed && w.key == watchKey(p)
}

func (a *App) handleWatchEvent(m watchEventMsg) tea.Cmd {
	if a.watches[m.pane] != m.w || !a.hasPane(m.pane) {
		return nil // stale watch
	}
	p := m.pane
	if wait := watchDebounce - time.Since(p.lastList); wait > 0 || p.loading {
		if wait <= 0 {
			wait = watchDebounce
		}
		if p.watchPending {
			return nil
		}
		p.watchPending = true
		return tea.Tick(wait, func(time.Time) tea.Msg { return watchEventMsg{pane: p, w: m.w} })
	}
	p.watchPending = false
	return tea.Batch(a.refreshPane(p), waitWatch(p, m.w))
}

func (a *App) handleWatchEnd(m watchEndMsg) tea.Cmd {
	if a.watches[m.pane] != m.w {
		return nil
	}
	if m.err == nil {
		return nil // cancelled
	}
	if errors.Is(m.err, kube.ErrWatchClosed) {
		// normal server-side timeout: resume immediately
		return a.startWatch(m.pane, m.w.key)
	}
	m.w.failed, m.w.err, m.w.retryAt = true, m.err, time.Now().Add(watchRetry)
	return nil
}

// liveBadge is the title marker: ● live (watch) or ↻ Ns (polling).
func (a *App) liveBadge() string {
	if !a.res.Watchable() {
		return ""
	}
	if a.watching(a.pane) {
		return " · ● live"
	}
	b := " · ↻ " + a.refreshEvery().String()
	if w := a.watches[a.pane]; w != nil && w.failed {
		b += " (watch unavailable)"
	}
	return b
}
