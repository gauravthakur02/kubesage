package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/kube"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func twoContainerPod() corev1.Pod {
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"}, Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "migrate"}},
		Containers:     []corev1.Container{{Name: "api"}, {Name: "envoy"}},
	}}
}

func captureShell(t *testing.T) *[]*shellExec {
	t.Helper()
	var got []*shellExec
	execShellFn = func(s *shellExec, done func(error) tea.Msg) tea.Cmd {
		got = append(got, s)
		return nil
	}
	t.Cleanup(func() {
		execShellFn = func(s *shellExec, done func(error) tea.Msg) tea.Cmd { return tea.Exec(s, done) }
	})
	return &got
}

func TestShellPicksRunningContainerAndBuildsSession(t *testing.T) {
	got := captureShell(t)
	a := newTestApp(t)
	a.client.Context = "aks-staging"
	a.Update(podsMsg{purpose: "shell", pods: []corev1.Pod{twoContainerPod()}})
	if a.ov != ovPicker || len(a.picker.items) != 2 || strings.Contains(strings.Join(a.picker.items, ","), "migrate") {
		t.Fatalf("container picker should list only running containers: %v", a.picker.items)
	}
	a.picker.cursor = 1
	allowExec(a, true)
	press(a, "enter")
	if len(*got) != 1 {
		t.Fatal("shell session should start")
	}
	s := (*got)[0]
	if s.ns != "shop" || s.pod != "api-1" || s.container != "envoy" || s.command[0] != "sh" || !strings.Contains(s.command[2], "bash") {
		t.Fatalf("unexpected session %+v", s)
	}
	if !strings.Contains(s.banner, "shell in") || !strings.Contains(s.banner, "aks-staging") || !strings.Contains(s.banner, "exit") {
		t.Fatalf("banner should say where you are and how to leave: %q", s.banner)
	}
}

func TestShellOnProdAsksFirst(t *testing.T) {
	got := captureShell(t)
	a := newTestApp(t) // aks-prod
	pod := twoContainerPod()
	pod.Spec.Containers = pod.Spec.Containers[:1]
	allowExec(a, true)
	_, cmd := a.Update(podsMsg{purpose: "shell", pods: []corev1.Pod{pod}})
	run(a, cmd, 0)
	if a.ov != ovConfirm || len(*got) != 0 {
		t.Fatal("a shell on a production-like context needs confirmation")
	}
	press(a, "y")
	if len(*got) != 1 {
		t.Fatal("confirming should open the shell")
	}
}

func TestShellDoneOutcomes(t *testing.T) {
	a := newTestApp(t)
	a.client.Context = "dev"
	a.Update(shellDoneMsg{ns: "shop", pod: "api-1", container: "api"})
	if !strings.Contains(a.status, "closed") || a.statusErr {
		t.Fatalf("clean exit: %q", a.status)
	}
	a.Update(shellDoneMsg{ns: "shop", pod: "api-1", container: "api", err: errors.New("command terminated with non-zero exit code: exit code 130")})
	if a.statusErr || !strings.Contains(a.status, "closed") {
		t.Fatalf("non-zero exit of the shell is a normal close: %q", a.status)
	}
	a.Update(shellDoneMsg{ns: "shop", pod: "api-1", container: "api", err: errors.New(`exec: "sh": executable file not found in $PATH`)})
	if a.ov != ovConfirm || !strings.Contains(a.confirm, "ephemeral debug container") {
		t.Fatalf("distroless images should offer a debug container: ov=%v %q", a.ov, a.confirm)
	}
	press(a, "n")
	a.opts.ReadOnly = true
	a.Update(shellDoneMsg{ns: "shop", pod: "api-1", container: "api", err: errors.New(`exec: "sh": executable file not found in $PATH`)})
	if a.ov == ovConfirm || !strings.Contains(a.status, "read-only") {
		t.Fatalf("read-only mode must not offer the debug container: %q", a.status)
	}
	a.Update(shellDoneMsg{ns: "shop", pod: "api-1", container: "api", err: errors.New(`pods "api-1" is forbidden: cannot create resource "pods/exec"`)})
	if !strings.Contains(a.status, "not allowed") {
		t.Fatalf("forbidden: %q", a.status)
	}
}

func TestShellFromHealthView(t *testing.T) {
	a := newTestApp(t)
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	press(a, "h")
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m := cmd()
	if pm, ok := m.(podsMsg); !ok || pm.purpose != "shell" {
		t.Fatalf("s in the health view should start the shell flow, got %#v", m)
	}
}

func TestSizeQueueStops(t *testing.T) {
	q := newSizeQueue(-1) // not a terminal: no sizes
	done := make(chan *struct{})
	go func() { q.Next(); close(done) }()
	q.stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Next must return after stop")
	}
	if !kube.NoShell(errors.New("OCI runtime exec failed: exec failed: unable to start container process: exec: \"sh\": executable file not found in $PATH")) {
		t.Fatal("NoShell should detect a missing shell")
	}
	if kube.NoShell(errors.New("command terminated with non-zero exit code: exit code 127")) {
		t.Fatal("a shell exiting 127 is not a missing shell")
	}
}

// allowExec makes the fake API server answer SelfSubjectAccessReviews.
func allowExec(a *App, allowed bool) {
	a.client.Clientset.(*fake.Clientset).PrependReactor("create", "selfsubjectaccessreviews", func(act k8stesting.Action) (bool, runtime.Object, error) {
		r := act.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
		r.Status.Allowed = allowed
		return true, r, nil
	})
}

func TestShellForbiddenIsReportedWithoutTakingOverTheScreen(t *testing.T) {
	got := captureShell(t)
	a := newTestApp(t)
	a.client.Context = "aks-staging"
	allowExec(a, false)
	pod := twoContainerPod()
	pod.Spec.Containers = pod.Spec.Containers[:1]
	_, cmd := a.Update(podsMsg{purpose: "shell", pods: []corev1.Pod{pod}})
	run(a, cmd, 0)
	if len(*got) != 0 || !strings.Contains(a.status, "may not open shells in namespace shop") {
		t.Fatalf("forbidden exec must be explained up front: started=%d status=%q", len(*got), a.status)
	}
}
