package ui

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"kubesage/internal/ai"
	"kubesage/internal/kube"
)

type fakeAI struct{}

func (fakeAI) Name() string  { return "fake" }
func (fakeAI) Model() string { return "fake-1" }
func (fakeAI) Stream(ctx context.Context, _ string, msgs []ai.Message, out chan<- string) error {
	for _, s := range []string{"## Verdict\n", "Pod is **crashing**. Run `kubectl logs -p`.\n", "```\nkubectl rollout undo\n```"} {
		out <- s
	}
	return nil
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "shop"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "api"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cfg := testConfig(t)
	client := &kube.Client{Clientset: fake.NewSimpleClientset(pod), Namespace: "shop", Context: "aks-prod"}
	a := New(cfg, client, Options{})
	a.provider = fakeAI{}
	a.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return a
}

// run executes a command chain synchronously (skipping ticks), feeding results back.
func run(a *App, cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 20 {
		return
	}
	msg := cmd()
	switch m := msg.(type) {
	case tea.BatchMsg:
		for _, c := range m {
			run(a, c, depth+1)
		}
		return
	case tickMsg, nil:
		return
	}
	if _, ok := msg.(interface{ Tick() }); ok {
		return
	}
	_, next := a.Update(msg)
	run(a, next, depth+1)
}

func press(a *App, k string) {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	_, cmd := a.Update(msg)
	run(a, cmd, 0)
}

func TestTableRendersAndFilters(t *testing.T) {
	a := newTestApp(t)
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	view := ansi.Strip(a.View())
	for _, want := range []string{"kubesage", "ctx aks-prod", "⚠ PROD", "api-1", "Running", "Pods (1)"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q\n%s", want, view)
		}
	}
	press(a, "/")
	press(a, "zzz")
	if len(a.visible) != 0 {
		t.Fatalf("filter should hide rows")
	}
	press(a, "esc")
	if len(a.visible) != 1 {
		t.Fatalf("esc should clear filter")
	}
	for _, line := range strings.Split(a.View(), "\n") {
		if w := ansi.StringWidth(line); w > 120 {
			t.Fatalf("line wider than terminal (%d): %q", w, ansi.Strip(line))
		}
	}
}

func TestDangerContextRequiresTypedName(t *testing.T) {
	a := newTestApp(t)
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	press(a, "D")
	if a.ov != ovConfirm || a.inputHint != "api-1" {
		t.Fatalf("expected type-to-confirm overlay, got ov=%v hint=%q", a.ov, a.inputHint)
	}
	press(a, "y") // a stray "y" must not delete on prod contexts
	press(a, "enter")
	if a.ov != ovNone || !a.statusErr {
		t.Fatalf("mismatched name should cancel: status=%q", a.status)
	}
}

func TestReadOnlyBlocksWrites(t *testing.T) {
	a := newTestApp(t)
	a.opts.ReadOnly = true
	run(a, fetchList(a.client, a.res, a.listGen), 0)
	press(a, "D")
	if a.ov != ovNone || !strings.Contains(a.status, "read-only") {
		t.Fatalf("read-only should block delete, status=%q", a.status)
	}
}

func TestAIChatStreams(t *testing.T) {
	a := newTestApp(t)
	press(a, "A")
	if a.mode != modeAI {
		t.Fatal("A should open AI mode")
	}
	cmd := a.sendToAI("why?", "why?")
	run(a, cmd, 0)
	if a.aiBusy {
		t.Fatal("stream should be finished")
	}
	last := a.chat[len(a.chat)-1]
	if last.role != "assistant" || !strings.Contains(last.content, "crashing") {
		t.Fatalf("unexpected chat %+v", a.chat)
	}
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "Verdict") || !strings.Contains(view, "kubectl rollout undo") {
		t.Fatalf("markdown not rendered:\n%s", view)
	}
}

func TestPulseView(t *testing.T) {
	a := newTestApp(t)
	press(a, "H")
	view := ansi.Strip(a.View())
	if !strings.Contains(view, "Nodes") || !strings.Contains(view, "Detected issues") {
		t.Fatalf("pulse not rendered:\n%s", view)
	}
}

func TestCommands(t *testing.T) {
	a := newTestApp(t)
	run(a, a.runCommand("deploy"), 0)
	if a.res.Name != "deployments" {
		t.Fatalf("got %s", a.res.Name)
	}
	run(a, a.runCommand("pods all"), 0)
	if a.client.Namespace != kube.AllNamespaces || a.res.Name != "pods" {
		t.Fatalf("pods all failed: ns=%q res=%s", a.client.Namespace, a.res.Name)
	}
	a.runCommand("theme dracula")
	if a.st.T.Name != "dracula" {
		t.Fatal("theme not applied")
	}
	for _, name := range ThemeNames() {
		a.applyTheme(name)
		_ = a.View()
	}
}

