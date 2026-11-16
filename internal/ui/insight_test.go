package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"kubesage/internal/config"
	"kubesage/internal/kube"
)

func shopFixtures(t *testing.T) []runtime.Object {
	yes := true
	replicas := int32(2)
	now := time.Now()
	lbl := map[string]string{"app": "web"}
	pod := func(name string, crash bool) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: lbl, CreationTimestamp: metav1.NewTime(now.Add(-30 * time.Minute)),
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7d9", Controller: &yes}}},
			Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "web", Image: "shop/web:latest",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi")}},
				EnvFrom:   []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-config"}}}}}},
				Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "web-data"}}}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "web", Ready: true, ImageID: "docker.io/shop/web@sha256:abc123"}}},
		}
		if crash {
			p.Status.Conditions[0].Status = corev1.ConditionFalse
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "web", RestartCount: 7,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1, FinishedAt: metav1.NewTime(now.Add(-2 * time.Minute))}}}}
		}
		return p
	}
	pathType := networkingv1.PathTypePrefix
	https := intstr.FromInt(443)
	cert := selfSignedPEM(t, "shop.example.com", now.Add(10*24*time.Hour))
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: types.UID("d1"),
			Annotations: map[string]string{"argocd.argoproj.io/tracking-id": "shop-web:apps/Deployment:shop/web"}},
			Spec:   appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: lbl}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: pod("x", false).Spec}},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1, Replicas: 2}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-7d9", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-35 * time.Minute)),
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "4"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "d1", Controller: &yes}}},
			Spec:   appsv1.ReplicaSetSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: pod("x", false).Spec}},
			Status: appsv1.ReplicaSetStatus{ReadyReplicas: 1}},
		pod("web-7d9-aaaaa", false), pod("web-7d9-bbbbb", true),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: corev1.ServiceSpec{Selector: lbl, Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Port: 80}}}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "shop.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pathType,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}}}}}}}}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "web-data", Namespace: "shop"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: "shop"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "shop"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-7d9-bbbbb", Namespace: "shop"},
			Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 12, LastTimestamp: metav1.NewTime(now.Add(-time.Minute))},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shop-tls", Namespace: "shop"}, Type: corev1.SecretTypeTLS,
			Data: map[string][]byte{"tls.crt": cert, "tls.key": []byte("PRIVATE-KEY-NEVER-SHOWN")}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "shop", UID: "cj1"}, Spec: batchv1.CronJobSpec{Schedule: "0 * * * *",
			JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "b", Image: "busybox"}}}}}}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup-100", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "backup", UID: "cj1"}}},
			Status: batchv1.JobStatus{Succeeded: 1, StartTime: &metav1.Time{Time: now.Add(-time.Hour)}, CompletionTime: &metav1.Time{Time: now.Add(-time.Hour + 42*time.Second)}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "backup-200", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{{Kind: "CronJob", Name: "backup", UID: "cj1"}}},
			Status: batchv1.JobStatus{Failed: 1, StartTime: &metav1.Time{Time: now.Add(-10 * time.Minute)}}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "web-allow-frontend", Namespace: "shop"}, Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: lbl}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}}}}}},
			Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"}}},
				Ports: []networkingv1.NetworkPolicyPort{{Port: &https}}}}}},
	}
}

func newShopApp(t *testing.T, res string, readOnly bool, dynObjs ...runtime.Object) (*App, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(shopFixtures(t)...)
	metrics := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	argo := schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	certs := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		metrics: "PodMetricsList", argo: "ApplicationList", certs: "CertificateList",
		{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}: "KustomizationList",
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}:        "HelmReleaseList",
		{Version: "v1", Resource: "pods"}:                                                 "PodList"}, dynObjs...)
	for _, p := range []string{"web-7d9-aaaaa", "web-7d9-bbbbb"} {
		u := podMetrics("shop", p, "40m", "300Mi")
		u.Object["containers"] = []any{map[string]any{"name": "web", "usage": map[string]any{"cpu": "40m", "memory": "300Mi"}}}
		if err := dyn.Tracker().Create(metrics, u, "shop"); err != nil {
			t.Fatal(err)
		}
	}
	c := kube.NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	a := New(testConfig(t), c, Options{Resource: res, ReadOnly: readOnly})
	a.provider = fakeAI{}
	a.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	run(a, a.refresh(), 0)
	return a, cs
}

