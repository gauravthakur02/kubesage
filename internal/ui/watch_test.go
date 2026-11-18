package ui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"strings"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

func TestWatchTriggersRefresh(t *testing.T) {
	cm := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "shop"}}
	dyn := dynfake.NewSimpleDynamicClient(scheme.Scheme, cm)
	cs := fake.NewSimpleClientset(cm)
	c := kube.NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	a := New(testConfig(t), c, Options{Resource: "cm"})
	a.Update(tea.WindowSizeMsg{Width: 150, Height: 30})
	run(a, a.refresh(), 0)

	cmds := a.ensureWatches()
	if len(cmds) != 1 {
		t.Fatalf("expected one watch, got %d", len(cmds))
	}
	if !strings.Contains(ansi.Strip(a.View()), "● live") {
		t.Fatalf("title should show live badge:\n%s", ansi.Strip(a.View()))
	}
	// a change in the cluster wakes the watch
	time.Sleep(100 * time.Millisecond)
	cm2 := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "shop"}}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cs.CoreV1().ConfigMaps("shop").Create(t.Context(), cm2, metav1.CreateOptions{})
		u, _ := runtimeToUnstructured(cm2)
		dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}).Namespace("shop").Create(t.Context(), u, metav1.CreateOptions{})
	}()
	done := make(chan any, 1)
	go func() { done <- cmds[0]() }()
	select {
	case msg := <-done:
		if _, ok := msg.(watchEventMsg); !ok {
			t.Fatalf("expected watch event, got %T", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not fire")
	}

	// switching to poll mode stops the watch
	a.cfg.LiveUpdates = "poll"
	a.ensureWatches()
	if a.watching(a.pane) {
		t.Fatal("poll mode must stop the watch")
	}
	if !strings.Contains(ansi.Strip(a.View()), "↻") {
		t.Fatal("poll badge missing")
	}
}

func runtimeToUnstructured(o any) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	return &unstructured.Unstructured{Object: m}, err
}
