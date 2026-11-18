package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// KUBEWEAVE_SNAPSHOT_DIR=/tmp/x go test ./internal/ui -run Snapshot writes ANSI screens for visual review.
func TestSnapshot(t *testing.T) {
	dir := os.Getenv("KUBEWEAVE_SNAPSHOT_DIR")
	if dir == "" {
		t.Skip("set KUBEWEAVE_SNAPSHOT_DIR")
	}
	lipgloss.SetColorProfile(termenv.TrueColor)
	ago := func(d time.Duration) metav1.Time { return metav1.NewTime(time.Now().Add(-d)) }
	pod := func(name, ns string, ready bool, waiting string, restarts int32) *corev1.Pod {
		cs := corev1.ContainerStatus{Name: "app", Ready: ready, RestartCount: restarts, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: ago(time.Hour)}}}
		if waiting != "" {
			cs.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waiting}}
		}
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, CreationTimestamp: ago(26 * time.Hour)},
			Spec:   corev1.PodSpec{NodeName: "aks-system-12345-vmss000001", Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.17", ContainerStatuses: []corev1.ContainerStatus{cs}}}
	}
	three, one := int32(3), int32(1)
	objs := []runtime.Object{
		pod("checkout-api-7d9f8b6c5-2xkqp", "shop", true, "", 0),
		pod("checkout-api-7d9f8b6c5-9hjtw", "shop", true, "", 1),
		pod("payments-worker-5c7d9-lmn4z", "shop", false, "CrashLoopBackOff", 14),
		pod("catalog-6f8d7c9b-x2v8r", "shop", false, "ImagePullBackOff", 0),
		pod("redis-0", "shop", true, "", 0),
		pod("frontend-5b6c7d8e9-qwert", "shop", false, "", 0),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "payments-worker", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &one}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &three},
			Status: appsv1.DeploymentStatus{AvailableReplicas: 2, ReadyReplicas: 2}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "aks-system-12345-vmss000001"}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}},
	}
	cs := fake.NewSimpleClientset()
	for _, o := range objs {
		_ = cs.Tracker().Add(o)
	}
	cfg := testConfig(t)
	a := New(cfg, &kube.Client{Clientset: cs, Namespace: "shop", Context: "aks-staging-weu"}, Options{})
	a.provider = fakeAI{}
	a.version = "v1.31.2"
	a.Update(tea.WindowSizeMsg{Width: 130, Height: 30})
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	a.cursor = 2
	write := func(name string) { _ = os.WriteFile(filepath.Join(dir, name), []byte(a.View()), 0o644) }
	write("table.ans")
	press(a, "H")
	write("pulse.ans")
	press(a, "A")
	run(a, a.sendToAI("Analyze pod shop/payments-worker-5c7d9-lmn4z", "x"), 0)
	write("ai.ans")
	press(a, "esc")
	a.mode = modeTable
	press(a, "?")
	write("help.ans")
	press(a, "esc")
	run(a, a.applyLayout("triage"), 0)
	a.Update(tea.WindowSizeMsg{Width: 150, Height: 34})
	run(a, a.runCommand("ns shop"), 0)
	write("panes.ans")
}
