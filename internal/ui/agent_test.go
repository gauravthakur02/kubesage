package ui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/gauravthakur02/kubeweave/internal/agent"
	"github.com/gauravthakur02/kubeweave/internal/ai"
)

// scriptedAI replies with the next scripted message on each call and records what it saw.
type scriptedAI struct {
	mu      sync.Mutex
	replies []string
	seen    [][]ai.Message
	system  string
}

func (s *scriptedAI) Name() string  { return "scripted" }
func (s *scriptedAI) Model() string { return "s-1" }
func (s *scriptedAI) Stream(_ context.Context, system string, msgs []ai.Message, out chan<- string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.system = system
	s.seen = append(s.seen, msgs)
	reply := "done"
	if len(s.replies) > 0 {
		reply, s.replies = s.replies[0], s.replies[1:]
	}
	out <- reply
	return nil
}

func TestAIRunsReadCommandsAndAsksForWrites(t *testing.T) {
	a := newTestApp(t)
	a.client.Context = "aks-staging" // not production-like
	var ran []string
	a.execFn = func(c agent.Command) (string, error) {
		ran = append(ran, c.Display())
		return "NAME    READY\napi-1   0/1\n", nil
	}
	sc := &scriptedAI{replies: []string{
		"Checking.\n```run\nkubectl get pods -n shop\n```",
		"Restart it:\n```run\nkubectl rollout restart deploy/api -n shop\nkubectl get pods -n shop | grep api\n```",
		"All good now.",
	}}
	a.provider = sc
	press(a, "A")
	run(a, a.sendToAI("why is api down?", "why is api down?"), 0)

	// Step 1 ran the read-only command automatically; step 2 now waits for approval.
	if len(ran) != 1 || ran[0] != "kubectl get pods -n shop" {
		t.Fatalf("ran = %v", ran)
	}
	if a.ov != ovApprove {
		t.Fatalf("expected approval overlay, ov=%v", a.ov)
	}
	if v := ansi.Strip(a.View()); !strings.Contains(v, "kubectl rollout restart deploy/api -n shop") || !strings.Contains(v, "changes cluster") {
		t.Fatalf("approval view:\n%s", v)
	}
	press(a, "y")
	if len(ran) != 2 || ran[1] != "kubectl rollout restart deploy/api -n shop" {
		t.Fatalf("approved command not run: %v", ran)
	}
	// The piped command was blocked, the results went back, and the AI concluded.
	last := sc.seen[len(sc.seen)-1]
	feedback := last[len(last)-1].Content
	if !strings.Contains(feedback, "not executed: shell operators") || !strings.Contains(feedback, "api-1   0/1") {
		t.Fatalf("feedback to model:\n%s", feedback)
	}
	if a.chat[len(a.chat)-1].content != "All good now." || a.aiBusy {
		t.Fatalf("loop did not finish: %+v", a.chat[len(a.chat)-1])
	}
	if !strings.Contains(sc.system, "```run") {
		t.Fatal("system prompt should teach the run protocol")
	}
}

func TestDeclineAndProdTyping(t *testing.T) {
	a := newTestApp(t) // context "aks-prod" is production-like
	var ran []string
	a.execFn = func(c agent.Command) (string, error) { ran = append(ran, c.Display()); return "ok", nil }
	sc := &scriptedAI{replies: []string{"```run\nkubectl delete pod api-1 -n shop\nkubectl scale deploy api --replicas=0 -n shop\n```", "understood"}}
	a.provider = sc
	press(a, "A")
	run(a, a.sendToAI("fix it", "fix it"), 0)
	press(a, "y") // on prod, y asks for typing "approve"
	if !a.ag.typing || len(ran) != 0 {
		t.Fatalf("prod should require typing approve, ran=%v", ran)
	}
	press(a, "nope")
	press(a, "enter")
	if len(ran) != 0 || a.ov != ovApprove {
		t.Fatal("wrong confirmation text must not run")
	}
	press(a, "esc") // leave typing mode
	press(a, "n")   // skip delete
	press(a, "esc") // stop batch (scale declined too)
	if len(ran) != 0 {
		t.Fatalf("nothing should have run: %v", ran)
	}
	fb := sc.seen[len(sc.seen)-1]
	if c := fb[len(fb)-1].Content; strings.Count(c, "not executed") != 2 {
		t.Fatalf("both commands should be reported as not executed:\n%s", c)
	}
}

func TestReadOnlyRefusesAIWrites(t *testing.T) {
	a := newTestApp(t)
	a.opts.ReadOnly = true
	a.execFn = func(c agent.Command) (string, error) { t.Fatalf("should not run %s", c.Display()); return "", nil }
	a.provider = &scriptedAI{replies: []string{"```run\nkubectl delete pod api-1 -n shop\n```", "ok"}}
	press(a, "A")
	run(a, a.sendToAI("x", "x"), 0)
	if a.ov == ovApprove {
		t.Fatal("read-only mode should not even ask")
	}
}

func TestStepLimit(t *testing.T) {
	a := newTestApp(t)
	a.client.Context = "dev"
	a.cfg.AI.MaxSteps = 2
	n := 0
	a.execFn = func(agent.Command) (string, error) { n++; return "x", nil }
	loop := "```run\nkubectl get pods\n```"
	a.provider = &scriptedAI{replies: []string{loop, loop, loop, loop}}
	press(a, "A")
	run(a, a.sendToAI("x", "x"), 0)
	if n != 2 || !strings.Contains(a.chat[len(a.chat)-1].display, "step limit") {
		t.Fatalf("expected 2 runs then step limit, got %d, last=%+v", n, a.chat[len(a.chat)-1])
	}
}

func TestPickSuggestedAndManual(t *testing.T) {
	a := newTestApp(t)
	a.client.Context = "dev"
	var ran []string
	a.execFn = func(c agent.Command) (string, error) { ran = append(ran, c.Display()); return "out", nil }
	a.provider = &scriptedAI{replies: []string{"Try:\n```bash\nkubectl describe pod api-1 -n shop\n```", "thanks"}}
	press(a, "A")
	run(a, a.sendToAI("x", "x"), 0)
	if len(ran) != 0 {
		t.Fatal("bash blocks must not auto-run")
	}
	a.pickSuggested()
	if a.ov != ovPicker || !strings.Contains(a.picker.items[0], "[read] kubectl describe pod api-1") {
		t.Fatalf("picker: %+v", a.picker.items)
	}
	press(a, "enter")
	if len(ran) != 1 {
		t.Fatalf("picked command should run: %v", ran)
	}

	// "!" from the table runs a typed command and shows output.
	a.mode = modeTable
	a.ov = ovNone
	cmd := a.runManual("get nodes")
	run(a, cmd, 0)
	if ran[len(ran)-1] != "kubectl get nodes" || a.mode != modeText || a.textLines[0] != "out" {
		t.Fatalf("manual run: ran=%v mode=%v lines=%v", ran, a.mode, a.textLines)
	}
}

func TestParsePorts(t *testing.T) {
	cases := map[string][3]int{"8080:80": {8080, 80, 0}, "9000": {9000, 9000, 0}, ":443": {0, 443, 0}, "": {0, 5432, 0}}
	for in, want := range cases {
		l, r, err := parsePorts(in, 5432)
		if err != nil || l != want[0] || r != want[1] {
			t.Errorf("parsePorts(%q) = %d,%d,%v", in, l, r, err)
		}
	}
	if _, _, err := parsePorts("abc:80", 0); err == nil {
		t.Error("expected error")
	}
}
