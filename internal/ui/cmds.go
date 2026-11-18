package ui

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/ai"
	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// ---------- messages ----------

type tickMsg time.Time
type listMsg struct {
	pane  *pane
	gen   int
	table kube.Table
	err   error
}
type versionMsg struct {
	ctx, v string
	err    error
}
type textMsg struct {
	pane                 *pane
	title, kind, content string
	err                  error
}
type logLinesMsg struct {
	pane  *pane
	gen   int
	lines []string
	done  bool
}
type aiChunkMsg struct {
	gen  int
	text string
	done bool
	err  error
}
type aiPromptMsg struct {
	display, prompt string
	err             error
}
type pulseMsg struct {
	pane   *pane
	pulse  kube.Pulse
	issues []kube.Issue
	err    error
}
type actionMsg struct {
	status string
	err    error
}
type clientMsg struct {
	client *kube.Client
	err    error
}
type namespacesMsg struct {
	names []string
	err   error
}
type podsMsg struct {
	purpose string
	pods    []corev1.Pod
	err     error
}
type execDoneMsg struct{ err error }

const timeout = 20 * time.Second

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---------- kubernetes commands ----------

func fetchList(c *kube.Client, r kube.Resource, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		t, err := r.List(ctx, c)
		return listMsg{gen: gen, table: t, err: err}
	}
}

func fetchVersion(c *kube.Client) tea.Cmd {
	return func() tea.Msg {
		v, err := c.ServerVersion()
		return versionMsg{ctx: c.Context, v: v, err: err}
	}
}

func fetchText(c *kube.Client, r kube.Resource, ns, name, kind string, reveal bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		var out string
		var err error
		switch kind {
		case "yaml":
			out, err = c.YAML(ctx, r, ns, name, reveal)
		default:
			out, err = c.Describe(ctx, r, ns, name)
		}
		return textMsg{title: fmt.Sprintf("%s %s/%s", kind, strings.ToLower(r.Kind), name), kind: kind, content: out, err: err}
	}
}

func fetchPulse(c *kube.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		p, err := c.Pulse(ctx)
		if err != nil {
			return pulseMsg{err: err}
		}
		issues, err := c.Scan(ctx)
		return pulseMsg{pulse: p, issues: issues, err: err}
	}
}

func fetchNamespaces(c *kube.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		list, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return namespacesMsg{err: err}
		}
		names := []string{"all"}
		for _, n := range list.Items {
			names = append(names, n.Name)
		}
		return namespacesMsg{names: names}
	}
}

func fetchPods(c *kube.Client, r kube.Resource, ns, name, purpose string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		pods, err := c.PodsFor(ctx, r, ns, name)
		return podsMsg{purpose: purpose, pods: pods, err: err}
	}
}

func switchContext(kubeconfig, ctxName, ns string) tea.Cmd {
	return func() tea.Msg {
		c, err := kube.New(kubeconfig, ctxName, ns)
		return clientMsg{client: c, err: err}
	}
}

func runAction(desc string, fn func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		return actionMsg{status: desc, err: fn(ctx)}
	}
}

// WebMode is set by `kubeweave serve`: programs that take over the terminal (shells, $EDITOR, kubectl
// hand-offs, terminal plugins) would run on the server's terminal, so they are refused.
var WebMode bool

var errWeb = fmt.Errorf("not available in the web UI (it would open on the server's terminal); use kubeweave in a terminal")

func kubectlExec(c *kube.Client, args ...string) tea.Cmd {
	if WebMode {
		return func() tea.Msg { return execDoneMsg{err: errWeb} }
	}
	if !kube.HasKubectl() {
		return func() tea.Msg { return execDoneMsg{err: fmt.Errorf("kubectl not found on PATH")} }
	}
	cmd := exec.Command("kubectl", c.KubectlArgs(args...)...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return execDoneMsg{err: err} })
}

// ---------- streaming logs ----------