func TestLogWrapToggle(t *testing.T) {
	a := newTestApp(t)
	a.mode, a.textKind = modeText, "logs"
	a.textLines = []string{strings.Repeat("x", 300)}
	a.renderText(true)
	if n := len(strings.Split(a.vp.View(), "\n")); a.vp.TotalLineCount() < 2 {
		t.Fatalf("long line should wrap by default (lines=%d)", n)
	}
	press(a, "w")
	if a.vp.TotalLineCount() != 1 {
		t.Fatalf("w should turn wrapping off, got %d lines", a.vp.TotalLineCount())
	}
}

func TestInAppKeyAndProfileWizard(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "") // the developer's own key must not satisfy the new profile
	a := newTestApp(t)
	run(a, a.runCommand("ai-add"), 0)
	if a.ov != ovPicker {
		t.Fatal(":ai-add should start with the provider picker")
	}
	a.picker.cursor = pickerIndex(t, a, "anthropic")
	press(a, "enter")
	a.input.SetValue("claude-fast") // name
	press(a, "enter")
	a.input.SetValue("claude-haiku-5-5") // model
	press(a, "enter")
	// No key known for the new profile → the hidden key prompt follows.
	if a.ov != ovInput || a.input.EchoMode != textinput.EchoPassword {
		t.Fatalf("expected a hidden key prompt, ov=%v echo=%v", a.ov, a.input.EchoMode)
	}
	a.input.SetValue("sk-ant-test-123")
	if strings.Contains(ansi.Strip(a.View()), "sk-ant-test-123") {
		t.Fatal("the key must never be visible on screen")
	}
	press(a, "enter")
	if a.input.Value() != "" || a.input.EchoMode != textinput.EchoNormal {
		t.Fatal("the input must be cleared after reading the key")
	}
	p := a.cfg.AI.Profiles["claude-fast"]
	if a.profile != "claude-fast" || !p.KeyInKeyring || p.Key() != "sk-ant-test-123" || p.Model != "claude-haiku-5-5" {
		t.Fatalf("profile not configured: active=%s %+v", a.profile, p)
	}
	data, _ := os.ReadFile(a.cfg.Path())
	if strings.Contains(string(data), "sk-ant-test-123") || !strings.Contains(string(data), "claude-fast") {
		t.Fatalf("config must list the profile but not the key:\n%s", data)
	}
	if !strings.Contains(a.status, "stored in") {
		t.Fatalf("status = %q", a.status)
	}

	// :ai-key replaces the key; ctrl+o lists add/set-key actions.
	run(a, a.runCommand("ai-key claude-fast"), 0)
	a.input.SetValue("sk-ant-new")
	press(a, "enter")
	if a.cfg.AI.Profiles["claude-fast"].Key() != "sk-ant-new" {
		t.Fatal(":ai-key should update the key")
	}
	a.pickProfile()
	if pickerIndex(t, a, "+ Add AI profile") < 0 || pickerIndex(t, a, "⚿ Set API key") < 0 {
		t.Fatal("profile picker should offer add and set-key")
	}
	press(a, "esc")
	run(a, a.runCommand("ai-rm claude-fast"), 0)
	press(a, "y")
	if _, ok := a.cfg.AI.Profiles["claude-fast"]; ok {
		t.Fatal(":ai-rm should remove the profile")
	}
}

func TestPlural(t *testing.T) {
	for in, want := range map[string]string{"Pod": "Pods", "NetworkPolicy": "NetworkPolicies", "Ingress": "Ingresses",
		"Endpoints": "Endpoints", "StorageClass": "StorageClasses", "Gateway": "Gateways", "DaemonSet": "DaemonSets"} {
		if got := plural(in); got != want {
			t.Errorf("plural(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestLogoAnimates(t *testing.T) {
	lipgloss.SetColorProfile(termenv.TrueColor) // the rotation is carried by colour
	defer lipgloss.SetColorProfile(termenv.Ascii)
	st := newStyles(themeByName("tokyonight"))
	seen := map[string]bool{}
	for n := 0; n < 16; n++ {
		lines := LogoFrame(st, "x", n)
		if len(lines) != 5 {
			t.Fatal("every frame must have 5 lines")
		}
		seen[strings.Join(lines, "\n")] = true
		// Only colours and the sparkle change, never the shape, so the logo cannot jitter.
		for i, l := range lines[1:] {
			if ansi.Strip(l) != ansi.Strip(Logo(st, "x")[i+1]) {
				t.Fatalf("frame %d line %d changed shape", n, i+1)
			}
		}
	}
	if len(seen) < 8 {
		t.Fatalf("expected at least 8 distinct frames, got %d", len(seen))
	}
	a := newTestApp(t)
	a.cfg.Animations = false
	if a.logoFrame() != -1 {
		t.Fatal("animations: false must render the static logo")
	}
}
