package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStaleConfigs(t *testing.T) {
	now := time.Now()
	started := metav1.NewTime(now.Add(-time.Hour))
	changed := metav1.NewTime(now.Add(-5 * time.Minute))
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "app-config", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour)),
		ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl", Time: &changed}}}}
	fresh := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "old-config", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}}
	pod := func(name string, spec corev1.PodSpec) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"pod-template-hash": "abc"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}}},
			Spec: spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started}}
	}
	envSpec := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-config"}}}},
		Env: []corev1.EnvVar{{Name: "X", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "old-config"}, Key: "x"}}}}}}}
	c := NewForTesting(fake.NewSimpleClientset(cm, fresh, pod("web-abc-1", envSpec), pod("web-abc-2", envSpec)), "dev", "shop", map[string]string{"dev": "dev"})
	got, err := c.StaleConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Workload != "Deployment/web" || got[0].Object != "ConfigMap/app-config" || got[0].Pods != 2 || !got[0].NeedsRestart {
		t.Fatalf("stale: %+v", got)
	}
}

func TestStartupBreakdown(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) metav1.Time { return metav1.NewTime(now.Add(d)) }
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop", CreationTimestamp: at(-10 * time.Minute)},
		Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "api",
			LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health"}}, InitialDelaySeconds: 5, PeriodSeconds: 5, FailureThreshold: 3, TimeoutSeconds: 1}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: "True", LastTransitionTime: at(-9 * time.Minute)},
				{Type: corev1.PodInitialized, Status: "True", LastTransitionTime: at(-8 * time.Minute)},
				{Type: corev1.PodReady, Status: "True", LastTransitionTime: at(-6 * time.Minute)}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: at(-7 * time.Minute)}}}}}}
	ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e", Namespace: "shop"}, InvolvedObject: corev1.ObjectReference{Name: "api-1"}, Reason: "Pulled",
		Message: `Successfully pulled image "shop/api:1" in 75.5s (75.5s including waiting)`}
	c := NewForTesting(fake.NewSimpleClientset(p, ev), "dev", "shop", map[string]string{"dev": "dev"})
	r, err := c.StartupBreakdown(context.Background(), "shop", "api-1")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, ph := range r.Phases {
		names = append(names, ph.Name)
	}
	advice := strings.Join(r.Advice, "\n")
	if !strings.Contains(strings.Join(names, ","), "scheduling") || !strings.Contains(strings.Join(names, ","), "readiness") || r.Total != 4*time.Minute {
		t.Fatalf("phases %v total %s", names, r.Total)
	}
	for _, w := range []string{"add a startupProbe", "timeout is 1s", "no readinessProbe", "took 1m16s"} {
		if !strings.Contains(advice, w) {
			t.Errorf("advice missing %q:\n%s", w, advice)
		}
	}
}

func TestAutoscalers(t *testing.T) {
	min1, util := int32(1), int32(70)
	h := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"}, MinReplicas: &min1, MaxReplicas: 4,
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: "cpu", Target: autoscalingv2.MetricTarget{AverageUtilization: &util}}}}},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 4,
			Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingLimited, Status: "True", Reason: "TooManyReplicas"}}}}
	c := NewForTesting(fake.NewSimpleClientset(h), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = nil
	list, err := c.Autoscalers(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	if list[0].H != HealthWarn || !strings.Contains(list[0].Status, "stuck at max") || !strings.Contains(list[0].Metrics, "cpu ?/70%") {
		t.Fatalf("%+v", list[0])
	}
}

func TestKubeconfigRenameDelete(t *testing.T) {
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	os.WriteFile(kc, []byte(`apiVersion: v1
kind: Config
current-context: a
clusters:
- name: ca
  cluster: {server: https://a.example}
- name: cb
  cluster: {server: https://b.example}
users:
- name: ua
  user: {token: "tok-a"}
- name: ub
  user: {token: "tok-b"}
contexts:
- name: a
  context: {cluster: ca, user: ua}
- name: b
  context: {cluster: cb, user: ub}
`), 0o600)
	if _, err := RenameContext(kc, "a", "alpha"); err != nil {
		t.Fatal(err)
	}
	bak, err := DeleteContext(kc, "b")
	if err != nil || bak == "" {
		t.Fatal(err, bak)
	}
	list, _ := KubeContexts(kc)
	if len(list) != 1 || list[0].Name != "alpha" || !list[0].Current || list[0].Server != "https://a.example" {
		t.Fatalf("%+v", list)
	}
	b, _ := os.ReadFile(kc)
	if strings.Contains(string(b), "cb") || strings.Contains(string(b), "ub") {
		t.Fatalf("unused cluster/user should be removed:\n%s", b)
	}
	if _, err := RenameContext(kc, "nope", "x"); err == nil {
		t.Error("renaming a missing context must fail")
	}
}

func TestParseLsAndManifests(t *testing.T) {
	es := ParseLs("total 8\ndrwxr-xr-x    2 root     root          4096 Nov 16 10:00 .\ndrwxr-xr-x    1 root     root          4096 Nov 16 10:00 ..\n-rw-r--r--    1 root     root           120 Nov 16 10:00 app config.yaml\nlrwxrwxrwx    1 root     root             9 Nov 16 10:00 data -> /mnt/data\ndrwxr-xr-x    2 root     root          4096 Nov 16 10:00 logs\n")
	if len(es) != 3 || es[0].Name != "app config.yaml" || es[0].Size != 120 || !es[1].Link || es[1].Target != "/mnt/data" || !es[2].Dir {
		t.Fatalf("%+v", es)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: one\n---\napiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Secret\n  metadata:\n    name: two\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644)
	y, how, err := RenderLocal(context.Background(), dir)
	if err != nil || how != "1 files" {
		t.Fatal(err, how)
	}
	objs, err := ParseManifests(y)
	if err != nil || len(objs) != 2 || objs[1].GetKind() != "Secret" {
		t.Fatalf("%v %d", err, len(objs))
	}
}

func TestMeshAndCosts(t *testing.T) {
	ready := true
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: map[string]string{"istio-injection": "enabled"}}}
	meshed := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "shop"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "istio-proxy"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "istio-proxy", Ready: true, Started: &ready}}}}
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "shop"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	c := NewForTesting(fake.NewSimpleClientset(ns, meshed, plain), "dev", "shop", map[string]string{"dev": "dev"})
	ms, err := c.MeshStatus(context.Background())
	if err != nil || len(ms) != 1 || ms[0].Mesh != "istio" || ms[0].WithSidecar != 1 || ms[0].H != HealthWarn {
		t.Fatalf("%v %+v", err, ms)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("aggregate") != "namespace" {
			t.Errorf("aggregate=%s", r.URL.Query().Get("aggregate"))
		}
		w.Write([]byte(`{"code":200,"data":[{"shop":{"cpuCost":1.5,"ramCost":0.5,"pvCost":0.1,"networkCost":0,"totalCost":2.1,"totalEfficiency":0.25},"__idle__":{"totalCost":9}}]}`))
	}))
	defer srv.Close()
	lines, _, err := c.Costs(context.Background(), srv.URL, "7d", "namespace")
	if err != nil || len(lines) != 1 || lines[0].Name != "shop" || lines[0].Total != 2.1 {
		t.Fatalf("%v %+v", err, lines)
	}
}
