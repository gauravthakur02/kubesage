package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

func newNavApp(t *testing.T) *App {
	t.Helper()
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	certGVR := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdGVR: "CustomResourceDefinitionList", certGVR: "CertificateList",
		{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}: "PodMetricsList"})
	_ = dyn.Tracker().Create(crdGVR, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "certificates.cert-manager.io"},
		"spec": map[string]any{"group": "cert-manager.io", "scope": "Namespaced",
			"names":    map[string]any{"plural": "certificates", "singular": "certificate", "kind": "Certificate", "shortNames": []any{"cert"}},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true}}},
	}}, "")
	_ = dyn.Tracker().Create(certGVR, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate", "metadata": map[string]any{"name": "web-tls", "namespace": "shop"},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
	}}, "shop")
	cs := fake.NewSimpleClientset(podIn("shop", "api"),
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "devs", Namespace: "shop"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "edit"}})
	c := kube.NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	a := New(testConfig(t), c, Options{})
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	run(a, a.refresh(), 0)
	return a
}

func TestNavigatorListsEverything(t *testing.T) {
	a := newNavApp(t)
	press(a, "0")
	if a.ov != ovPicker {
		t.Fatal("0 should open the resource navigator")
	}
	items := strings.Join(a.picker.items, "\n")
	for _, want := range []string{
		"Workloads", "Deployment", "DaemonSet", "ReplicaSet", "StatefulSet", "CronJob",
		"Config & Storage", "ConfigMap", "Secret", "PersistentVolume (cluster)", "StorageClass",
		"Access control (RBAC)", "ServiceAccount", "Role ", "RoleBinding", "ClusterRole (cluster)", "ClusterRoleBinding",
		"Network", "NetworkPolicy", "Endpoints", "CustomResourceDefinition", "Custom resources", "Certificate  · cert-manager.io",
	} {
		if !strings.Contains(items, want) {
			t.Errorf("navigator missing %q", want)
		}
	}
	// Type to filter, then open RoleBindings.
	press(a, "rolebinding")
	press(a, "enter")
	if a.res.Name != "rolebindings" || len(a.table.Rows) != 1 || a.table.Rows[0].Cells[1] != "ClusterRole/edit" {
		t.Fatalf("expected rolebindings table, got %s %+v", a.res.Name, a.table.Rows)
	}
	if !strings.Contains(ansi.Strip(a.View()), "0 ☰ All") {
		t.Fatal("header should advertise the navigator")
	}
}

func TestCustomResourceCommandAndCRDDrillDown(t *testing.T) {
	a := newNavApp(t)
	run(a, a.runCommand("cert"), 0) // unknown until discovered → discovers, then opens
	if a.res.Name != "certificates.cert-manager.io" || len(a.table.Rows) != 1 || a.table.Rows[0].Name != "web-tls" {
		t.Fatalf(":cert should open Certificates, got %s %+v", a.res.Name, a.table.Rows)
	}
	run(a, a.runCommand("crd"), 0)
	if a.res.Name != "customresourcedefinitions" || len(a.table.Rows) != 1 {
		t.Fatalf("crd table: %s %d", a.res.Name, len(a.table.Rows))
	}
	press(a, "enter")
	if a.res.Name != "certificates.cert-manager.io" {
		t.Fatalf("enter on a CRD should list its objects, got %s", a.res.Name)
	}
	run(a, a.runCommand("nope-nothing"), 0)
	if !strings.Contains(a.status, "unknown command or resource") {
		t.Fatalf("status = %q", a.status)
	}
}