func screen(a *App) string { return ansi.Strip(a.View()) }

func mustContain(t *testing.T, view string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(view, w) {
			t.Fatalf("missing %q in:\n%s", w, view)
		}
	}
}

func TestRelationshipGraphAndTimeline(t *testing.T) {
	a, _ := newShopApp(t, "deploy", false)
	press(a, "w")
	mustContain(t, screen(a), "relationships deployment shop/web", "Ingress web", "Service web", "Deployment web", "ReplicaSet web-7d9",
		"web-7d9-bbbbb", "CrashLoopBackOff", "ConfigMap web-config", "PersistentVolumeClaim web-data", "managed by Argo CD app shop-web", "failing")
	press(a, "t")
	mustContain(t, screen(a), "timeline deployment shop/web", "rollout", "revision 4 created", "restart", "exit 1", "BackOff", "×12")
}

func TestServiceGraphShowsEndpoints(t *testing.T) {
	a, _ := newShopApp(t, "svc", false)
	press(a, "w")
	mustContain(t, screen(a), "Service web", "1/2 endpoints ready", "reached through", "Ingress web")
}

func TestIncidentModeBuildsPrompt(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	cmd := a.openIncident()
	msg := cmd()
	pm, ok := msg.(aiPromptMsg)
	if !ok || pm.err != nil {
		t.Fatalf("expected prompt, got %#v", msg)
	}
	for _, w := range []string{"ROOT CAUSE", "CrashLoopBackOff", "BackOff", "orphan", "Rollouts in the last 2 hours"} {
		if !strings.Contains(pm.prompt, w) {
			t.Fatalf("prompt missing %q:\n%s", w, pm.prompt)
		}
	}
}

func TestRightSizing(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	a.cfg.Cost = config.Cost{CPUCoreHour: 0.03, MemGiBHour: 0.004, Currency: "EUR"}
	run(a, a.runCommand("rightsize"), 0)
	mustContain(t, screen(a), "Right-sizing", "Deployment/web", "over-provisioned", "60m", "EUR/month")
	a.cursor = 0
	press(a, "enter")
	mustContain(t, screen(a), "set resources deployment/web --requests=cpu=60m")
}

func TestCertsImagesStorage(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	run(a, a.runCommand("certs"), 0)
	v := screen(a)
	mustContain(t, v, "TLS certificates", "shop-tls", "shop.example.com", "1 expire within 30 days")
	if strings.Contains(v, "PRIVATE-KEY") {
		t.Fatal("private key leaked")
	}
	run(a, a.runCommand("images"), 0)
	mustContain(t, screen(a), "Images in use", "shop/web", "latest ⚠", "1 use latest or no tag", "sha256:abc123")
	run(a, a.runCommand("storage"), 0)
	mustContain(t, screen(a), "Storage", "web-data", "web-7d9-aaaaa", "orphan", "not mounted")
}

func TestGitOpsApps(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "shop-web", "namespace": "shop"},
		"spec":     map[string]any{"source": map[string]any{"repoURL": "https://git.example.com/shop", "path": "deploy"}},
		"status":   map[string]any{"sync": map[string]any{"status": "OutOfSync", "revision": "abc123"}, "health": map[string]any{"status": "Degraded"}}}}
	a, _ := newShopApp(t, "pods", false, app)
	run(a, a.runCommand("gitops"), 0)
	mustContain(t, screen(a), "GitOps apps", "Argo CD", "shop-web", "OutOfSync", "Degraded", "1 failing")
}

