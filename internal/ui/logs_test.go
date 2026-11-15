package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func textApp(t *testing.T, lines []string) *App {
	t.Helper()
	a := newTestApp(t)
	a.mode, a.textKind, a.textLines = modeText, "describe", lines
	a.renderText(true)
	return a
}

func TestSearchJumpsAndHighlights(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		l := "line " + string(rune('a'+i%26))
		if i == 50 || i == 120 || i == 180 {
			l = "ERROR connection refused at step " + string(rune('A'+i%26))
		}
		lines = append(lines, l)
	}
	a := textApp(t, lines)
	press(a, "/")
	a.input.SetValue("connection REFUSED") // case-insensitive
	press(a, "enter")
	if len(a.matches) != 3 || a.matchIdx != 0 || !strings.Contains(a.status, "match 1/3") {
		t.Fatalf("matches=%v idx=%d status=%q", a.matches, a.matchIdx, a.status)
	}
	if !strings.Contains(ansi.Strip(a.vp.View()), "ERROR connection refused") {
		t.Fatal("the first match should be scrolled into view")
	}
	press(a, "n")
	press(a, "n")
	if a.matchIdx != 2 || a.vp.YOffset < 150 {
		t.Fatalf("n should move to the 3rd match: idx=%d offset=%d", a.matchIdx, a.vp.YOffset)
	}
	press(a, "n") // wraps around
	press(a, "N") // back to the last
	if a.matchIdx != 2 {
		t.Fatalf("N: idx=%d", a.matchIdx)
	}
	press(a, "esc") // first esc clears the search, view stays open
	if a.search != "" || a.mode != modeText {
		t.Fatal("esc should clear the search before leaving")
	}
}

func TestSearchWithWrappedLines(t *testing.T) {
	long := strings.Repeat("x", 500)
	lines := []string{long, long, long, "needle here", long}
	a := textApp(t, lines)
	a.setSearch("needle")
	if a.visualStart[3] <= 3 || a.vp.YOffset > a.visualStart[3] {
		t.Fatalf("wrapped lines must be counted: visualStart=%v offset=%d", a.visualStart, a.vp.YOffset)
	}
}

func TestMergedLogsAndWindow(t *testing.T) {
	a := newTestApp(t)
	pods := []corev1.Pod{}
	for _, n := range []string{"api-7d9f-abc12", "api-7d9f-def34"} {
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "shop"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}}})
	}
	a.Update(podsMsg{purpose: "logs", pods: pods})
	if a.ov != ovPicker || !strings.HasPrefix(a.picker.items[0], "★ all 2 pods, merged") {
		t.Fatalf("workload logs should offer a merged stream: %v", a.picker.items)
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(a, cmd, 0)
	deadline := time.Now().Add(2 * time.Second)
	for len(a.textLines) < 2 && time.Now().Before(deadline) {
		run(a, waitLines(a.logCh, a.logGen), 0)
	}
	got := strings.Join(a.textLines, "\n")
	if !strings.Contains(got, "[abc12] fake logs") || !strings.Contains(got, "[def34] fake logs") {
		t.Fatalf("merged lines should carry pod tags:\n%s", got)
	}
	if !strings.Contains(a.textTitle, "2 pods merged") {
		t.Fatalf("title = %q", a.textTitle)
	}
	press(a, "t")
	a.picker.cursor = pickerIndex(t, a, "last 15m")
	press(a, "enter")
	if a.logSpec.since != 15*time.Minute || len(a.logSpec.pods) != 2 || !strings.Contains(a.textTitle, "last 15m") {
		t.Fatalf("time window should restart the merged stream: %+v %q", a.logSpec, a.textTitle)
	}
	press(a, "T")
	if !a.logSpec.timestamps {
		t.Fatal("T should toggle timestamps")
	}
	a.stopLogs()
}
