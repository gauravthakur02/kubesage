package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/config"
)

func TestRunbooksMatchAndInject(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "crashloop.md"), []byte("---\ntitle: Pod crash loops\nmatch: [CrashLoopBackOff, BackOff]\n---\n# Crash loops\n1. Check previous logs\n2. Page the owner\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "OOMKilled.md"), []byte("# Out of memory\nRaise the limit.\n"), 0o644)
	a, _ := newShopApp(t, "pods", false)
	a.cfg.Runbooks.Dir = dir
	p, n := a.withRunbooks("pod web is in CrashLoopBackOff with BackOff events")
	if n != 1 || !strings.Contains(p, "Runbook: Pod crash loops") || !strings.Contains(p, "Page the owner") || strings.Contains(p, "Out of memory") {
		t.Fatalf("n=%d prompt:\n%s", n, p)
	}
	run(a, a.runCommand("runbooks"), 0)
	mustContain(t, screen(a), "Runbooks", "Pod crash loops", "Out of memory", "OOMKilled")
	a.cursor = 0
	for i, idx := range a.visible {
		if strings.Contains(a.table.Rows[idx].Name, "crashloop") {
			a.cursor = i
		}
	}
	press(a, "enter")
	mustContain(t, screen(a), "runbook · Pod crash loops", "Check previous logs")
}

func TestStaleStartupScalingScreens(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	run(a, a.runCommand("restart-needed"), 0)
	mustContain(t, screen(a), "Restart needed")
	run(a, a.runCommand("autoscaling"), 0)
	mustContain(t, screen(a), "Autoscaling", "0 autoscalers")
	run(a, a.runCommand("pods"), 0)
	press(a, "W")
	mustContain(t, screen(a), "startup of pod", "Where the time went", "Probes and advice")
}

func TestSecretsHealthAndMesh(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	run(a, a.runCommand("secrets-health"), 0)
	v := screen(a)
	mustContain(t, v, "Secrets health", "shop-tls", "TLS (see :certs")
	if strings.Contains(v, "PRIVATE-KEY") {
		t.Fatal("secret values leaked")
	}
	run(a, a.runCommand("mesh"), 0)
	mustContain(t, screen(a), "Service mesh", "shop")
}

func TestLogsJSONFilterAndSave(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	a.mode, a.textKind = modeText, "logs"
	a.logSpec = logSpec{ns: "shop", pods: []string{"web-1"}}
	a.textLines = []string{`{"level":"error","ts":"2026-11-16T10:00:00Z","msg":"db down","retry":3}`, `{"level":"info","msg":"healthy"}`, "plain debug line"}
	a.renderText(false)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("J")})
	v := ansi.Strip(a.vp.View())
	if !strings.Contains(v, "ERROR") || !strings.Contains(v, "db down") || !strings.Contains(v, "retry=3") {
		t.Fatalf("pretty JSON:\n%s", v)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	a.input.SetValue("healthy|debug")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	v = ansi.Strip(a.vp.View())
	if strings.Contains(v, "healthy") || strings.Contains(v, "plain debug") || !strings.Contains(v, "db down") {
		t.Fatalf("exclude filter:\n%s", v)
	}
	wd, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(wd)
	a.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !strings.Contains(a.status, "saved 1 lines") {
		t.Fatalf("save: %q", a.status)
	}
}

