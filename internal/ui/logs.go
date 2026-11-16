package ui

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// logSpec describes the current log stream so it can be restarted (time window, timestamps).
type logSpec struct {
	ns         string
	pods       []string // more than one = merged stream
	container  string
	previous   bool
	since      time.Duration
	timestamps bool
}

const maxMergedPods = 20

func (l logSpec) window() string {
	if l.since > 0 {
		return "last " + shortDuration(l.since)
	}
	return "last 500 lines"
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return d.String()
}

// podTag is the short label used to prefix merged log lines: the random suffix of the pod name.
func podTag(pod string) string {
	if i := strings.LastIndex(pod, "-"); i >= 0 && i < len(pod)-1 {
		return pod[i+1:]
	}
	return pod
}

// startMergedLogStream follows several pods at once; every line is prefixed with [tag].
func startMergedLogStream(c *kube.Client, spec logSpec) (chan string, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan string, 4096)
	var wg sync.WaitGroup
	for _, pod := range spec.pods {
		wg.Add(1)
		go func(pod string) {
			defer wg.Done()
			tag := "[" + podTag(pod) + "] "
			opts := kube.LogOptions{Container: spec.container, Tail: 200, Follow: !spec.previous, Previous: spec.previous,
				Since: spec.since, Timestamps: spec.timestamps}
			rc, err := c.Logs(ctx, spec.ns, pod, opts)
			if err != nil {
				select {
				case ch <- tag + "[kubesage] error: " + err.Error():
				case <-ctx.Done():
				}
				return
			}
			defer rc.Close()
			sc := bufio.NewScanner(rc)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				select {
				case ch <- tag + sc.Text():
				case <-ctx.Done():
					return
				}
			}
		}(pod)
	}
	go func() { wg.Wait(); close(ch) }()
	return ch, cancel
}

// startLogs (re)starts the stream described by spec in the focused pane.
func (a *App) startLogs(spec logSpec) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode = modeText
	podRes, _ := kube.Lookup("pods")
	a.target = targetRef{podRes, spec.ns, spec.pods[0]}
	a.textKind, a.logSpec = "logs", spec
	who := spec.ns + "/" + spec.pods[0]
	if len(spec.pods) > 1 {
		who = fmt.Sprintf("%s/ %d pods merged", spec.ns, len(spec.pods))
	}
	a.textTitle = fmt.Sprintf("logs %s [%s] · %s", who, spec.container, spec.window())
	if spec.previous {
		a.textTitle += " (previous)"
	}
	if spec.timestamps {
		a.textTitle += " · timestamps"
	}
	a.textLines, a.follow = nil, true
	a.logGen++
	if len(spec.pods) > 1 {
		a.logCh, a.logCancel = startMergedLogStream(a.client, spec)
	} else {
		a.logCh, a.logCancel = startLogStream(a.client, spec.ns, spec.pods[0], kube.LogOptions{Container: spec.container, Tail: 500,
			Follow: !spec.previous, Previous: spec.previous, Since: spec.since, Timestamps: spec.timestamps})
	}
	a.renderText(true)
	return tagPane(a.pane, waitLines(a.logCh, a.logGen))
}

// openMergedLogs follows a container across several pods of a workload.
func (a *App) openMergedLogs(pods []corev1.Pod) tea.Cmd {
	if len(pods) > maxMergedPods {
		pods = pods[:maxMergedPods]
	}
	names := make([]string, len(pods))
	for i := range pods {
		names[i] = pods[i].Name
	}
	start := func(container string) tea.Cmd {
		return a.startLogs(logSpec{ns: pods[0].Namespace, pods: names, container: container})
	}
	var ctrs []string
	for _, c := range pods[0].Spec.Containers {
		ctrs = append(ctrs, c.Name)
	}
	if len(ctrs) == 1 {
		return start(ctrs[0])
	}
	a.openPicker("Container to follow in all pods", ctrs, start)
	return nil
}

// pickLogWindow restarts the current stream with another time window.
func (a *App) pickLogWindow() tea.Cmd {
	opts := []struct {
		label string
		since time.Duration
	}{{"last 500 lines", 0}, {"last 5m", 5 * time.Minute}, {"last 15m", 15 * time.Minute}, {"last 1h", time.Hour},
		{"last 6h", 6 * time.Hour}, {"last 24h", 24 * time.Hour}}
	labels := make([]string, len(opts))
	for i, o := range opts {
		labels[i] = o.label
	}
	a.openPicker("Show logs from", labels, func(s string) tea.Cmd {
		for _, o := range opts {
			if o.label == s {
				spec := a.logSpec
				spec.since = o.since
				return a.startLogs(spec)
			}
		}
		return nil
	})
	return nil
}

// ---------- search ----------

func (a *App) setSearch(q string) {
	a.search = strings.TrimSpace(q)
	a.matchIdx = -1
	a.renderText(false)
	if a.search == "" {
		a.setStatus("search cleared", false)
		return
	}
	if len(a.matches) == 0 {
		a.setStatus("no matches for "+q, true)
		return
	}
	// First match at or after the current position.
	top := a.vp.YOffset
	a.matchIdx = 0
	for i, li := range a.matches {
		if li < len(a.visualStart) && a.visualStart[li] >= top {
			a.matchIdx = i
			break
		}
	}
	a.jumpToMatch()
}

func (a *App) nextMatch(delta int) {
	if len(a.matches) == 0 {
		a.setStatus("no search active (press / to search)", true)
		return
	}
	a.matchIdx = (a.matchIdx + delta + len(a.matches)) % len(a.matches)
	a.jumpToMatch()
}

func (a *App) jumpToMatch() {
	if a.matchIdx < 0 || a.matchIdx >= len(a.matches) {
		return
	}
	li := a.matches[a.matchIdx]
	if li < len(a.visualStart) {
		a.vp.SetYOffset(max(0, a.visualStart[li]-a.vp.Height/3))
	}
	if a.textKind == "logs" {
		a.follow = false // stay on the match instead of jumping to new lines
	}
	a.renderText(false)
	a.setStatus(fmt.Sprintf("match %d/%d for %q · n next · N previous · esc clears", a.matchIdx+1, len(a.matches), a.search), false)
}

// highlightSearch marks every occurrence of the search term in a raw line.
func (a *App) highlightSearch(raw string, current bool) string {
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(a.search))
	hl := lipgloss.NewStyle().Background(a.st.T.Warn).Foreground(a.st.T.Bg).Bold(true)
	if current {
		hl = lipgloss.NewStyle().Background(a.st.T.Accent2).Foreground(a.st.T.Bg).Bold(true)
	}
	var b strings.Builder
	last := 0
	for _, m := range re.FindAllStringIndex(raw, -1) {
		b.WriteString(a.st.Base.Render(raw[last:m[0]]))
		b.WriteString(hl.Render(raw[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(a.st.Base.Render(raw[last:]))
	return b.String()
}

// podTagColors give every pod in a merged stream its own colour.
func (a *App) podTagStyle(tag string) lipgloss.Style {
	t := a.st.T
	palette := []lipgloss.Color{t.Accent, t.Accent2, t.Info, t.OK, t.Warn}
	h := 0
	for _, r := range tag {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return lipgloss.NewStyle().Foreground(palette[h%len(palette)]).Bold(true)
}
