package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/gauravthakur02/kubesage/internal/ai"
	"github.com/gauravthakur02/kubesage/internal/config"
)

// scriptAI answers every prompt with a fixed reply.
type scriptAI struct{ reply string }

func (scriptAI) Name() string  { return "script" }
func (scriptAI) Model() string { return "script-1" }
func (s scriptAI) Stream(_ context.Context, _ string, _ []ai.Message, out chan<- string) error {
	out <- s.reply
	return nil
}

func selectRow(t *testing.T, a *App, name string) {
	t.Helper()
	for i, idx := range a.visible {
		if a.table.Rows[idx].Name == name {
			a.cursor = i
			return
		}
	}
	t.Fatalf("row %s not found", name)
}

func TestTourShowsOnceAndSaves(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	a.opts.Tour = true
	a.Init()
	if a.ov != ovTour {
		t.Fatal("tour not shown")
	}
	mustContain(t, screen(a), "Welcome to kubesage", "1/")
	press(a, "enter")
	mustContain(t, screen(a), "Find anything")
	press(a, "esc")
	if a.ov != ovNone || !a.cfg.TourDone {
		t.Fatal("tour not finished")
	}
	b, _ := os.ReadFile(a.cfg.Path())
	if !strings.Contains(string(b), "tour_done: true") {
		t.Fatalf("tour_done not saved:\n%s", b)
	}
}

func TestAIFixShowsDiffBeforeApply(t *testing.T) {
	a, cs := newShopApp(t, "deployments", false)
	d, _ := cs.AppsV1().Deployments("shop").Get(context.Background(), "web", metav1.GetOptions{})
	u, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(d)
	obj := &unstructured.Unstructured{Object: u}
	obj.SetAPIVersion("apps/v1")
	obj.SetKind("Deployment")
	if err := a.client.Dynamic.(*dynfake.FakeDynamicClient).Tracker().Add(obj); err != nil {
		t.Fatal(err)
	}
	a.provider = scriptAI{reply: "```yaml\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        resources:\n          limits: {memory: 256Mi}\n```\nWhy:\n- adds a memory limit\n"}
	selectRow(t, a, "web")
	run(a, a.runCommand("fix"), 0)
	if a.edit == nil || a.edit.patch == "" || a.textKind != "editdiff" {
		t.Fatalf("no AI fix preview: status %q", a.status)
	}
	v := screen(a)
	mustContain(t, v, "AI-proposed change", "adds a memory limit", "256Mi")
	if !strings.Contains(a.edit.edited, "image: shop/web:latest") {
		t.Fatal("patch dropped existing fields")
	}
	a.provider = scriptAI{reply: "```yaml\nspec:\n  template:\n    spec:\n      containers:\n      - name: web\n        env: [{name: X, value: <redacted>}]\n```"}
	a.endEdit()
	a.mode = modeTable
	run(a, a.runCommand("fix"), 0)
	if !strings.Contains(a.status, "redacted") {
		t.Fatalf("redacted patch not refused: %q", a.status)
	}
}

func TestLocalOnlyContextsBlockCloudAI(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	a.cfg.AI.LocalOnlyContexts = "^dev$"
	a.profile = "claude"
	if why := a.aiBlocked("dev"); !strings.Contains(why, "local-only") {
		t.Fatalf("cloud AI allowed on local-only context: %q", why)
	}
	a.profile = "local"
	if why := a.aiBlocked("dev"); why != "" {
		t.Fatalf("local model blocked: %q", why)
	}
	a.cfg.AI.Profiles["claude"] = config.AIProfile{Provider: "anthropic", PriceInPerMTok: 3, PriceOutPerMTok: 15}
	a.profile = "claude"
	a.countTokens(strings.Repeat("x", 40000), strings.Repeat("y", 4000))
	if u := a.aiUsage(); !strings.Contains(u, "11.0k tokens") || !strings.Contains(u, "$0.045") {
		t.Fatalf("usage = %q", u)
	}
}

func TestNaturalLanguageSearch(t *testing.T) {
	a, _ := newShopApp(t, "deployments", true)
	a.provider = scriptAI{reply: "```json\n{\"resource\":\"pods\",\"namespace\":\"\",\"filter\":\"\",\"min_restarts\":3,\"status\":\"\",\"explain\":\"pods restarting a lot\"}\n```"}
	run(a, a.runCommand("nl pods that keep restarting"), 0)
	if a.res.Name != "pods" || a.minRestarts != 3 {
		t.Fatalf("search = %s / %d · %q", a.res.Name, a.minRestarts, a.status)
	}
	for _, i := range a.visible {
		if a.rowRestarts(a.table.Rows[i]) < 3 {
			t.Fatalf("row %s shown with fewer restarts", a.table.Rows[i].Name)
		}
	}
	if len(a.visible) == 0 {
		t.Fatal("restarting pod not shown")
	}
}

