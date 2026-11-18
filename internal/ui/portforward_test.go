package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

func TestForwardIndicatorsAndBrowser(t *testing.T) {
	var opened []string
	openBrowserFn = func(u string) error { opened = append(opened, u); return nil }
	defer func() { openBrowserFn = openBrowser }()
	defer kube.StopAllForwards()

	a := newTestApp(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}}
	if _, err := a.client.Clientset.CoreV1().Services("shop").Create(t.Context(), svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Simulate a forward that just started for svc/web, backed by pod api-1.
	fw := &kube.Forward{Context: a.client.Context, Namespace: "shop", Pod: "api-1", ObjectDesc: "svc/web",
		Resource: "services", Object: "web", Local: 8080, Remote: 80, Started: time.Now()}
	kube.RegisterForward(fw)
	defer kube.RemoveForward(fw.ID)
	a.Update(pfStartedMsg{fw: fw})
	if len(opened) != 1 || opened[0] != "http://localhost:8080" {
		t.Fatalf("browser not opened: %v", opened)
	}
	if !strings.Contains(a.status, "opened in browser") {
		t.Fatalf("status = %q", a.status)
	}

	// Services table: the forwarded service is tagged.
	run(a, a.runCommand("svc"), 0)
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "web  ⇄ :8080") || !strings.Contains(view, "⇄ svc/web :8080") {
		t.Fatalf("service indicator missing:\n%s", view)
	}

	// Pods table: the backing pod shows it receives traffic through the service forward.
	run(a, a.runCommand("pods"), 0)
	a.cursor = 1 // move selection off the row to see the coloured mark
	view = ansi.Strip(a.View())
	if !strings.Contains(view, "api-1  ⇄ via svc/web :8080") || !strings.Contains(view, "↔") {
		t.Fatalf("pod indicator missing:\n%s", view)
	}

	// F on an already-forwarded service offers open/stop/new instead of starting a duplicate.
	run(a, a.runCommand("svc"), 0)
	a.cursor = 0
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F")})
	run(a, cmd, 0)
	if a.ov != ovPicker || !strings.HasPrefix(a.picker.items[0], "Open http://localhost:8080") {
		t.Fatalf("expected forward menu, got %+v", a.picker)
	}
	press(a, "enter")
	if len(opened) != 2 {
		t.Fatal("open action should open the browser again")
	}
	// Stop it through the menu; the indicator disappears.
	_, cmd = a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("F")})
	run(a, cmd, 0)
	a.picker.cursor = pickerIndex(t, a, "Stop #")
	press(a, "enter")
	if fw.Running() {
		t.Fatal("stop action should stop the forward")
	}
	if view := ansi.Strip(a.View()); strings.Contains(view, "⇄ :8080") {
		t.Fatalf("indicator should disappear after stop:\n%s", view)
	}
}

func TestOpenBrowserDisabled(t *testing.T) {
	called := false
	openBrowserFn = func(string) error { called = true; return nil }
	defer func() { openBrowserFn = openBrowser }()
	a := newTestApp(t)
	a.cfg.OpenBrowser = false
	fw := &kube.Forward{Context: "x", Local: 9000, Remote: 443, ObjectDesc: "pod/p"}
	a.Update(pfStartedMsg{fw: fw})
	if called || !strings.Contains(a.status, "https://localhost:9000") {
		t.Fatalf("called=%v status=%q", called, a.status)
	}
}

func pickerIndex(t *testing.T, a *App, prefix string) int {
	t.Helper()
	for i, it := range a.picker.items {
		if strings.HasPrefix(it, prefix) {
			return i
		}
	}
	t.Fatalf("no picker item starting with %q in %v", prefix, a.picker.items)
	return -1
}

// The user is in another namespace, sees "⇄ ... in shop" in the header, presses f and jumps to it.
func TestFindForgottenForward(t *testing.T) {
	openBrowserFn = func(string) error { return nil }
	defer func() { openBrowserFn = openBrowser }()
	defer kube.StopAllForwards()
	a := newTestApp(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "grafana", Namespace: "shop"}}
	_, _ = a.client.Clientset.CoreV1().Services("shop").Create(t.Context(), svc, metav1.CreateOptions{})
	fw := &kube.Forward{Context: a.client.Context, Namespace: "shop", Pod: "api-1", ObjectDesc: "svc/grafana",
		Resource: "services", Object: "grafana", Local: 8080, Remote: 3000, Started: time.Now().Add(-12 * time.Minute)}
	kube.RegisterForward(fw)

	a.client.Namespace = "monitoring"
	run(a, a.switchResource(a.res), 0)
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "⇄ svc/grafana :8080 in shop") {
		t.Fatalf("header should name the forward and where it is:\n%s", strings.SplitN(view, "\n", 2)[0])
	}

	press(a, "f")
	if a.ov != ovPicker || !strings.Contains(a.picker.items[0], "svc/grafana → http://localhost:8080  (ns shop)  · up 12m") {
		t.Fatalf("forwards list: %v", a.picker.items)
	}
	press(a, "enter")
	if got := a.picker.items; !strings.HasPrefix(got[0], "Open ") || !strings.HasPrefix(got[1], "Go to svc/grafana") || !strings.HasPrefix(got[2], "Stop #") {
		t.Fatalf("menu order: %v", got)
	}
	a.picker.cursor = 1
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(a, cmd, 0)
	row, ok := a.selected()
	if a.res.Name != "services" || a.client.Namespace != "shop" || !ok || row.Name != "grafana" {
		t.Fatalf("go to failed: res=%s ns=%s row=%+v", a.res.Name, a.client.Namespace, row)
	}
	if !strings.Contains(a.status, "forwarded object") {
		t.Fatalf("status = %q", a.status)
	}

	// Pulse lists forwards too.
	press(a, "H")
	if v := ansi.Strip(a.View()); !strings.Contains(v, "Forwards") || !strings.Contains(v, "svc/grafana → http://localhost:8080") {
		t.Fatalf("pulse should list forwards:\n%s", v)
	}
}

func TestHeaderManyForwards(t *testing.T) {
	defer kube.StopAllForwards()
	a := newTestApp(t)
	kube.RegisterForward(&kube.Forward{Context: a.client.Context, Namespace: "shop", ObjectDesc: "pod/a", Resource: "pods", Object: "a", Local: 1})
	kube.RegisterForward(&kube.Forward{Context: "other", Namespace: "x", ObjectDesc: "pod/b", Resource: "pods", Object: "b", Local: 2})
	if got := a.forwardPill(); got != "⇄ 2 forwards · 1 elsewhere  f" {
		t.Fatalf("pill = %q", got)
	}
}