func TestChangeWindowsFreezeAndReason(t *testing.T) {
	cfg := &config.Config{ChangeWindows: []config.ChangeWindow{{Contexts: "(?i)prod", Days: "Mon-Fri", Hours: "08:00-18:00"}},
		Freezes: []config.Freeze{{Contexts: "staging", From: "2026-12-20", To: "2027-01-02", Reason: "holidays"}}}
	sat := time.Date(2026, 11, 14, 12, 0, 0, 0, time.Local)
	mon := time.Date(2026, 11, 16, 9, 0, 0, 0, time.Local)
	if m, _ := changePolicy(cfg, "aks-prod", sat); !strings.Contains(m, "outside the change window") {
		t.Errorf("saturday: %q", m)
	}
	if m, _ := changePolicy(cfg, "aks-prod", mon); m != "" {
		t.Errorf("monday 09:00 should be allowed: %q", m)
	}
	if m, _ := changePolicy(cfg, "staging", time.Date(2026, 12, 24, 10, 0, 0, 0, time.Local)); !strings.Contains(m, "holidays") {
		t.Errorf("freeze: %q", m)
	}
	if !windowAllows(config.ChangeWindow{Hours: "22:00-02:00"}, time.Date(2026, 11, 16, 1, 0, 0, 0, time.Local)) {
		t.Error("windows over midnight")
	}

	a, _ := newShopApp(t, "deploy", false)
	a.client.Context = "aks-prod"
	a.cfg.ChangeWindows = []config.ChangeWindow{{Contexts: "prod", Days: "Sun", Hours: "03:00-03:01"}}
	press(a, "S")
	if !strings.Contains(a.status, "⛔") {
		t.Fatalf("scale outside the window must be blocked: %q", a.status)
	}
	a.cfg.ChangeWindows = nil
	a.cfg.RequireReason = "prod"
	press(a, "R")
	if a.ov != ovConfirm {
		t.Fatal("restart asks first")
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if a.ov != ovInput || !strings.Contains(a.inputHint, "reason") {
		t.Fatalf("a reason must be asked: ov=%v hint=%q", a.ov, a.inputHint)
	}
	a.input.SetValue("JIRA-123 config reload")
	_, cmd := a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(a, cmd, 0)
	run(a, a.runCommand("audit"), 0)
	mustContain(t, screen(a), "reason: JIRA-123 config reload")
}

func TestSortColumnsKeysMouse(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	press(a, "(")
	mustContain(t, screen(a), "NAME ▲")
	press(a, ")")
	mustContain(t, screen(a), "NAME ▼")
	if a.table.Rows[a.visible[0]].Name != "web-7d9-bbbbb" {
		t.Errorf("descending by name: first=%s", a.table.Rows[a.visible[0]].Name)
	}
	if v, ok := sortValue("2d16h"); !ok || v != 2*86400+16*3600 {
		t.Errorf("age %v", v)
	}
	if v, _ := sortValue("1.5Gi"); v != 1.5*(1<<30) {
		t.Errorf("qty %v", v)
	}

	a.cfg.HideColumns = map[string][]string{"pods": {"IP", "NODE"}}
	a.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	v := screen(a)
	if strings.Contains(v, " IP ") || strings.Contains(v, "NODE") {
		t.Fatalf("hidden columns still shown:\n%s", v)
	}

	a.cfg.Keys = map[string]string{"w": "g"}
	a.cursor = 0
	press(a, "g")
	mustContain(t, screen(a), "relationships")
	press(a, "esc")

	a.cfg.Keys = nil
	a.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	run(a, a.refresh(), 0)
	a.cursor = 0
	a.Update(tea.MouseMsg{X: 10, Y: 2 + 1 + 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if a.cursor != 1 {
		t.Fatalf("click on the second row should select it, cursor=%d", a.cursor)
	}
	a.Update(tea.MouseMsg{X: 10, Y: 10, Button: tea.MouseButtonWheelUp})
	if a.cursor != 0 {
		t.Fatalf("wheel up should move up, cursor=%d", a.cursor)
	}
}

func TestKubeconfigScreenAndFwdProfiles(t *testing.T) {
	a, _ := newShopApp(t, "pods", false)
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	os.WriteFile(kc, []byte("apiVersion: v1\nkind: Config\ncurrent-context: dev\nclusters:\n- name: c\n  cluster: {server: https://dev.example}\nusers:\n- name: u\n  user: {token: \"t\"}\ncontexts:\n- name: dev\n  context: {cluster: c, user: u}\n- name: old\n  context: {cluster: c, user: u}\n"), 0o600)
	a.client.Kubeconfig = kc
	run(a, a.runCommand("kubeconfig"), 0)
	mustContain(t, screen(a), "Kubeconfig", "dev", "old", "https://dev.example")
	run(a, a.runCommand("fwd save x"), 0)
	if !strings.Contains(a.status, "no active port-forwards") {
		t.Fatalf("fwd save without forwards: %q", a.status)
	}
	_ = metav1.Now
}
