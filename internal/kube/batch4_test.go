package kube

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEventStoreRecordLoadPrune(t *testing.T) {
	s := &EventStore{Dir: t.TempDir()}
	now := time.Now()
	evs := []StoredEvent{
		{UID: "a", Namespace: "shop", Kind: "Pod", Name: "web-1", Type: "Warning", Reason: "BackOff", Last: now.Add(-2 * time.Hour), Count: 1},
		{UID: "b", Namespace: "shop", Kind: "Pod", Name: "web-2", Type: "Normal", Reason: "Pulled", Last: now.Add(-10 * 24 * time.Hour), Count: 1},
	}
	if n, err := s.Record("ctx/with:odd chars", evs); err != nil || n != 2 {
		t.Fatalf("record = %d, %v", n, err)
	}
	if n, _ := s.Record("ctx/with:odd chars", evs); n != 0 {
		t.Fatalf("unchanged events stored again: %d", n)
	}
	evs[0].Count, evs[0].Last = 3, now
	if n, _ := s.Record("ctx/with:odd chars", evs); n != 1 {
		t.Fatalf("count increase not stored: %d", n)
	}
	got, err := s.Load("ctx/with:odd chars", time.Time{})
	if err != nil || len(got) != 2 || got[0].UID != "a" || got[0].Count != 3 {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if err := s.Prune("ctx/with:odd chars", 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Load("ctx/with:odd chars", time.Time{})
	if len(got) != 1 || got[0].UID != "a" {
		t.Fatalf("after prune = %+v", got)
	}
	// A fresh store (new kubesage run) must not re-store what is on disk.
	s2 := &EventStore{Dir: s.Dir}
	if n, _ := s2.Record("ctx/with:odd chars", evs[:1]); n != 0 {
		t.Fatalf("restart re-stored %d events", n)
	}
}

func TestTimelineMergesHistory(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"}})
	c := NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"})
	hist := []StoredEvent{{UID: "old", Namespace: "shop", Kind: "Pod", Name: "web-1", Type: "Warning", Reason: "OOMKilling",
		Message: "killed", Last: time.Now().Add(-30 * time.Hour), Count: 2}}
	tl, err := c.TimelineWith(context.Background(), "Pod", "shop", "web-1", hist)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range tl {
		if e.Text == "OOMKilling: killed (history)" && e.Health == HealthErr {
			found = true
		}
	}
	if !found {
		t.Fatalf("history entry missing: %+v", tl)
	}
}

func TestShouldBeDown(t *testing.T) {
	mon := time.Date(2026, 11, 16, 0, 0, 0, 0, time.Local) // a Monday
	at := func(d time.Time, h, m int) time.Time { return d.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	cases := []struct {
		now  time.Time
		down bool
	}{
		{at(mon, 6, 59), true}, {at(mon, 7, 0), false}, {at(mon, 12, 0), false}, {at(mon, 19, 0), true}, {at(mon, 23, 30), true},
		{at(mon.AddDate(0, 0, 5), 12, 0), true}, // Saturday: down all day
	}
	for _, c := range cases {
		got, err := ShouldBeDown(c.now, "19:00", "07:00", nil)
		if err != nil || got != c.down {
			t.Errorf("%s: down=%v err=%v, want %v", c.now.Format("Mon 15:04"), got, err, c.down)
		}
	}
	if got, _ := ShouldBeDown(at(mon, 2, 0), "01:00", "05:00", []string{"monday"}); !got {
		t.Error("same-day window not applied")
	}
	if _, err := ShouldBeDown(mon, "25:00", "07:00", nil); err == nil {
		t.Error("bad time accepted")
	}
	if _, err := ShouldBeDown(mon, "19:00", "07:00", []string{"funday"}); err == nil {
		t.Error("bad day accepted")
	}
}

func TestScaleToZeroAndRestore(t *testing.T) {
	two := int32(2)
	cs := fake.NewSimpleClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "dev", Labels: map[string]string{"tier": "app"}},
		Spec: appsv1.DeploymentSpec{Replicas: &two}})
	c := NewForTesting(cs, "dev", "dev", map[string]string{"dev": "dev"})
	ctx := context.Background()
	ws, err := c.ScheduleTargets(ctx, "dev", "tier=app")
	if err != nil || len(ws) != 1 {
		t.Fatalf("targets = %+v, %v", ws, err)
	}
	if done, err := c.ScaleToZero(ctx, ws, false); err != nil || len(done) != 1 {
		t.Fatalf("down = %v, %v", done, err)
	}
	d, _ := cs.AppsV1().Deployments("dev").Get(ctx, "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 0 || d.Annotations[ScaledDownAnnotation] != "2" {
		t.Fatalf("after down: replicas=%d annotations=%v", *d.Spec.Replicas, d.Annotations)
	}
	ws, _ = c.ScheduleTargets(ctx, "dev", "")
	if done, _ := c.ScaleToZero(ctx, ws, false); len(done) != 0 {
		t.Fatal("scaled-down workload scaled again")
	}
	if done, err := c.RestoreScale(ctx, ws, false); err != nil || len(done) != 1 {
		t.Fatalf("up = %v, %v", done, err)
	}
	d, _ = cs.AppsV1().Deployments("dev").Get(ctx, "web", metav1.GetOptions{})
	if *d.Spec.Replicas != 2 || d.Annotations[ScaledDownAnnotation] != "" {
		t.Fatalf("after up: replicas=%d annotations=%v", *d.Spec.Replicas, d.Annotations)
	}
}

func TestCustomRules(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "web", "namespace": "shop", "labels": map[string]any{"app": "web"}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{
			map[string]any{"name": "a", "image": "nginx:latest", "resources": map[string]any{"limits": map[string]any{"memory": "256Mi"}}},
			map[string]any{"name": "b", "image": "registry.example/b:1.2"},
		}}}},
	}}
	check := func(r CustomRule) string {
		t.Helper()
		cr, err := compileRule(r)
		if err != nil {
			t.Fatal(err)
		}
		return evalRule(cr, obj)
	}
	if msg := check(CustomRule{Name: "owner", RequireLabels: []string{"owner", "app"}}); msg != "missing label owner" {
		t.Errorf("labels: %q", msg)
	}
	if msg := check(CustomRule{Name: "tags", Path: "spec.template.spec.containers[*].image", Forbid: `:latest$`}); msg == "" {
		t.Error("latest tag not caught")
	}
	if msg := check(CustomRule{Name: "limits", Path: "spec.template.spec.containers[*].resources.limits.memory"}); msg != "spec.template.spec.containers[*].resources.limits.memory is not set on 1 of 2 items" {
		t.Errorf("limits: %q", msg)
	}
	if msg := check(CustomRule{Name: "first", Path: "spec.template.spec.containers[0].name", Match: "^a$"}); msg != "" {
		t.Errorf("index path: %q", msg)
	}
	if _, err := compileRule(CustomRule{Name: "x", Match: "a"}); err == nil {
		t.Error("match without path accepted")
	}
}

func TestChaosRefusesOwnerlessPod(t *testing.T) {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "dev", UID: types.UID("1")},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	cs := fake.NewSimpleClientset(p)
	c := NewForTesting(cs, "dev", "dev", map[string]string{"dev": "dev"})
	pods, _ := Lookup("pods")
	if _, err := c.ChaosKillPod(context.Background(), pods, "dev", "solo", time.Second); err == nil {
		t.Fatal("ownerless pod would be deleted with nothing to replace it")
	}
	if _, err := cs.CoreV1().Pods("dev").Get(context.Background(), "solo", metav1.GetOptions{}); err != nil {
		t.Fatal("pod was deleted")
	}
}
