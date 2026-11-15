package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"

	"kubesage/internal/kube"
)

func TestPolicyViewAndAIFix(t *testing.T) {
	yes := true
	bad := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "nginx", Image: "nginx:latest",
			SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}}}}}
	c := kube.NewForTesting(fake.NewSimpleClientset(bad), "dev", "shop", map[string]string{"dev": "dev"})
	bad.TypeMeta = metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"}
	c.Dynamic = dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme.Scheme,
		map[schema.GroupVersionResource]string{{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}: "PodMetricsList"}, bad)
	a := New(testConfig(t), c, Options{})
	a.Update(tea.WindowSizeMsg{Width: 170, Height: 40})
	run(a, a.refresh(), 0)
	press(a, "V")
	if a.res.Name != policyRes || len(a.table.Rows) == 0 {
		t.Fatalf("V should open the policy view: %s %d", a.res.Name, len(a.table.Rows))
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"Policy & security · shop", "errors", "✗ error", "privileged", "deployment/web", "nginx"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
	press(a, "/")
	press(a, "image tag")
	press(a, "enter")
	if len(a.visible) != 1 {
		t.Fatalf("filter should narrow to the image tag finding, got %d", len(a.visible))
	}
	sc := &scriptedAI{replies: []string{"Patch: drop privileged"}}
	a.provider = sc
	press(a, "a")
	if a.mode != modeAI || len(sc.seen) == 0 {
		t.Fatal("a should ask the AI for a fix")
	}
	prompt := sc.seen[0][len(sc.seen[0])-1].Content
	if !strings.Contains(prompt, "privileged") || !strings.Contains(prompt, "kind: Deployment") || !strings.Contains(prompt, "Do not apply anything") {
		t.Fatalf("AI prompt should carry findings and manifest:\n%s", prompt)
	}
	press(a, "esc")
	press(a, "esc") // clear filter
	press(a, "esc") // back to where we came from
	if a.res.Name != "pods" {
		t.Fatalf("esc should return from the policy view, got %s", a.res.Name)
	}
}
