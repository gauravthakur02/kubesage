package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"kubesage/internal/agent"
	"kubesage/internal/kube"
)

var testContexts = map[string]string{"aks-staging": "staging-cl", "aks-prod": "prod-cl", "kind-dev": "kind"}

func podIn(ns, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

// newMultiApp starts on aks-staging with prod and dev pre-connected in the pool.
func newMultiApp(t *testing.T) *App {
	t.Helper()
	staging := kube.NewForTesting(fake.NewSimpleClientset(podIn("shop", "staging-api")), "aks-staging", "shop", testContexts)
	a := New(testConfig(t), staging, Options{})
	prod := kube.NewForTesting(fake.NewSimpleClientset(podIn("shop", "prod-api"), podIn("shop", "prod-worker")), "aks-prod", "shop", testContexts)
	prod.Dynamic = dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme.Scheme,
		map[schema.GroupVersionResource]string{{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}: "PodMetricsList"},
		podIn("shop", "prod-api"))
	a.cl.add(prod)
	a.cl.add(kube.NewForTesting(fake.NewSimpleClientset(podIn("default", "dev-api")), "kind-dev", "default", testContexts))
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 36})
	run(a, a.refresh(), 0)
	return a
}

func TestSideBySideClusters(t *testing.T) {
	a := newMultiApp(t)
	run(a, a.runCommand("vsplit @aks-prod"), 0)
	ps := a.panes()
	if len(ps) != 2 || ps[0].client.Context != "aks-staging" || ps[1].client.Context != "aks-prod" {
		t.Fatalf("expected staging | prod panes, got %v / %v", ps[0].client.Context, ps[len(ps)-1].client.Context)
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"⎈ aks-staging · 1 Pods (1)", "⚠ ⎈ aks-prod · 2 Pods (2)", "staging-api", "prod-worker", "ctx aks-prod"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in\n%s", want, view)
		}
	}
	assertFits(t, a)
	// Each pane acts on its own cluster: the prod pane needs typed confirmation, staging doesn't.
	a.focus(ps[1])
	press(a, "D")
	if a.ov != ovConfirm || a.inputHint == "" {
		t.Fatal("delete in the prod pane must require typing the name")
	}
	press(a, "esc")
	a.focus(ps[0])
	press(a, "D")
	if a.ov != ovConfirm || a.inputHint != "" {
		t.Fatal("staging pane uses a simple y/N confirm")
	}
	press(a, "n")
}

func TestClusterTabsAndSwitching(t *testing.T) {
	a := newMultiApp(t)
	run(a, a.runCommand("tabnew @kind-dev"), 0)
	if a.client.Context != "kind-dev" || a.client.Namespace != "default" || len(a.tabs) != 2 {
		t.Fatalf("tab on kind-dev expected, got %s/%s", a.client.Context, a.client.Namespace)
	}
	run(a, a.runCommand("tabnew @aks-prod kube-system"), 0)
	if a.client.Context != "aks-prod" || a.client.Namespace != "kube-system" {
		t.Fatalf(":tabnew @ctx ns: got %s/%s", a.client.Context, a.client.Namespace)
	}
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "aks-staging shop·Pods") || !strings.Contains(view, "⚠ aks-prod kube-system·Pods") {
		t.Fatalf("tab strip should name clusters:\n%s", strings.Join(strings.Split(view, "\n")[:2], "\n"))
	}
	// A tab mixing clusters says so.
	run(a, a.runCommand("tab 1"), 0)
	run(a, a.runCommand("vsplit @kind-dev"), 0)
	if l := a.tabLabel(0); l != "⎈×2 clusters" {
		t.Fatalf("mixed tab label = %q", l)
	}
	a.closePane()
	// :ctx switches only the current tab.
	key(a, "|")
	run(a, a.runCommand("ctx kind-dev"), 0)
	for _, p := range a.panes() {
		if p.client.Context != "kind-dev" {
			t.Fatalf("all panes of the tab should switch, got %s", p.client.Context)
		}
	}
	if a.tabs[2].focus.client.Context != "aks-prod" {
		t.Fatal("other tabs must keep their cluster")
	}
}

func TestClustersOverview(t *testing.T) {
	a := newMultiApp(t)
	run(a, a.runCommand("clusters"), 0)
	if a.res.Name != clustersRes || len(a.table.Rows) != 3 {
		t.Fatalf("clusters view should list 3 contexts, got %d", len(a.table.Rows))
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"CONTEXT", "aks-prod  ⚠ prod", "prod-cl", "kind-dev", "1 pane(s)"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q in clusters view:\n%s", want, view)
		}
	}
	// enter on kind-dev opens it in a new tab.
	for i, idx := range a.visible {
		if a.table.Rows[idx].Name == "kind-dev" {
			a.cursor = i
		}
	}
	press(a, "enter")
	if len(a.tabs) != 2 || a.client.Context != "kind-dev" || a.res.Name != "pods" {
		t.Fatalf("enter should open kind-dev in a new tab with pods, got tabs=%d ctx=%s res=%s", len(a.tabs), a.client.Context, a.res.Name)
	}
}

func TestAICommandsRunOnAnalysedCluster(t *testing.T) {
	a := newMultiApp(t)
	run(a, a.applyLayout("ai"), 0) // pods(staging) | AI
	run(a, a.runCommand("vsplit @aks-prod"), 0)
	prodPane := a.pane
	var ranOn []string
	a.execFn = func(c agent.Command) (string, error) { ranOn = append(ranOn, a.aiTarget().Context); return "ok", nil }
	a.provider = &scriptedAI{replies: []string{"```run\nkubectl get pods -n shop\n```", "done"}}
	run(a, a.analyze(prodPane.res, "shop", "prod-api"), 0)
	if a.aiTarget().Context != "aks-prod" || len(ranOn) != 1 || ranOn[0] != "aks-prod" {
		t.Fatalf("AI commands should run on aks-prod, ran on %v (target %s)", ranOn, a.aiTarget().Context)
	}
	if a.pane.mode != modeAI || prodPane.mode != modeTable {
		t.Fatal("analysis should go to the AI pane and leave the prod table")
	}
}

func TestPulsePerPane(t *testing.T) {
	a := newMultiApp(t)
	press(a, "H")
	run(a, a.runCommand("vsplit @aks-prod"), 0)
	press(a, "H")
	ps := a.panes()
	if ps[0].pulse == nil || ps[1].pulse == nil || ps[0].pulse.PodsTotal != 1 || ps[1].pulse.PodsTotal != 2 {
		t.Fatalf("each pulse pane should show its own cluster")
	}
}
