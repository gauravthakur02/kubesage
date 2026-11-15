package kube

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func crashPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "api", RestartCount: 7,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}}},
	}
}

func TestPodStatusAndIssues(t *testing.T) {
	p := crashPod()
	if got := PodStatus(p); got != "CrashLoopBackOff" {
		t.Fatalf("PodStatus = %q", got)
	}
	issues := PodIssues(p)
	reasons := map[string]bool{}
	for _, i := range issues {
		reasons[i.Reason] = true
	}
	if !reasons["CrashLoopBackOff"] || !reasons["OOMKilled"] {
		t.Fatalf("expected CrashLoopBackOff and OOMKilled, got %+v", issues)
	}
	if reasons["HighRestarts"] {
		t.Fatalf("HighRestarts should be suppressed when a specific waiting reason exists")
	}
}

func TestScanAndPulse(t *testing.T) {
	two := int32(2)
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "shop"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/3 nodes are available: insufficient cpu"}}},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec:       appsv1.DeploymentSpec{Replicas: &two},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 0},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}}}
	c := &Client{Clientset: fake.NewSimpleClientset(crashPod(), pending, dep, node), Namespace: "shop", Context: "test"}

	issues, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text := FormatIssues(issues)
	for _, want := range []string{"CrashLoopBackOff", "Unschedulable", "Unavailable", "NodeNotReady"} {
		if !strings.Contains(text, want) {
			t.Errorf("scan missing %s:\n%s", want, text)
		}
	}
	if issues[0].Severity != HealthErr {
		t.Errorf("errors should sort first, got %+v", issues[0])
	}

	p, err := c.Pulse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.PodsTotal != 2 || p.NodesTotal != 1 || p.NodesReady != 0 || p.DeploysTotal != 1 || p.DeploysReady != 0 {
		t.Errorf("unexpected pulse %+v", p)
	}
}

func TestListAllNamespacesAddsColumn(t *testing.T) {
	c := &Client{Clientset: fake.NewSimpleClientset(crashPod()), Namespace: AllNamespaces}
	r, _ := Lookup("po")
	tbl, err := r.List(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if tbl.Columns[0] != "NAMESPACE" || tbl.Rows[0].Cells[0] != "shop" || tbl.Rows[0].Health != HealthErr {
		t.Fatalf("unexpected table %+v", tbl)
	}
	for _, res := range Resources() {
		if res.Name == "customresourcedefinitions" {
			continue // served through the dynamic client; covered in TestDiscoverCRDs
		}
		if res.Group == "" {
			t.Errorf("%s has no navigator group", res.Name)
		}
		if _, err := res.List(context.Background(), c); err != nil {
			t.Errorf("%s: %v", res.Name, err)
		}
	}
}

func TestCleanRedacts(t *testing.T) {
	secret := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Secret", "metadata": map[string]any{"name": "db", "managedFields": []any{"x"}},
		"data": map[string]any{"password": "aHVudGVyMg=="},
	}}
	m := Clean(secret, true, false)
	if v := m["data"].(map[string]any)["password"]; !strings.HasPrefix(v.(string), "<redacted") {
		t.Fatalf("secret not redacted: %v", v)
	}
	if _, ok := m["metadata"].(map[string]any)["managedFields"]; ok {
		t.Fatal("managedFields not stripped")
	}
	if v := Clean(secret, false, true)["data"].(map[string]any)["password"]; v != "hunter2" {
		t.Fatalf("reveal should decode, got %v", v)
	}

	pod := &unstructured.Unstructured{Object: map[string]any{
		"kind": "Pod", "spec": map[string]any{"containers": []any{map[string]any{"env": []any{
			map[string]any{"name": "DB_PASSWORD", "value": "s3cret"},
			map[string]any{"name": "LOG_LEVEL", "value": "debug"},
		}}}},
	}}
	env := Clean(pod, true, false)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["env"].([]any)
	if env[0].(map[string]any)["value"] != "<redacted>" || env[1].(map[string]any)["value"] != "debug" {
		t.Fatalf("env redaction wrong: %v", env)
	}
}

