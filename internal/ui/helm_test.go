package ui

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubesage/internal/kube"
)

func helmSecret(t *testing.T, ns, name string, rev int, status, chartVer string, values map[string]any, manifest string) *corev1.Secret {
	t.Helper()
	rec := map[string]any{
		"name": name, "namespace": ns, "version": rev,
		"info":     map[string]any{"status": status, "last_deployed": "2026-11-01T10:00:00Z", "description": fmt.Sprintf("Upgrade %d", rev)},
		"chart":    map[string]any{"metadata": map[string]any{"name": name, "version": chartVer, "appVersion": "1." + chartVer}},
		"config":   values,
		"manifest": manifest,
	}
	js, _ := json.Marshal(rec)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(js)
	zw.Close()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, rev), Namespace: ns,
			Labels: map[string]string{"owner": "helm", "name": name, "version": fmt.Sprint(rev), "status": status}},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))},
	}
}

func newHelmApp(t *testing.T, readOnly bool) *App {
	t.Helper()
	cs := fake.NewSimpleClientset(
		helmSecret(t, "shop", "web", 1, "superseded", "0.1.0", map[string]any{"replicas": 1, "db": map[string]any{"password": "hunter2"}}, "kind: Deployment\nreplicas: 1\n"),
		helmSecret(t, "shop", "web", 2, "deployed", "0.2.0", map[string]any{"replicas": 3, "db": map[string]any{"password": "hunter2"}}, "kind: Deployment\nreplicas: 3\n"),
		helmSecret(t, "shop", "cache", 1, "failed", "1.0.0", nil, "kind: StatefulSet\n"),
	)
	a := New(testConfig(t), kube.NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"}), Options{ReadOnly: readOnly})
	a.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	run(a, a.runCommand("helm"), 0)
	return a
}

func TestHelmReleasesValuesAndHistory(t *testing.T) {
	a := newHelmApp(t, false)
	view := ansi.Strip(a.View())
	for _, want := range []string{"Helm releases", "2 releases", "web", "deployed", "web-0.2.0", "cache", "failed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("helm list missing %q:\n%s", want, view)
		}
	}
	// select web and open values: password masked
	for i, r := range a.table.Rows {
		if r.Name == "web" {
			a.cursor = i
		}
	}
	press(a, "enter")
	view = ansi.Strip(a.View())
	if !strings.Contains(view, "replicas: 3") || !strings.Contains(view, "<masked>") || strings.Contains(view, "hunter2") {
		t.Fatalf("values view should mask secrets:\n%s", view)
	}
	press(a, "esc")
	press(a, "h")
	view = ansi.Strip(a.View())
	if !strings.Contains(view, "Helm history shop/web") || !strings.Contains(view, "2 revisions") {
		t.Fatalf("history missing:\n%s", view)
	}
	// diff revision 1 against current
	a.cursor = 1
	press(a, "d")
	view = ansi.Strip(a.View())
	if !strings.Contains(view, "revision 1") || !strings.Contains(view, "replicas: 3") || strings.Contains(view, "hunter2") {
		t.Fatalf("diff view wrong:\n%s", view)
	}
}

func TestHelmRollbackBlockedReadOnly(t *testing.T) {
	a := newHelmApp(t, true)
	for i, r := range a.table.Rows {
		if r.Name == "web" {
			a.cursor = i
		}
	}
	press(a, "h")
	a.cursor = 1
	press(a, "U")
	if !strings.Contains(strings.ToLower(a.status), "read-only") {
		t.Fatalf("rollback must be blocked in read-only, status=%q", a.status)
	}
}

func TestDecodeHelmReleaseRejectsGarbage(t *testing.T) {
	if _, err := kube.DecodeHelmRelease([]byte("not-base64!!")); err == nil {
		t.Fatal("expected error")
	}
}
