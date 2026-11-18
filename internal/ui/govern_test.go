package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/gauravthakur02/kubeweave/internal/config"
)

func allowPodsRead(cs interface {
	PrependReactor(verb, resource string, fn k8stesting.ReactionFunc)
}) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(act k8stesting.Action) (bool, runtime.Object, error) {
		r := act.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		ra := r.Spec.ResourceAttributes
		r.Status.Allowed = ra.Resource == "pods" && (ra.Verb == "get" || ra.Verb == "list" || ra.Verb == "watch") && ra.Subresource == ""
		return true, r, nil
	})
}

func TestAccessMatrixWhoCanAndWhy(t *testing.T) {
	a, cs := newShopApp(t, "pods", false)
	allowPodsRead(cs)
	cs.RbacV1().Roles("shop").Create(t.Context(), &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "secret-reader", Namespace: "shop"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}}}, metav1.CreateOptions{})
	cs.RbacV1().RoleBindings("shop").Create(t.Context(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ops-secrets", Namespace: "shop"},
		RoleRef:  rbacv1.RoleRef{Kind: "Role", Name: "secret-reader"},
		Subjects: []rbacv1.Subject{{Kind: "Group", Name: "ops-team"}, {Kind: "ServiceAccount", Name: "backup", Namespace: "shop"}}}, metav1.CreateOptions{})

	run(a, a.runCommand("rbac"), 0)
	v := screen(a)
	mustContain(t, v, "your access in shop", "pods/exec", "secrets", "✓", "✗", "3 of")
	press(a, "esc")
	run(a, a.runCommand("whocan list secrets"), 0)
	mustContain(t, screen(a), "you may not list secrets in shop", "Group/ops-team", "ServiceAccount/shop/backup", "RoleBinding shop/ops-secrets")
	press(a, "esc")
	a.setStatus(`secrets is forbidden: User "me" cannot list resource "secrets" in API group "" in the namespace "shop"`, true)
	run(a, a.runCommand("why"), 0)
	mustContain(t, screen(a), "You may not list secrets in shop", "Group/ops-team")
}

func TestServiceAccountPermissions(t *testing.T) {
	a, cs := newShopApp(t, "sa", false)
	cs.CoreV1().ServiceAccounts("shop").Create(t.Context(), &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "shop"}}, metav1.CreateOptions{})
	cs.RbacV1().ClusterRoles().Create(t.Context(), &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "view"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}}}}, metav1.CreateOptions{})
	cs.RbacV1().RoleBindings("shop").Create(t.Context(), &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "backup-view", Namespace: "shop"},
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "view"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "backup", Namespace: "shop"}}}, metav1.CreateOptions{})
	run(a, a.refresh(), 0)
	for i, idx := range a.visible {
		if a.table.Rows[idx].Name == "backup" {
			a.cursor = i
		}
	}
	press(a, "p")
	mustContain(t, screen(a), "permissions of serviceaccount shop/backup", "RoleBinding backup-view → ClusterRole/view", "deployments", "[apps]")
}

func TestUpgradeScreen(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	run(a, a.runCommand("upgrade 1.36"), 0)
	mustContain(t, screen(a), "upgrade readiness", "→ 1.36", "no use of APIs removed by 1.36")
}

