package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

func TestServe(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "shop/web:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "web", RestartCount: 9,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	c := kube.NewForTesting(fake.NewSimpleClientset(pod), "dev", "shop", map[string]string{"dev": "dev"})
	s := &Server{Version: "test", Default: c}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"find_issues","arguments":{"namespace":"shop"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_resources","arguments":{"resource":"pods"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":6,"method":"bogus"}`,
	}, "\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		got[string(must(json.Marshal(m["id"])))] = m
	}
	if len(got) != 6 {
		t.Fatalf("expected 6 responses (notification gets none), got %d:\n%s", len(got), out.String())
	}
	if !strings.Contains(out.String(), `"protocolVersion":"`+ProtocolVersion+`"`) || !strings.Contains(out.String(), `"readOnlyHint":true`) {
		t.Fatalf("initialize/tools wrong:\n%s", out.String())
	}
	text := func(id string) string {
		r := got[id]["result"].(map[string]any)
		return r["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if !strings.Contains(text("3"), "CrashLoopBackOff") {
		t.Errorf("find_issues: %s", text("3"))
	}
	if !strings.Contains(text("4"), "web-1") {
		t.Errorf("list_resources: %s", text("4"))
	}
	if got["5"]["error"] == nil || got["6"]["error"] == nil {
		t.Error("unknown tool / method must error")
	}
}

func must(b []byte, _ error) []byte { return b }