func TestLookupAliases(t *testing.T) {
	for alias, want := range map[string]string{"deploy": "deployments", "Pod": "pods", "sts": "statefulsets", "hpa": "horizontalpodautoscalers"} {
		if r, ok := Lookup(alias); !ok || r.Name != want {
			t.Errorf("Lookup(%q) = %v, %v", alias, r.Name, ok)
		}
	}
}

func TestNamespaceFallbackWhenClusterListForbidden(t *testing.T) {
	two := int32(2)
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "secret-team"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &two}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "hidden", Namespace: "secret-team"}},
	)
	// Simulate namespace-scoped RBAC: cluster-wide deployment lists and the
	// secret-team namespace are forbidden.
	cs.PrependReactor("list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if ns := a.GetNamespace(); ns == "" || ns == "secret-team" {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", nil)
		}
		return false, nil, nil
	})
	c := &Client{Clientset: cs, Namespace: AllNamespaces}
	r, _ := Lookup("deploy")
	tbl, err := r.List(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.Rows) != 1 || tbl.Rows[0].Name != "api" || tbl.Columns[0] != "NAMESPACE" {
		t.Fatalf("unexpected table %+v", tbl)
	}
	issues, err := c.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(FormatIssues(issues), "shop/deployments/api Unavailable") {
		t.Fatalf("scan should see deployments via fallback:\n%s", FormatIssues(issues))
	}
	// Namespaced views must not fall back: the error is real there.
	c.Namespace = "secret-team"
	if _, err := r.List(context.Background(), c); err == nil {
		t.Fatal("expected forbidden in a single forbidden namespace")
	}
}

func TestRecoveredOOMIsWarning(t *testing.T) {
	p := crashPod()
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
	p.Status.ContainerStatuses[0].Ready = true
	for _, i := range PodIssues(p) {
		if i.Reason == "OOMKilled" && i.Severity != HealthWarn {
			t.Fatalf("recovered OOM should be a warning, got %+v", i)
		}
	}
}

func TestLocalPortBusyDetection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	if LocalPortFree(busy) {
		t.Fatalf("port %d is in use but reported free", busy)
	}
	// A server listening on all interfaces must also count as busy.
	all, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	go func() {
		for {
			c, err := all.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if LocalPortFree(all.Addr().(*net.TCPAddr).Port) {
		t.Fatal("a wildcard listener must make the port busy")
	}
	if s := SuggestLocalPort(busy); s == busy || s == 0 {
		t.Fatalf("suggestion should skip the busy port, got %d", s)
	}
	c := &Client{Rest: &rest.Config{Host: "https://example.invalid"}, Clientset: fake.NewSimpleClientset()}
	if _, err := c.PortForward(PortTarget{Pod: "p", Namespace: "n"}, busy, 80); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("expected busy-port error, got %v", err)
	}
}

func TestDiscoverCRDsAndGenericList(t *testing.T) {
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	certGVR := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdGVR: "CustomResourceDefinitionList", certGVR: "CertificateList"})
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "certificates.cert-manager.io"},
		"spec": map[string]any{"group": "cert-manager.io", "scope": "Namespaced",
			"names": map[string]any{"plural": "certificates", "singular": "certificate", "kind": "Certificate", "shortNames": []any{"cert", "certs"}},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true, "additionalPrinterColumns": []any{
				map[string]any{"name": "Secret", "jsonPath": ".spec.secretName"},
				map[string]any{"name": "Age", "jsonPath": ".metadata.creationTimestamp"},
			}}}},
	}}
	cert := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
		"metadata": map[string]any{"name": "web-tls", "namespace": "shop"},
		"spec":     map[string]any{"secretName": "web-tls-secret"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "Issuing"}}},
	}}
	_ = dyn.Tracker().Create(crdGVR, crd, "")
	_ = dyn.Tracker().Create(certGVR, cert, "shop")

	c := &Client{Clientset: fake.NewSimpleClientset(), Dynamic: dyn, Namespace: "shop"}
	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, ok := FindIn(found, "cert")
	if !ok || r.Name != "certificates.cert-manager.io" || r.Group != "Custom resources" || !r.Namespaced {
		t.Fatalf("CRD resource not discovered by short name: %+v", r)
	}
	tbl, err := r.List(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tbl.Columns, ",") != "NAME,SECRET,AGE" || tbl.Rows[0].Cells[1] != "web-tls-secret" || tbl.Rows[0].Health != HealthErr {
		t.Fatalf("generic table wrong: %v %+v", tbl.Columns, tbl.Rows)
	}
	crds, _ := Lookup("crd")
	if ct, err := crds.List(context.Background(), c); err != nil || len(ct.Rows) != 1 || ct.Rows[0].Cells[1] != "Certificate" {
		t.Fatalf("CRD table: %v %+v", err, ct)
	}
}