func TestApprovalRuleInterceptsChanges(t *testing.T) {
	a, cs := newShopApp(t, "deployments", false)
	a.cfg.Approvals.Contexts = "^dev$"
	selectRow(t, a, "web")
	press(a, "R") // rollout restart
	if a.ov != ovConfirm || !strings.Contains(a.confirm, "second person") {
		t.Fatalf("no approval request: %q", a.confirm)
	}
	press(a, "y")
	cms, _ := cs.CoreV1().ConfigMaps("kubesage-approvals").List(context.Background(), metav1.ListOptions{})
	if len(cms.Items) != 1 || cms.Items[0].Data["status"] != "pending" || len(a.awaitingFn) != 1 {
		t.Fatalf("approval not stored: %d configmaps, %d waiting, status %q", len(cms.Items), len(a.awaitingFn), a.status)
	}
}

func TestWebModeRefusesTerminalPrograms(t *testing.T) {
	WebMode = true
	defer func() { WebMode = false }()
	msg := execEditorFn("/tmp/x", func(err error) tea.Msg { return editDoneMsg{err: err} })()
	if m, ok := msg.(editDoneMsg); !ok || m.err != errWeb {
		t.Fatalf("editor ran in web mode: %#v", msg)
	}
	if m, ok := execShellFn(nil, func(err error) tea.Msg { return execDoneMsg{err: err} })().(execDoneMsg); !ok || m.err != errWeb {
		t.Fatal("shell ran in web mode")
	}
}

func TestDevLoopRefusesProdAndApplies(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: dev-settings}\ndata: {a: b}\n"), 0o644)
	a.cfg.DangerContexts = "dev"
	a.dangerRe = mustRe("dev")
	a.devCommand(dir)
	if a.dev != nil {
		t.Fatal("dev loop started on a production-like context")
	}
	a.dangerRe = nil
	a.devCommand(dir)
	if a.dev == nil || a.dev.ns != "shop" {
		t.Fatalf("dev loop not started: %q", a.status)
	}
	a.devCommand("stop")
	if a.dev != nil {
		t.Fatal("dev loop not stopped")
	}
}

func TestCloneAndCatalogScreens(t *testing.T) {
	a, cs := newShopApp(t, "pods", true)
	cs.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-1",
		Labels: map[string]string{"kubesage.io/clone-of": "shop"}}}, metav1.CreateOptions{})
	run(a, a.runCommand("clone"), 0)
	mustContain(t, screen(a), "Namespace clones", "shop-pr-1")
	idx := filepath.Join(t.TempDir(), "index.yaml")
	os.WriteFile(idx, []byte("plugins:\n  - name: hello\n    description: says hi\n    plugin: {name: hello, key: ctrl+g, scopes: [pods], command: echo, args: [hi]}\n"), 0o644)
	a.cfg.PluginIndex = idx
	run(a, a.runCommand("plugins"), 0)
	mustContain(t, screen(a), "Plugin catalogue", "hello", "says hi")
	selectRow(t, a, "hello")
	run(a, a.extraRowActionCmd("enter"), 0)
	if a.ov != ovConfirm || !strings.Contains(a.confirm, "echo hi") {
		t.Fatalf("install did not show the command: %q", a.confirm)
	}
	press(a, "y")
	if len(a.cfg.Plugins) != 1 || a.cfg.Plugins[0].Key != "ctrl+g" {
		t.Fatalf("plugin not installed: %+v", a.cfg.Plugins)
	}
}

func (a *App) extraRowActionCmd(key string) tea.Cmd {
	row, _ := a.selected()
	cmd, _ := a.extraRowAction(key, row)
	return cmd
}

func TestOperatorScreensWhenMissing(t *testing.T) {
	a, _ := newShopApp(t, "pods", true)
	run(a, a.runCommand("argo-rollouts"), 0)
	mustContain(t, screen(a), "Argo Rollouts")
	run(a, a.runCommand("karpenter"), 0)
	mustContain(t, screen(a), "Karpenter")
}
