package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"kubesage/internal/kube"
)

func rolloutFixtures() (*appsv1.Deployment, *appsv1.ReplicaSet, *appsv1.ReplicaSet) {
	three := int32(3)
	tmpl := func(image, hash string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api", "pod-template-hash": hash}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: image}}}}
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop", UID: types.UID("dep-uid"),
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}},
		Spec: appsv1.DeploymentSpec{Replicas: &three, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: "myacr.azurecr.io/shop/api:1.5"}}}}},
		Status: appsv1.DeploymentStatus{Replicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3, ReadyReplicas: 3}}
	owner := []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "dep-uid"}}
	zero := int32(0)
	rs1 := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-aaa", Namespace: "shop", OwnerReferences: owner, Labels: map[string]string{"app": "api"},
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "1", "kubernetes.io/change-cause": "initial release"}},
		Spec: appsv1.ReplicaSetSpec{Replicas: &zero, Template: tmpl("myacr.azurecr.io/shop/api:1.4", "aaa")}}
	rs2 := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-bbb", Namespace: "shop", OwnerReferences: owner, Labels: map[string]string{"app": "api"},
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}},
		Spec: appsv1.ReplicaSetSpec{Replicas: &three, Template: tmpl("myacr.azurecr.io/shop/api:1.5", "bbb")}, Status: appsv1.ReplicaSetStatus{ReadyReplicas: 3}}
	return d, rs1, rs2
}

func TestRolloutHistoryDiffAndRollback(t *testing.T) {
	d, rs1, rs2 := rolloutFixtures()
	cs := fake.NewSimpleClientset(d, rs1, rs2)
	a := New(testConfig(t), kube.NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"}), Options{Resource: "deploy"})
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	run(a, a.refresh(), 0)
	press(a, "o")
	if _, _, ok := isRollout(a.res); !ok || len(a.table.Rows) != 2 {
		t.Fatalf("o should open the rollout history: %s %d", a.res.Name, len(a.table.Rows))
	}
	view := ansi.Strip(a.View())
	for _, want := range []string{"Rollout history deployment shop/api", "✓ rolled out: 3/3 available", "● current", "shop/api:1.5", "initial release"} {
		if !strings.Contains(view, want) {
			t.Errorf("missing %q:\n%s", want, view)
		}
	}
	if a.table.Rows[0].Name != "2" {
		t.Fatal("newest revision first")
	}
	a.cursor = 1 // revision 1
	press(a, "enter")
	diff := ansi.Strip(strings.Join(a.textLines, "\n"))
	if !strings.Contains(diff, "- ") || !strings.Contains(diff, "api:1.4") || !strings.Contains(diff, "+ ") || !strings.Contains(diff, "api:1.5") ||
		strings.Contains(diff, "pod-template-hash") {
		t.Fatalf("diff should show the image change without the hash label:\n%s", diff)
	}
	press(a, "esc") // back to history
	a.cursor = 1
	press(a, "U")
	if a.ov != ovConfirm {
		t.Fatal("rollback needs confirmation")
	}
	press(a, "y")
	got, _ := cs.AppsV1().Deployments("shop").Get(t.Context(), "api", metav1.GetOptions{})
	if img := got.Spec.Template.Spec.Containers[0].Image; img != "myacr.azurecr.io/shop/api:1.4" {
		t.Fatalf("rollback should restore revision 1's template, image = %s", img)
	}
	if _, ok := got.Spec.Template.Labels["pod-template-hash"]; ok {
		t.Fatal("the ReplicaSet hash label must not be copied into the deployment")
	}
	a.cursor = 0
	press(a, "U")
	if a.ov == ovConfirm || !strings.Contains(a.status, "already the current") {
		t.Fatalf("current revision cannot be rolled back to: %q", a.status)
	}
	press(a, "esc")
	if a.res.Name != "deployments" {
		t.Fatalf("esc should return to deployments, got %s", a.res.Name)
	}
}

func TestRollbackBlockedReadOnly(t *testing.T) {
	d, rs1, rs2 := rolloutFixtures()
	a := New(testConfig(t), kube.NewForTesting(fake.NewSimpleClientset(d, rs1, rs2), "dev", "shop", map[string]string{"dev": "dev"}),
		Options{Resource: "deploy", ReadOnly: true})
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	run(a, a.refresh(), 0)
	press(a, "o")
	a.cursor = 1
	press(a, "U")
	if a.ov == ovConfirm || !strings.Contains(a.status, "read-only") {
		t.Fatalf("read-only must block rollback: %q", a.status)
	}
}

func TestDiffLines(t *testing.T) {
	a := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	b := []string{"a", "b", "c", "D", "e", "f", "g", "h", "i", "j", "k"}
	d := diffLines(a, b, 1)
	add, del := diffStats(d)
	if add != 2 || del != 1 || !strings.Contains(strings.Join(d, "|"), "-d|+D") || d[0] != " c" {
		t.Fatalf("diff = %q", d)
	}
}
