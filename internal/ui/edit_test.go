package ui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// editApp has a ConfigMap and a fake API server that honours dry-run (it does not persist).
func editApp(t *testing.T, readOnly bool) (*App, *dynfake.FakeDynamicClient, *[]bool) {
	t.Helper()
	cm := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "shop", ResourceVersion: "7"}, Data: map[string]string{"LOG_LEVEL": "info"}}
	dyn := dynfake.NewSimpleDynamicClient(scheme.Scheme, cm)
	var dryRuns []bool
	dyn.PrependReactor("update", "configmaps", func(act k8stesting.Action) (bool, runtime.Object, error) {
		u := act.(k8stesting.UpdateActionImpl)
		dry := len(u.UpdateOptions.DryRun) > 0
		dryRuns = append(dryRuns, dry)
		if dry {
			return true, u.Object, nil // validated, not persisted
		}
		return false, nil, nil
	})
	c := kube.NewForTesting(fake.NewSimpleClientset(cm), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	a := New(testConfig(t), c, Options{Resource: "cm", ReadOnly: readOnly})
	a.Update(tea.WindowSizeMsg{Width: 150, Height: 40})
	run(a, a.refresh(), 0)
	return a, dyn, &dryRuns
}

// fakeEditor replaces the editor with a function that rewrites the file.
func fakeEditor(t *testing.T, change func(string) string) {
	t.Helper()
	execEditorFn = func(path string, done func(error) tea.Msg) tea.Cmd {
		b, _ := os.ReadFile(path)
		_ = os.WriteFile(path, []byte(change(string(b))), 0o600)
		return func() tea.Msg { return done(nil) }
	}
	t.Cleanup(func() {
		execEditorFn = func(path string, done func(error) tea.Msg) tea.Cmd { return tea.ExecProcess(editorCommand(path), done) }
	})
}

func liveValue(t *testing.T, dyn *dynfake.FakeDynamicClient) string {
	r, _ := kube.Lookup("cm")
	obj, _ := dyn.Resource(r.GVR).Namespace("shop").Get(t.Context(), "app-config", metav1.GetOptions{})
	v, _, _ := unstructured.NestedString(obj.Object, "data", "LOG_LEVEL")
	return v
}

func TestSafeEditDiffDryRunApply(t *testing.T) {
	fakeEditor(t, func(s string) string { return strings.Replace(s, "LOG_LEVEL: info", "LOG_LEVEL: debug", 1) })
	a, dyn, dry := editApp(t, false)
	press(a, "e")
	if a.textKind != "editdiff" {
		t.Fatalf("after editing, the diff view should open (kind=%q, status=%q)", a.textKind, a.status)
	}
	view := ansi.Strip(strings.Join(a.textLines, "\n"))
	if !strings.Contains(view, "✓ Server-side dry-run passed") || !strings.Contains(view, "- ") || !strings.Contains(view, "LOG_LEVEL: info") ||
		!strings.Contains(view, "LOG_LEVEL: debug") || strings.Contains(view, "managedFields") {
		t.Fatalf("diff view:\n%s", view)
	}
	if len(*dry) != 1 || !(*dry)[0] || liveValue(t, dyn) != "info" {
		t.Fatalf("the dry-run must not change the object: dryRuns=%v value=%s", *dry, liveValue(t, dyn))
	}
	press(a, "A")
	if a.ov != ovConfirm {
		t.Fatal("apply needs confirmation")
	}
	press(a, "y")
	if liveValue(t, dyn) != "debug" || !strings.Contains(a.status, "✓ applied app-config") || a.edit != nil {
		t.Fatalf("apply: value=%s status=%q", liveValue(t, dyn), a.status)
	}
}

func TestSafeEditRejectsRenameAndNoop(t *testing.T) {
	fakeEditor(t, func(s string) string { return strings.Replace(s, "name: app-config", "name: other", 1) })
	a, _, _ := editApp(t, false)
	press(a, "e")
	view := ansi.Strip(strings.Join(a.textLines, "\n"))
	if !strings.Contains(view, "✗ The API server would reject this change") || !strings.Contains(view, "renaming is not supported") {
		t.Fatalf("rename should be refused:\n%s", view)
	}
	press(a, "A")
	if a.ov == ovConfirm || !strings.Contains(a.status, "dry-run must pass") {
		t.Fatal("cannot apply a failing change")
	}
	press(a, "esc")
	if a.edit != nil || a.mode != modeTable {
		t.Fatal("esc discards the edit")
	}

	fakeEditor(t, func(s string) string { return s })
	press(a, "e")
	if !strings.Contains(a.status, "no changes") || a.edit != nil {
		t.Fatalf("unchanged file: %q", a.status)
	}
}

func TestSafeEditBlockedReadOnly(t *testing.T) {
	fakeEditor(t, func(s string) string { return s + "\n" })
	a, _, _ := editApp(t, true)
	press(a, "e")
	if a.edit != nil || !strings.Contains(a.status, "read-only") {
		t.Fatalf("read-only must block editing: %q", a.status)
	}
}
