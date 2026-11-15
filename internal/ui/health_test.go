package ui

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"kubesage/internal/kube"
)

func healthyPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour))},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{
			Name: "api", Image: "shop/api:1.4",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
			ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt(8080)}}},
		}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: corev1.PodQOSBurstable, PodIP: "10.0.0.5",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "api", Ready: true, RestartCount: 2,
				State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137, FinishedAt: metav1.Now()}},
			}}},
	}
}

func podMetrics(ns, name, cpu, mem string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"containers": []any{map[string]any{"name": "api", "usage": map[string]any{"cpu": cpu, "memory": mem}}},
	}}
}

func newHealthApp(t *testing.T, objs ...runtime.Object) *App {
	t.Helper()
	sch := runtime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(sch, map[schema.GroupVersionResource]string{gvr: "PodMetricsList"})
	if err := dyn.Tracker().Create(gvr, podMetrics("shop", "api-1", "150m", "240Mi"), "shop"); err != nil {
		t.Fatal(err)
	}
	c := kube.NewForTesting(fake.NewSimpleClientset(objs...), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	a := New(testConfig(t), c, Options{})
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 50})
	run(a, a.refresh(), 0)
	return a
}

func TestPodsTableShowsUsage(t *testing.T) {
	a := newHealthApp(t, healthyPod())
	if got := strings.Join(a.table.Columns, " "); !strings.Contains(got, "CPU MEM") {
		t.Fatalf("CPU/MEM columns missing: %s", got)
	}
	row := a.table.Rows[0]
	if row.Cells[4] != "150m 75%" || row.Cells[5] != "240Mi 93%" {
		t.Fatalf("usage cells = %q %q", row.Cells[4], row.Cells[5])
	}
	if row.Health != kube.HealthWarn {
		t.Fatal("93% of the memory limit should mark the pod as warning")
	}
}

func TestPodHealthReport(t *testing.T) {
	a := newHealthApp(t, healthyPod())
	press(a, "h")
	if a.mode != modeText || a.textKind != "health" {
		t.Fatalf("h should open the health view, mode=%v kind=%s", a.mode, a.textKind)
	}
	view := ansi.Strip(strings.Join(a.textLines, "\n"))
	for _, want := range []string{
		"Degraded: OOMKilled", "QoS Burstable", "✓ PodScheduled", "✓ Ready",
		"OOMKilled (exit 137)", "150m", " 75% of limit", "240Mi", " 93% of limit", "req 100m · limit 200m",
		"readiness HTTP GET :8080 /ready", "no liveness probe", "memory at 93% of limit",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("health view missing %q\n%s", want, view)
		}
	}
	assertFits(t, a)
}

func selfSignedPEM(t *testing.T, cn string, notAfter time.Time) []byte {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: notAfter, DNSNames: []string{cn}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestSecretViewerMaskRevealAutoMask(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"password": []byte("hunter2-secret"),
			"config":   []byte(`{"user":"app","pool":5}`),
			"tls.crt":  selfSignedPEM(t, "api.shop.example", time.Now().Add(10*24*time.Hour)),
		}}
	a := newHealthApp(t, healthyPod(), sec)
	a.client.Context = "dev"
	run(a, a.runCommand("secrets"), 0)
	press(a, "enter")
	view := ansi.Strip(strings.Join(a.textLines, "\n"))
	if strings.Contains(view, "hunter2-secret") || !strings.Contains(view, "● masked") {
		t.Fatalf("values must be masked by default:\n%s", view)
	}
	if !strings.Contains(view, "cert: api.shop.example") || !strings.Contains(view, "▲ expires") || !strings.Contains(view, "JSON") {
		t.Fatalf("public cert details and value types should show while masked:\n%s", view)
	}

	press(a, "x") // asks first
	if a.ov != ovConfirm {
		t.Fatal("unmasking needs confirmation")
	}
	press(a, "y")
	view = ansi.Strip(strings.Join(a.textLines, "\n"))
	if !strings.Contains(view, "hunter2-secret") || !strings.Contains(view, `"pool": 5`) || !strings.Contains(view, "UNMASKED") {
		t.Fatalf("decoded values should show after confirming:\n%s", view)
	}

	a.revealedAt = time.Now().Add(-2 * revealFor) // time passes
	a.autoMask()
	if a.reveal || strings.Contains(ansi.Strip(strings.Join(a.textLines, "\n")), "hunter2-secret") {
		t.Fatal("values must mask again automatically")
	}

	press(a, "x")
	press(a, "y")
	press(a, "esc")
	if a.reveal || a.secret != nil {
		t.Fatal("leaving the viewer must drop the decoded secret")
	}
}

func TestSecretRevealOnProdNeedsTypedName(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}, Data: map[string][]byte{"k": []byte("v")}}
	a := newHealthApp(t, sec)
	a.client.Context = "aks-prod"
	run(a, a.openSecret("shop", "db"), 0)
	press(a, "x")
	press(a, "y")
	if a.reveal {
		t.Fatal("a stray y must not unmask on a production context")
	}
	a.input.SetValue("db")
	press(a, "enter")
	if !a.reveal {
		t.Fatal("typing the secret name should unmask")
	}
}

func TestUsageHistoryAndSparklines(t *testing.T) {
	if got := sparkline([]int64{1, 2, 3, 4, 5, 6, 7, 8}, 8); got != "▁▂▃▄▅▆▇█" {
		t.Fatalf("sparkline = %q", got)
	}
	if got := sparkline([]int64{5, 5, 5}, 8); got != "▁▁▁" {
		t.Fatalf("flat sparkline = %q", got)
	}
	h := newHistory()
	base := time.Now()
	for i := 0; i < 200; i++ {
		h.record("k", kube.Usage{CPUMilli: int64(i), MemBytes: int64(i) << 20}, base.Add(time.Duration(i)*5*time.Second))
	}
	s := h.get("k")
	if len(s) != histKeep || s[1].at.Sub(s[0].at) < histInterval {
		t.Fatalf("history must be throttled to one sample per %s and capped at %d: n=%d", histInterval, histKeep, len(s))
	}

	a := newHealthApp(t, healthyPod())
	a.hist = newHistory() // start empty; samples arrive in time order
	key := histKey(a.client.Context, "shop", "api-1")
	for i, mem := range []int64{100, 150, 240} {
		a.hist.record(key, kube.Usage{CPUMilli: 50 + int64(i)*50, MemBytes: mem << 20}, time.Now().Add(time.Duration(i-3)*time.Minute))
	}
	a.Update(tea.WindowSizeMsg{Width: 170, Height: 40})
	run(a, a.refresh(), 0)
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "MEM 15m") || !strings.Contains(view, "▁") {
		t.Fatalf("pods table should show the memory trend:\n%s", view)
	}
	press(a, "h")
	health := ansi.Strip(strings.Join(a.textLines, "\n"))
	if !strings.Contains(health, "Mem trend") || !strings.Contains(health, "100Mi → 240Mi") {
		t.Fatalf("health report should show trends:\n%s", health)
	}
}
