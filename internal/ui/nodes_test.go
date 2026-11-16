package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

func nodeFixtures() []any {
	yes := true
	owned := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: &yes}}
	}
	pod := func(ns, name string, owners []metav1.OwnerReference, lbl map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: owners, Labels: lbl},
			Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	}
	one := intstr.FromInt(1)
	return []any{
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.azure.com/agentpool": "system", "topology.kubernetes.io/zone": "westeurope-1"}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("16Gi"), corev1.ResourcePods: resource.MustParse("110")},
				NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.35.7"}}},
		pod("kube-system", "agent-x1", owned("DaemonSet", "agent"), nil),
		pod("shop", "api-1", owned("ReplicaSet", "api-7d9"), map[string]string{"app": "api"}),
		pod("shop", "db-0", owned("StatefulSet", "db"), map[string]string{"app": "db"}),
		pod("shop", "debug", nil, nil),
		&policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "db-pdb", Namespace: "shop"},
			Spec:   policyv1.PodDisruptionBudgetSpec{MinAvailable: &one, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}}},
			Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0}},
	}
}

func newNodeApp(t *testing.T, readOnly bool) (*App, *fake.Clientset) {
	t.Helper()
	var objs []runtime.Object
	for _, o := range nodeFixtures() {
		objs = append(objs, o.(runtime.Object))
	}
	cs := fake.NewSimpleClientset(objs...)
	a := New(testConfig(t), kube.NewForTesting(cs, "dev", "", map[string]string{"dev": "dev"}), Options{Resource: "nodes", ReadOnly: readOnly})
	a.Update(tea.WindowSizeMsg{Width: 170, Height: 50})
	run(a, a.refresh(), 0)
	return a, cs
}

func TestNodeHealthReport(t *testing.T) {
	a, _ := newNodeApp(t, false)
	press(a, "h")
	v := ansi.Strip(strings.Join(a.textLines, "\n"))
	for _, want := range []string{"✓ Ready", "system · westeurope-1", "kubelet v1.35.7", "CPU requests", "2000m of 4000m", "Mem requests", "4.0Gi of 16.0Gi",
		"Pods on this node (4)", "shop/db-0"} {
		if !strings.Contains(v, want) {
			t.Errorf("node report missing %q\n%s", want, v)
		}
	}
}

func TestDrainPreviewAndCordon(t *testing.T) {
	a, cs := newNodeApp(t, false)
	press(a, "X")
	v := ansi.Strip(strings.Join(a.textLines, "\n"))
	for _, want := range []string{"nothing has been changed", "1 will be evicted", "1 skipped", "2 blocked",
		"DaemonSet pod", "PodDisruptionBudget db-pdb allows 0 disruptions", "not managed by a controller"} {
		if !strings.Contains(v, want) {
			t.Errorf("drain preview missing %q\n%s", want, v)
		}
	}
	if n, _ := cs.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{}); n.Spec.Unschedulable {
		t.Fatal("the preview must not change anything")
	}
	press(a, "A")
	press(a, "y") // a plain "y" is not enough: the node name must be typed
	if n, _ := cs.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{}); n.Spec.Unschedulable {
		t.Fatal("drain needs the node name typed")
	}
	a.input.SetValue("node-a")
	press(a, "enter")
	if n, _ := cs.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{}); !n.Spec.Unschedulable {
		t.Fatal("drain should cordon the node")
	}
	if !strings.Contains(a.status, "evicted") {
		t.Fatalf("status = %q", a.status)
	}
	evictions := 0
	for _, act := range cs.Actions() {
		if act.GetSubresource() == "eviction" {
			evictions++
		}
	}
	if evictions != 1 {
		t.Fatalf("only the evictable pod may be evicted, got %d evictions", evictions)
	}
}

func TestTaintAndReadOnly(t *testing.T) {
	a, cs := newNodeApp(t, false)
	run(a, a.runCommand("taint dedicated=gpu:NoSchedule"), 0)
	press(a, "y")
	n, _ := cs.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{})
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "dedicated" || n.Spec.Taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("taint: %+v", n.Spec.Taints)
	}
	run(a, a.runCommand("taint dedicated-"), 0)
	press(a, "y")
	n, _ = cs.CoreV1().Nodes().Get(t.Context(), "node-a", metav1.GetOptions{})
	if len(n.Spec.Taints) != 0 {
		t.Fatalf("taint removal: %+v", n.Spec.Taints)
	}
	if _, _, err := kube.ParseTaint("x=y:Sometimes"); err == nil {
		t.Fatal("invalid effect must be rejected")
	}

	ro, _ := newNodeApp(t, true)
	press(ro, "u")
	if ro.ov == ovConfirm || !strings.Contains(ro.status, "read-only") {
		t.Fatal("read-only blocks cordon")
	}
}
