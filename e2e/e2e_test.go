//go:build e2e

// Package e2e runs kubesage's cluster operations against a real, throwaway cluster.
//
//	kind create cluster --name kubesage-e2e
//	go test -tags e2e ./e2e/ -v
//
// It refuses to run unless the current context starts with "kind-", so it can never touch a real cluster.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

const ns = "kubesage-e2e"

func client(t *testing.T) *kube.Client {
	t.Helper()
	c, err := kube.New(os.Getenv("KUBECONFIG"), "", ns)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Context, "kind-") {
		t.Skipf("context %q is not a kind cluster; e2e tests only run on kind-* contexts", c.Context)
	}
	return c
}

func setup(t *testing.T, c *kube.Client) {
	t.Helper()
	ctx := context.Background()
	_, err := c.Clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	two := int32(2)
	lbl := map[string]string{"app": "web"}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns, Labels: lbl},
		Spec: appsv1.DeploymentSpec{Replicas: &two, Selector: &metav1.LabelSelector{MatchLabels: lbl},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: lbl}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "web", Image: "nginx:1.27-alpine", Ports: []corev1.ContainerPort{{ContainerPort: 80}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(80)}}},
			}}}}}}
	if _, err := c.Clientset.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: corev1.ServiceSpec{Selector: lbl, Ports: []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(80)}}}}
	if _, err := c.Clientset.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	waitReady(t, c, 2)
}

func waitReady(t *testing.T, c *kube.Client, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := c.Clientset.AppsV1().Deployments(ns).Get(context.Background(), "web", metav1.GetOptions{})
		if err == nil && d.Status.ReadyReplicas == want && d.Status.UpdatedReplicas == want {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("deployment web did not reach %d ready replicas", want)
}

func TestE2E(t *testing.T) {
	c := client(t)
	setup(t, c)
	ctx := context.Background()
	deps, _ := kube.Lookup("deployments")
	svcs, _ := kube.Lookup("services")

	t.Run("list", func(t *testing.T) {
		tbl, err := deps.List(ctx, c)
		if err != nil || len(tbl.Rows) != 1 || tbl.Rows[0].Name != "web" {
			t.Fatalf("list = %+v, %v", tbl.Rows, err)
		}
	})

	t.Run("exec", func(t *testing.T) {
		ps, err := c.PodsFor(ctx, deps, ns, "web")
		if err != nil || len(ps) == 0 {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = c.Exec(ctx, kube.ExecOptions{Namespace: ns, Pod: ps[0].Name, Container: "web", Command: []string{"cat", "/etc/hostname"}, Stdout: &out, Stderr: io.Discard})
		if err != nil || strings.TrimSpace(out.String()) != ps[0].Name {
			t.Fatalf("exec = %q, %v", out.String(), err)
		}
	})

	t.Run("port-forward", func(t *testing.T) {
		targets, err := c.PortTargets(ctx, svcs, ns, "web")
		if err != nil || len(targets) == 0 {
			t.Fatalf("targets = %v, %v", targets, err)
		}
		local := kube.SuggestLocalPort(18080)
		f, err := c.PortForward(targets[0], local, targets[0].Port)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Stop()
		var resp *http.Response
		for i := 0; i < 20; i++ {
			if resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/", local)); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET through forward: %v %v", resp, err)
		}
		resp.Body.Close()
	})

	t.Run("apply", func(t *testing.T) {
		objs, err := kube.ParseManifests("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: e2e-settings\ndata:\n  mode: \"on\"\n")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := c.PlanApply(ctx, objs, ns)
		if err != nil || len(plan) != 1 || plan[0].State != "new" {
			t.Fatalf("plan = %+v, %v", plan, err)
		}
		if n, errs := c.ExecuteApply(ctx, plan); n != 1 || len(errs) > 0 {
			t.Fatalf("apply = %d, %v", n, errs)
		}
		plan, _ = c.PlanApply(ctx, objs, ns)
		if plan[0].State != "unchanged" {
			t.Fatalf("second plan = %s", plan[0].State)
		}
	})

	t.Run("schedule", func(t *testing.T) {
		ws, err := c.ScheduleTargets(ctx, ns, "app=web")
		if err != nil || len(ws) != 1 {
			t.Fatalf("targets = %+v, %v", ws, err)
		}
		if _, err := c.ScaleToZero(ctx, ws, false); err != nil {
			t.Fatal(err)
		}
		waitReady(t, c, 0)
		ws, _ = c.ScheduleTargets(ctx, ns, "app=web")
		if _, err := c.RestoreScale(ctx, ws, false); err != nil {
			t.Fatal(err)
		}
		waitReady(t, c, 2)
	})

	t.Run("chaos", func(t *testing.T) {
		res, err := c.ChaosKillPod(ctx, deps, ns, "web", 3*time.Minute)
		if err != nil || !res.Recovered {
			t.Fatalf("chaos = %+v, %v", res, err)
		}
		t.Logf("recovered in %s", res.After)
	})

	t.Run("drain-plan", func(t *testing.T) {
		ps, _ := c.PodsFor(ctx, deps, ns, "web")
		plan, err := c.PlanDrain(ctx, ps[0].Spec.NodeName)
		if err != nil || len(plan.Items) == 0 {
			t.Fatalf("drain plan = %+v, %v", plan, err)
		}
	})

	t.Run("events-and-fleet", func(t *testing.T) {
		store := &kube.EventStore{Dir: t.TempDir()}
		evs, err := c.CollectEvents(ctx)
		if err != nil || len(evs) == 0 {
			t.Fatalf("events = %d, %v", len(evs), err)
		}
		if n, err := store.Record(c.Context, evs); err != nil || n == 0 {
			t.Fatalf("record = %d, %v", n, err)
		}
		s, err := c.Fleet(ctx)
		if err != nil || s.Nodes < 1 || s.Pods < 2 {
			t.Fatalf("fleet = %+v, %v", s, err)
		}
	})

	_ = c.Clientset.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
}
