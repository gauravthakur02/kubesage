package bot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	"github.com/gauravthakur02/kubeweave/internal/kube"
	"github.com/gauravthakur02/kubeweave/internal/owners"
)

func testBot(t *testing.T) *Bot {
	dep := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", Labels: map[string]string{"team": "storefront"}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"}, Status: corev1.PodStatus{Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 9, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}}
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	c := kube.NewForTesting(fake.NewSimpleClientset(dep, pod), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{}, dep)
	return &Bot{Client: func(string) (*kube.Client, error) { return c, nil }, SlackSecret: []byte("s3cret"), TeamsToken: []byte("tok"),
		Owners: []owners.Rule{{Team: "platform", Namespaces: "^kube-"}}}
}

func TestAnswers(t *testing.T) {
	b := testBot(t)
	ctx := context.Background()
	if a := b.Answer(ctx, "issues shop"); !strings.Contains(a, "api-1") || !strings.Contains(a, "CrashLoopBackOff") {
		t.Fatalf("issues: %s", a)
	}
	if a := b.Answer(ctx, "owner shop/web"); !strings.Contains(a, "storefront") {
		t.Fatalf("owner: %s", a)
	}
	if a := b.Answer(ctx, "dance"); !strings.Contains(a, "read-only") {
		t.Fatalf("help: %s", a)
	}
}

func TestSlackAndTeamsSignatures(t *testing.T) {
	b := testBot(t)
	now := time.Unix(1_800_000_000, 0)
	b.Now = func() time.Time { return now }
	srv := httptest.NewServer(b.Handler())
	defer srv.Close()
	body := "command=%2Fkubeweave&text=issues+shop"
	ts := fmt.Sprint(now.Unix())
	m := hmac.New(sha256.New, b.SlackSecret)
	fmt.Fprintf(m, "v0:%s:%s", ts, body)
	send := func(sig, ts string) (int, string) {
		req, _ := http.NewRequest("POST", srv.URL+"/slack", strings.NewReader(body))
		req.Header.Set("X-Slack-Signature", sig)
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		var out map[string]string
		_ = json.NewDecoder(r.Body).Decode(&out)
		return r.StatusCode, out["text"]
	}
	if code, text := send("v0="+hex.EncodeToString(m.Sum(nil)), ts); code != 200 || !strings.Contains(text, "api-1") {
		t.Fatalf("slack = %d %q", code, text)
	}
	if code, _ := send("v0=bad", ts); code != 401 {
		t.Fatalf("bad slack signature = %d", code)
	}
	if code, _ := send("v0="+hex.EncodeToString(m.Sum(nil)), fmt.Sprint(now.Add(-time.Hour).Unix())); code != 401 {
		t.Fatalf("replayed slack request = %d", code)
	}
	tb := []byte(`{"type":"message","text":"<at>kubeweave</at> owner shop/web"}`)
	tm := hmac.New(sha256.New, b.TeamsToken)
	tm.Write(tb)
	req, _ := http.NewRequest("POST", srv.URL+"/teams", strings.NewReader(string(tb)))
	req.Header.Set("Authorization", "HMAC "+base64.StdEncoding.EncodeToString(tm.Sum(nil)))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	_ = json.NewDecoder(r.Body).Decode(&out)
	r.Body.Close()
	if r.StatusCode != 200 || !strings.Contains(out["text"], "storefront") {
		t.Fatalf("teams = %d %v", r.StatusCode, out)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/teams", strings.NewReader(string(tb)))
	req.Header.Set("Authorization", "HMAC Zm9v")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Fatalf("bad teams signature = %d", r.StatusCode)
	}
}
