package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

func newLauncherApp(t *testing.T) (*App, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(podIn("shop", "staging-api"))
	staging := kube.NewForTesting(cs, "aks-staging", "shop", testContexts)
	a := New(testConfig(t), staging, Options{ChooseContext: true})
	a.cl.add(kube.NewForTesting(fake.NewSimpleClientset(podIn("shop", "prod-api")), "aks-prod", "shop", testContexts))
	a.cl.add(kube.NewForTesting(fake.NewSimpleClientset(podIn("default", "dev-api")), "kind-dev", "default", testContexts))
	a.Update(tea.WindowSizeMsg{Width: 150, Height: 30})
	// Same as Init with ChooseContext, minus the refresh/spinner timers that would block the test.
	a.launchCursor = true
	run(a, a.openLauncher(), 0)
	return a, cs
}

func TestLauncherShownWithoutContacting(t *testing.T) {
	a, cs := newLauncherApp(t)
	if !a.launcher || a.res.Name != clustersRes {
		t.Fatal("chooser should be shown at startup")
	}
	if n := len(cs.Actions()); n != 0 {
		t.Fatalf("no cluster may be contacted before the user chooses, saw %d API calls", n)
	}
	row, _ := a.selected()
	if row.Name != "aks-staging" {
		t.Fatalf("cursor should start on the current context, got %s", row.Name)
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"choose a cluster", "Choose cluster(s) (3)", "[ ] aks-prod  ⚠ prod", "not connected"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "1 pane(s)") {
		t.Error("the chooser must not count as using the current context")
	}
	assertFits(t, a)
}

func TestLauncherPickOne(t *testing.T) {
	a, _ := newLauncherApp(t)
	press(a, "enter") // current context
	if a.launcher || a.client.Context != "aks-staging" || a.res.Name != "pods" || len(a.tabs) != 1 {
		t.Fatalf("enter should open aks-staging pods in this tab: launcher=%v ctx=%s res=%s tabs=%d",
			a.launcher, a.client.Context, a.res.Name, len(a.tabs))
	}
	if len(a.table.Rows) != 1 || a.table.Rows[0].Name != "staging-api" {
		t.Fatalf("pods of the chosen cluster should load: %+v", a.table.Rows)
	}
}

func TestLauncherPickSeveral(t *testing.T) {
	a, _ := newLauncherApp(t)
	// tick kind-dev and aks-prod with space, in list order: aks-prod, aks-staging, kind-dev
	a.cursor = 0
	press(a, " ") // aks-prod, cursor moves down
	press(a, "x") // aks-staging (x also toggles)
	press(a, " ") // kind-dev
	if v := ansi.Strip(a.View()); strings.Count(v, "[x]") != 3 {
		t.Fatalf("three clusters should be ticked:\n%s", v)
	}
	a.cursor = 1
	press(a, " ") // untick aks-staging
	press(a, "enter")
	if len(a.tabs) != 2 {
		t.Fatalf("two ticked clusters → two tabs, got %d", len(a.tabs))
	}
	if a.tab != 0 || a.client.Context != "aks-prod" || a.tabs[1].focus.client.Context != "kind-dev" {
		t.Fatalf("first ticked cluster takes the front tab: tab=%d ctx=%s second=%s", a.tab, a.client.Context, a.tabs[1].focus.client.Context)
	}
	if a.launcher {
		t.Fatal("chooser should close")
	}
}

func TestLauncherFailedConnectStays(t *testing.T) {
	a, _ := newLauncherApp(t)
	run(a, a.openClustersPicked([]string{"no-such-ctx"}), 0)
	if !a.launcher || a.res.Name != clustersRes || !a.statusErr {
		t.Fatalf("a failed connection must keep the chooser open with an error: launcher=%v res=%s status=%q", a.launcher, a.res.Name, a.status)
	}
}

func TestContextFlagSkipsLauncher(t *testing.T) {
	a := newMultiApp(t) // Options without ChooseContext, like --context
	if a.launcher || a.res.Name != "pods" {
		t.Fatal("with --context the chooser must not appear")
	}
}