func startLogStream(c *kube.Client, ns, pod string, opts kube.LogOptions) (chan string, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan string, 2048)
	go func() {
		defer close(ch)
		rc, err := c.Logs(ctx, ns, pod, opts)
		if err != nil {
			ch <- "[kubeweave] error: " + err.Error()
			return
		}
		defer rc.Close()
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case ch <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil && ctx.Err() == nil {
			ch <- "[kubeweave] stream ended: " + err.Error()
		}
	}()
	return ch, cancel
}

// waitLines blocks for one line, then drains whatever else is buffered.
func waitLines(ch chan string, gen int) tea.Cmd {
	return func() tea.Msg {
		first, ok := <-ch
		if !ok {
			return logLinesMsg{gen: gen, done: true}
		}
		lines := []string{first}
		for len(lines) < 1000 {
			select {
			case l, ok := <-ch:
				if !ok {
					return logLinesMsg{gen: gen, lines: lines, done: true}
				}
				lines = append(lines, l)
			default:
				return logLinesMsg{gen: gen, lines: lines}
			}
		}
		return logLinesMsg{gen: gen, lines: lines}
	}
}

// ---------- AI ----------

func gatherForAI(c *kube.Client, r kube.Resource, ns, name string, redact bool, logLines int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		data, err := c.GatherContext(ctx, r, ns, name, redact, logLines)
		display := fmt.Sprintf("Analyze %s %s", strings.ToLower(r.Kind), objName(ns, name))
		data = fmt.Sprintf("Cluster (kube-context): %s\n\n%s", c.Context, data)
		return aiPromptMsg{display: display, prompt: ai.AnalyzePrompt(r.Kind, ns, name, data), err: err}
	}
}

func triageForAI(c *kube.Client, issues []kube.Issue) tea.Cmd {
	return func() tea.Msg {
		scope := fmt.Sprintf("context %q, namespace %q", c.Context, c.NamespaceLabel())
		if len(issues) == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
			defer cancel()
			var err error
			if issues, err = c.Scan(ctx); err != nil {
				return aiPromptMsg{err: err}
			}
		}
		if len(issues) == 0 {
			return aiPromptMsg{err: fmt.Errorf("no issues detected in %s 🎉", scope)}
		}
		return aiPromptMsg{
			display: fmt.Sprintf("Triage %d detected issues in %s", len(issues), scope),
			prompt:  ai.TriagePrompt(scope, kube.FormatIssues(issues)),
		}
	}
}

// chatPrelude adds cluster context to the first free-form question of a conversation.
func chatPrelude(c *kube.Client, question string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		issues, _ := c.Scan(ctx)
		var b strings.Builder
		fmt.Fprintf(&b, "Cluster context: kube-context=%q namespace=%q\n", c.Context, c.NamespaceLabel())
		if len(issues) > 0 {
			if len(issues) > 40 {
				issues = issues[:40]
			}
			fmt.Fprintf(&b, "Currently detected issues:\n%s\n", kube.FormatIssues(issues))
		} else {
			b.WriteString("No issues are currently detected by the rule engine.\n")
		}
		fmt.Fprintf(&b, "\nQuestion: %s", question)
		return aiPromptMsg{display: question, prompt: b.String()}
	}
}

func startAIStream(p ai.Provider, system string, msgs []ai.Message) (chan string, chan error, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string, 256)
	errc := make(chan error, 1)
	go func() {
		err := p.Stream(ctx, system, msgs, out)
		errc <- err
		close(out)
	}()
	return out, errc, cancel
}

func waitAI(out chan string, errc chan error, gen int) tea.Cmd {
	return func() tea.Msg {
		first, ok := <-out
		if !ok {
			return aiChunkMsg{gen: gen, done: true, err: <-errc}
		}
		var b strings.Builder
		b.WriteString(first)
		for {
			select {
			case s, ok := <-out:
				if !ok {
					return aiChunkMsg{gen: gen, text: b.String(), done: true, err: <-errc}
				}
				b.WriteString(s)
			default:
				return aiChunkMsg{gen: gen, text: b.String()}
			}
		}
	}
}

func objName(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}