func TestWatchListAlerts(t *testing.T) {
	a, cs := newShopApp(t, "deploy", false)
	run(a, a.runCommand("pin"), 0)
	if len(a.cfg.Watchlist) != 1 || a.cfg.Watchlist[0].Name != "web" {
		t.Fatalf("pin: %+v", a.cfg.Watchlist)
	}
	run(a, a.runMonitor(), 0) // primes: existing problems do not ring
	if a.unseenAlerts() == 0 {
		t.Fatal("the crashing pod should be an active alert")
	}
	if strings.Contains(a.status, "🔔") {
		t.Fatal("existing problems must not ring at startup")
	}
	no := false
	a.cfg.Alerts = config.Alerts{Bell: &no, Desktop: &no}
	bad := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-7d9-ccccc", Namespace: "shop"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "shop/web:latest"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: "web",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}}}}}}
	cs.CoreV1().Pods("shop").Create(t.Context(), bad, metav1.CreateOptions{})
	run(a, a.runMonitor(), 0)
	if !strings.Contains(a.status, "🔔") || !strings.Contains(a.status, "ImagePullBackOff") {
		t.Fatalf("new problem should alert, status=%q", a.status)
	}
	mustContain(t, screen(a), "🔔")
	run(a, a.runCommand("alerts"), 0)
	mustContain(t, screen(a), "Alerts", "ImagePullBackOff", "CrashLoopBackOff", "watching 1")
	if a.unseenAlerts() != 0 {
		t.Error("opening alerts marks them seen")
	}
	run(a, a.runCommand("unpin"), 0)
}

func TestSearchCapacityPoliciesBackups(t *testing.T) {
	report := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "wgpolicyk8s.io/v1alpha2", "kind": "PolicyReport",
		"metadata": map[string]any{"name": "r1", "namespace": "shop"},
		"results": []any{map[string]any{"policy": "disallow-latest-tag", "rule": "require-image-tag", "result": "fail", "source": "kyverno",
			"message": "image tag latest is not allowed", "resources": []any{map[string]any{"kind": "Pod", "name": "web-7d9-aaaaa"}}}}}}
	backup := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "daily-1", "namespace": "velero", "labels": map[string]any{"velero.io/schedule-name": "daily"}},
		"status":   map[string]any{"phase": "PartiallyFailed", "errors": int64(2), "startTimestamp": "2026-11-15T01:00:00Z", "completionTimestamp": "2026-11-15T01:04:00Z"}}}
	a, cs := newShopApp(t, "pods", false, report, backup)
	run(a, a.runCommand("find shop/web"), 0)
	mustContain(t, screen(a), "Search shop/web", "Pod", "web-7d9-aaaaa", "shop/web:latest")
	run(a, a.runCommand("find app=web"), 0)
	mustContain(t, screen(a), "label app=web", "web-7d9-bbbbb")

	cs.CoreV1().ResourceQuotas("shop").Create(t.Context(), &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "shop"},
		Status: corev1.ResourceQuotaStatus{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("4")},
			Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("3800m")}}}, metav1.CreateOptions{})
	cs.CoreV1().Nodes().Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"kubernetes.azure.com/agentpool": "app"}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("32Gi")}}}, metav1.CreateOptions{})
	run(a, a.runCommand("quota"), 0)
	mustContain(t, screen(a), "Resource quotas", "requests.cpu", "95%", "Node pools", "app")

	run(a, a.runCommand("policyreports"), 0)
	mustContain(t, screen(a), "Policy reports", "disallow-latest-tag", "fail", "Pod/web-7d9-aaaaa")
	run(a, a.runCommand("backups"), 0)
	mustContain(t, screen(a), "Velero backups", "daily-1", "PartiallyFailed", "4m0s")
}

func TestIncidentReportExport(t *testing.T) {
	a, _ := newShopApp(t, "deploy", false)
	out := filepath.Join(t.TempDir(), "r.md")
	run(a, a.runCommand("report "+out), 0)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"# Incident report: deployment shop/web", "## Relationships", "Ingress web", "## Timeline", "BackOff", "## Open issues"} {
		if !strings.Contains(string(b), w) {
			t.Fatalf("report missing %q:\n%s", w, b)
		}
	}
}

func TestNetCheckAsksAndIsBlockedReadOnly(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	press(a, "N")
	if a.ov != ovInput {
		t.Fatal("N should ask for a target")
	}
	ro, _ := newShopApp(t, "pods", true)
	press(ro, "N")
	if !strings.Contains(ro.status, "read-only") {
		t.Fatalf("netcheck must be blocked read-only: %q", ro.status)
	}
}
