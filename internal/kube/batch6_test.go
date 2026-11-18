package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestChanges(t *testing.T) {
	now := time.Now()
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-10 * time.Minute)),
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "7"}, OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: &yes}}},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: "web:2"}}}}}}
	old := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-48 * time.Hour))}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-72 * time.Hour)),
		ManagedFields: []metav1.ManagedFieldsEntry{{Time: &metav1.Time{Time: now.Add(-5 * time.Minute)}}}}}
	helm := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.web.v3", Namespace: "shop", CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
		Labels: map[string]string{"name": "web", "version": "3", "status": "deployed"}}, Type: "helm.sh/release.v1"}
	ev := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: "shop"}, Reason: "ScalingReplicaSet", Message: "Scaled up replica set web-2 to 3",
		InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "web"}, LastTimestamp: metav1.NewTime(now.Add(-9 * time.Minute))}
	c := NewForTesting(fake.NewSimpleClientset(rs, old, cm, helm, ev), "dev", "shop", map[string]string{"dev": "dev"})
	cs, err := c.Changes(context.Background(), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, x := range cs {
		got[x.Source+":"+x.Name] = true
	}
	for _, want := range []string{"rollout:web", "config:settings", "helm:web", "scale:web"} {
		if !got[want] {
			t.Errorf("missing %s in %+v", want, cs)
		}
	}
	if len(cs) != 4 || cs[0].Source != "helm" {
		t.Fatalf("old change included or wrong order: %+v", cs)
	}
}

func TestSnapshotsSaveLoadDiff(t *testing.T) {
	dep := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", UID: "1", ResourceVersion: "5"}}
	sec := &corev1.Secret{TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}, Data: map[string][]byte{"pw": []byte("hunter2")}}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	lists := map[schema.GroupVersionResource]string{}
	for _, k := range SnapshotKinds {
		r, _ := Lookup(k)
		lists[r.GVR] = r.Kind + "List"
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, lists, dep, sec)
	c := NewForTesting(fake.NewSimpleClientset(), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	ctx, base := context.Background(), t.TempDir()
	s1, err := c.TakeSnapshot(ctx, "shop", "before")
	if err != nil || len(s1.Objects) != 2 {
		t.Fatalf("snapshot = %+v, %v", s1, err)
	}
	if strings.Contains(s1.Objects["Secret/db"], "hunter2") || strings.Contains(s1.Objects["Secret/db"], "aHVudGVyMg") {
		t.Fatal("secret value stored in the snapshot")
	}
	if _, err := s1.Save(base); err != nil {
		t.Fatal(err)
	}
	old, err := LoadSnapshot(base, "dev", "shop", "before")
	if err != nil || len(old.Objects) != 2 {
		t.Fatalf("load = %+v, %v", old, err)
	}
	cur := &Snapshot{Objects: map[string]string{}}
	for k, v := range old.Objects {
		cur.Objects[k] = v
	}
	cur.Objects["Deployment/web"] += "  tier: front\n"
	delete(cur.Objects, "Secret/db")
	cur.Objects["Service/web"] = "kind: Service\n"
	d := DiffSnapshots(old, cur)
	if len(d) != 3 || d[0].Key != "Deployment/web" || d[0].State != "changed" || d[1].State != "removed" || d[2].State != "added" {
		t.Fatalf("diff = %+v", d)
	}
	ls, _ := ListSnapshots(base, "dev", "shop")
	if len(ls) != 1 || ls[0].Name != "before" {
		t.Fatalf("list = %+v", ls)
	}
	_ = filepath.Join
}

func TestEvalSLO(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		v := 0.0
		switch {
		case strings.Contains(q, "good") && strings.Contains(q, "[30d]"):
			v = 9990
		case strings.Contains(q, "total") && strings.Contains(q, "[30d]"):
			v = 10000
		case strings.Contains(q, "good") && strings.Contains(q, "[1h]"):
			v = 98
		case strings.Contains(q, "total") && strings.Contains(q, "[1h]"):
			v = 100
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"%g"]}]}}`, v)
	}))
	defer srv.Close()
	c := NewForTesting(fake.NewSimpleClientset(), "dev", "shop", map[string]string{"dev": "dev"})
	p, err := c.NewProm(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.EvalSLO(context.Background(), "sum(increase(good[{{window}}]))", "sum(increase(total[{{window}}]))", 0.995, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// 99.9% available vs 99.5% objective: 80% of the budget left; last hour 98% = 4x burn
	if fmt.Sprintf("%.3f %.2f %.1f", r.Availability, r.BudgetLeft, r.Burn1h) != "0.999 0.80 4.0" {
		t.Fatalf("slo = %+v", r)
	}
	if _, err := p.EvalSLO(context.Background(), "g", "t", 1.5, time.Hour); err == nil {
		t.Fatal("bad objective accepted")
	}
	_ = url.Values{}
}

func TestParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{"2h": 2 * time.Hour, "7d": 7 * 24 * time.Hour, "": time.Hour, "x": time.Hour, "30m": 30 * time.Minute} {
		if got := ParseSince(in, time.Hour); got != want {
			t.Errorf("%q = %s", in, got)
		}
	}
}

func TestSafeFileNames(t *testing.T) {
	for in, want := range map[string]string{"..": "_", "../etc": "_etc", "a/b": "a_b", ".hidden": "hidden", "ok-1.2": "ok-1.2"} {
		if got := unsafeFile.ReplaceAllString(in, "_"); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}

func TestCrashLoopErrorWindow(t *testing.T) {
	pod := func(cs corev1.ContainerStatus) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "x"}, Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{cs}}}
	}
	errWindow := pod(corev1.ContainerStatus{Name: "app", RestartCount: 3, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}}})
	between := pod(corev1.ContainerStatus{Name: "app", RestartCount: 4, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}}})
	healthy := pod(corev1.ContainerStatus{Name: "app", RestartCount: 4, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}}})
	for name, p := range map[string]*corev1.Pod{"terminated Error": errWindow, "running again, not ready": between} {
		is := PodIssues(p)
		if len(is) == 0 || is[0].Severity != HealthErr {
			t.Errorf("%s: %+v, want an error", name, is)
		}
	}
	for _, is := range PodIssues(healthy) {
		if is.Severity == HealthErr {
			t.Errorf("recovered pod is an error: %+v", is)
		}
	}
}

func TestScanDropsWarningsFromBeforeReady(t *testing.T) {
	now := time.Now()
	readyPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Status: corev1.PodStatus{Phase: corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now.Add(-5 * time.Minute))}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	ev := func(name string, at time.Time) *corev1.Event {
		return &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"}, Type: corev1.EventTypeWarning, Reason: "Unhealthy",
			Message: "Readiness probe failed", LastTimestamp: metav1.NewTime(at), InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: "web"}}
	}
	c := NewForTesting(fake.NewSimpleClientset(readyPod, ev("old", now.Add(-10*time.Minute))), "dev", "shop", map[string]string{"dev": "dev"})
	if is, _ := c.Scan(context.Background()); len(is) != 0 {
		t.Fatalf("startup warning before Ready reported: %+v", is)
	}
	c = NewForTesting(fake.NewSimpleClientset(readyPod, ev("new", now.Add(-time.Minute))), "dev", "shop", map[string]string{"dev": "dev"})
	if is, _ := c.Scan(context.Background()); len(is) != 1 {
		t.Fatalf("warning after Ready dropped: %+v", is)
	}
}
