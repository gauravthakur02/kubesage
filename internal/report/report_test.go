package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

func TestBuildAndPost(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 9,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	c := kube.NewForTesting(fake.NewSimpleClientset(pod), "dev", "shop", map[string]string{"dev": "dev"})
	text := Build(context.Background(), []*kube.Client{c}, Options{}, time.Now())
	if !strings.Contains(text, "*dev*") || !strings.Contains(text, "shop/api-1") {
		t.Fatalf("report:\n%s", text)
	}
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := Post(context.Background(), srv.URL, text); err != nil || got["text"] != text {
		t.Fatalf("post: %v %v", err, got)
	}
}