func TestCronJobRunsAndTrigger(t *testing.T) {
	a, cs := newShopApp(t, "cronjobs", false)
	press(a, "h")
	mustContain(t, screen(a), "Runs of cronjob shop/backup", "backup-100", "Succeeded", "backup-200", "Failed", "avg 42s")
	press(a, "esc")
	press(a, "J")
	if a.ov != ovConfirm {
		t.Fatal("trigger must ask first")
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	run(a, cmd, 0)
	jobs, _ := cs.BatchV1().Jobs("shop").List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 3 {
		t.Fatalf("expected a manual job, got %d jobs", len(jobs.Items))
	}
	ro, _ := newShopApp(t, "cronjobs", true)
	press(ro, "J")
	if !strings.Contains(ro.status, "read-only") {
		t.Fatalf("read-only must block trigger, status=%q", ro.status)
	}
}

func TestNetworkPolicyReport(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	press(a, "P")
	mustContain(t, screen(a), "network policies for pod", "web-allow-frontend", "pods [app=frontend] in this namespace", "ip 10.0.0.0/8",
		"no rule allows port 53", "blocked")
}

func TestExportAuditPaletteColumns(t *testing.T) {
	var dynPods []runtime.Object
	for _, o := range shopFixtures(t) {
		if p, ok := o.(*corev1.Pod); ok {
			m, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
			u := &unstructured.Unstructured{Object: m}
			u.SetAPIVersion("v1")
			u.SetKind("Pod")
			dynPods = append(dynPods, u)
		}
	}
	a, _ := newShopApp(t, "pods", false, dynPods...)
	dir := t.TempDir()
	out := filepath.Join(dir, "pods.csv")
	run(a, a.runCommand("export csv "+out), 0)
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), "NAME") || !strings.Contains(string(b), "web-7d9-aaaaa") {
		t.Fatalf("csv export wrong: %v %s", err, b)
	}
	md := renderExport("md", []string{"A", "B"}, [][]string{{"x|y", "z"}})
	if !strings.Contains(md, `x\|y`) {
		t.Fatalf("md escaping: %s", md)
	}
	a.Update(actionMsg{status: "scaled deployment web to 3"})
	run(a, a.runCommand("audit"), 0)
	mustContain(t, screen(a), "audit log · 1 changes", "scaled deployment web to 3")
	press(a, "esc")
	a.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	mustContain(t, screen(a), "Command palette", "Helm releases")
	press(a, "esc")

	a.cfg.Columns = map[string][]config.Column{"pods": {{Name: "image", Path: "spec.containers[0].image"}, {Name: "app", Label: "app"}}}
	run(a, a.refresh(), 0)
	mustContain(t, screen(a), "IMAGE", "shop/web:latest", "APP")
}

func TestFieldPath(t *testing.T) {
	obj := map[string]any{"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "web"}},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}}}}
	cases := map[string]string{"spec.containers[1].name": "b", ".spec.containers[*].name": "a,b", `{.metadata.labels.app\.kubernetes\.io/name}`: "web", "spec.nope": ""}
	for p, want := range cases {
		if got := fieldPath(obj, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
}

func TestPluginRunsInBackground(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	no := false
	a.cfg.Plugins = []config.Plugin{{Name: "echo", Key: "K", Scopes: []string{"pods"}, Command: "echo", Args: []string{"$NAME", "in", "$NAMESPACE"}, Background: true, Confirm: &no}}
	press(a, "K")
	mustContain(t, screen(a), "plugin echo", "web-7d9-aaaaa in shop")
}

func TestActionsMenuAndHintsShowNewFeatures(t *testing.T) {
	a, _ := newShopApp(t, "deploy", false)
	v := screen(a)
	mustContain(t, v, "space actions", "w relations", "t timeline", "I Incident", "^P Palette")
	press(a, " ")
	v = screen(a)
	mustContain(t, v, "Actions · deployment shop/web", "relationships graph", "timeline", "right-sizing")
	// filter to the graph entry and run it
	for _, r := range "relationships" {
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(a, cmd, 0)
	mustContain(t, screen(a), "relationships deployment shop/web", "Ingress web")
}
