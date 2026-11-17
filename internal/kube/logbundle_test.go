package kube

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLogBundle(t *testing.T) {
	lbl := map[string]string{"app": "web"}
	dep := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: lbl}}}
	pod := func(name string, restarts int32) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: lbl},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}, {Name: "proxy"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "web", RestartCount: restarts}, {Name: "proxy"}}}}
	}
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	c := NewForTesting(fake.NewSimpleClientset(pod("web-a", 2), pod("web-b", 0)), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{}, dep)
	deps, _ := Lookup("deployments")
	var buf bytes.Buffer
	res, err := c.WriteLogBundle(context.Background(), &buf, deps, "shop", "web", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		names[f.Name] = string(b)
	}
	for _, want := range []string{"web-a/web.log", "web-a/web.previous.log", "web-a/proxy.log", "web-b/web.log", "web-b/proxy.log", "SUMMARY.txt"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	if _, ok := names["web-b/web.previous.log"]; ok {
		t.Error("previous log for a container that never restarted")
	}
	if res.Pods != 2 || res.Files != 5 || !strings.Contains(names["SUMMARY.txt"], "restarts=2") || !strings.Contains(names["web-a/web.log"], "fake logs") {
		t.Fatalf("result %+v summary:\n%s", res, names["SUMMARY.txt"])
	}
	buf.Reset()
	if res, err := c.WriteLogBundleForPods(context.Background(), &buf, "shop", []string{"web-b", "gone"}, 0); err != nil || res.Pods != 1 {
		t.Fatalf("for pods = %+v %v", res, err)
	}
}