func TestRBACTables(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ops-admins"}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: "Group", Name: "ops"}}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "god", Namespace: "shop"}, Rules: []rbacv1.PolicyRule{{Verbs: []string{"*"}, Resources: []string{"*"}}}},
	)
	c := &Client{Clientset: cs, Namespace: "shop"}
	crb, _ := Lookup("crb")
	tbl, _ := crb.List(context.Background(), c)
	if tbl.Rows[0].Cells[1] != "ClusterRole/cluster-admin" || tbl.Rows[0].Cells[2] != "Group:ops" || tbl.Rows[0].Health != HealthWarn {
		t.Fatalf("crb: %+v", tbl.Rows)
	}
	roles, _ := Lookup("roles")
	tbl, _ = roles.List(context.Background(), c)
	if tbl.Rows[0].Health != HealthWarn {
		t.Fatal("wildcard rules should be flagged")
	}
}

func TestPolicyScan(t *testing.T) {
	no, yes, uid := false, true, int64(1000)
	good := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "good", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: &no,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid},
			Containers: []corev1.Container{{Name: "api", Image: "myacr.azurecr.io/shop/api:1.4",
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
				ReadinessProbe: &corev1.Probe{}, LivenessProbe: &corev1.Probe{}}}}}}}
	bad := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true,
			Containers: []corev1.Container{{Name: "app", Image: "busybox", SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}}}}}
	sys := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "kube-system"}, Spec: appsv1.DaemonSetSpec{
		Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{HostNetwork: true, Containers: []corev1.Container{{Name: "a", Image: "x"}}}}}}
	c := &Client{Clientset: fake.NewSimpleClientset(good, bad, sys), Namespace: AllNamespaces}
	fs, err := c.PolicyScan(context.Background(), PolicyOptions{AllowedRegistries: []string{`^[a-z0-9-]+\.azurecr\.io/`}, SkipNamespaces: []string{"kube-system"}})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]Health{}
	for _, f := range fs {
		if f.Workload == "good" {
			t.Errorf("a hardened deployment should have no findings, got %s: %s", f.Check, f.Message)
		}
		if f.Namespace == "kube-system" {
			t.Error("skipped namespaces must not be checked")
		}
		checks[f.Check] = f.Severity
	}
	for check, sev := range map[string]Health{"host namespaces": HealthErr, "privileged": HealthErr, "image registry": HealthErr,
		"image tag": HealthWarn, "runs as root": HealthWarn, "capabilities": HealthWarn, "resource limits": HealthWarn,
		"writable root filesystem": HealthWarn, "service account token": HealthNeutral} {
		if checks[check] != sev {
			t.Errorf("check %q: severity %v, want %v", check, checks[check], sev)
		}
	}
	if fs[0].Severity != HealthErr {
		t.Fatal("errors first")
	}
	if _, err := c.PolicyScan(context.Background(), PolicyOptions{AllowedRegistries: []string{"("}}); err == nil {
		t.Fatal("invalid registry regex must be reported")
	}
}
