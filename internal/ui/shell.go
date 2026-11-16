package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// shellExec implements tea.ExecCommand: Bubble Tea hands over the terminal, we run an
// interactive TTY session in the container, then the IDE comes back exactly as it was.
type shellExec struct {
	client             *kube.Client
	ns, pod, container string
	command            []string
	stdin              io.Reader
	stdout, stderr     io.Writer
	banner             string
}

func (s *shellExec) SetStdin(r io.Reader)  { s.stdin = r }
func (s *shellExec) SetStdout(w io.Writer) { s.stdout = w }
func (s *shellExec) SetStderr(w io.Writer) { s.stderr = w }

func (s *shellExec) Run() error {
	fmt.Fprint(s.stdout, s.banner)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		if old, err := term.MakeRaw(fd); err == nil {
			defer term.Restore(fd, old)
		}
	}
	q := newSizeQueue(int(os.Stdout.Fd()))
	defer q.stop()
	err := s.client.Exec(context.Background(), kube.ExecOptions{
		Namespace: s.ns, Pod: s.pod, Container: s.container, Command: s.command, TTY: true,
		Stdin: s.stdin, Stdout: s.stdout, SizeQueue: q,
	})
	return err
}

// sizeQueue reports terminal size changes to the container (polling works on every OS, incl. Windows).
type sizeQueue struct {
	ch   chan *remotecommand.TerminalSize
	done chan struct{}
	once sync.Once
}

func newSizeQueue(fd int) *sizeQueue {
	q := &sizeQueue{ch: make(chan *remotecommand.TerminalSize, 1), done: make(chan struct{})}
	go func() {
		var lw, lh int
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			if w, h, err := term.GetSize(fd); err == nil && (w != lw || h != lh) {
				lw, lh = w, h
				select {
				case q.ch <- &remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
				default:
				}
			}
			select {
			case <-q.done:
				return
			case <-t.C:
			}
		}
	}()
	return q
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case s := <-q.ch:
		return s
	case <-q.done:
		return nil
	}
}

func (q *sizeQueue) stop() { q.once.Do(func() { close(q.done) }) }

// shellDoneMsg is sent when the shell session ends.
type shellDoneMsg struct {
	ns, pod, container string
	err                error
}

// execShellFn is swapped in tests (no real terminal there).
var execShellFn = func(s *shellExec, done func(error) tea.Msg) tea.Cmd { return tea.Exec(s, done) }

// shellAllowedMsg carries the RBAC pre-check result for opening a shell.
type shellAllowedMsg struct {
	ns, pod, container string
	allowed            bool
}

// openShell checks RBAC first (so a forbidden shell never takes over the screen), then starts it.
func (a *App) openShell(ns, pod, container string) tea.Cmd {
	c := a.client
	a.setStatus(fmt.Sprintf("checking permission to exec into %s/%s…", ns, pod), false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ok, _, err := c.CanI(ctx, "create", "pods", "exec", ns)
		if err != nil {
			ok = true // cannot tell (e.g. old API server): just try, errors are reported anyway
		}
		return shellAllowedMsg{ns: ns, pod: pod, container: container, allowed: ok}
	}
}

func (a *App) handleShellAllowed(m shellAllowedMsg) tea.Cmd {
	if !m.allowed {
		a.setStatus(fmt.Sprintf("your account may not open shells in namespace %s on %s (needs create on pods/exec) — ask your cluster admin",
			m.ns, a.client.Context), true)
		return nil
	}
	return a.startShell(m.ns, m.pod, m.container)
}

// startShell opens the interactive session (asks first on production-like contexts).
func (a *App) startShell(ns, pod, container string) tea.Cmd {
	start := func() tea.Cmd {
		s := &shellExec{client: a.client, ns: ns, pod: pod, container: container, command: kube.ShellCommand,
			banner: fmt.Sprintf("\r\n\x1b[1;36m⎈ kubesage\x1b[0m shell in \x1b[1m%s/%s\x1b[0m [%s] on %s · type \x1b[1mexit\x1b[0m or Ctrl-D to return to the IDE\r\n\r\n",
				ns, pod, container, a.client.Context)}
		a.setStatus(fmt.Sprintf("opening shell in %s/%s [%s]…", ns, pod, container), false)
		return execShellFn(s, func(err error) tea.Msg { return shellDoneMsg{ns: ns, pod: pod, container: container, err: err} })
	}
	if a.danger() {
		a.askConfirm(fmt.Sprintf("Open a shell in %s/%s [%s] on %s?", ns, pod, container, a.client.Context), "", start)
		return nil
	}
	return start()
}

func (a *App) handleShellDone(m shellDoneMsg) tea.Cmd {
	switch {
	case m.err == nil:
		a.setStatus(fmt.Sprintf("shell in %s/%s [%s] closed", m.ns, m.pod, m.container), false)
	case kube.NoShell(m.err):
		a.setStatus(fmt.Sprintf("container %s has no shell (distroless image?)", m.container), true)
		if a.readOnly() {
			a.setStatus(fmt.Sprintf("container %s has no shell; a debug container needs write access (read-only mode)", m.container), true)
			return nil
		}
		target := m
		a.askConfirm(fmt.Sprintf("%s has no shell. Start an ephemeral debug container (%s) sharing its process namespace? This permanently adds an ephemeral container to pod %s.",
			m.container, a.debugImage(), m.pod), "", func() tea.Cmd { return a.debugShell(target.ns, target.pod, target.container) })
	case strings.Contains(m.err.Error(), "exit code"):
		// The shell itself exited non-zero (e.g. the last command failed before `exit`): a normal close.
		a.setStatus(fmt.Sprintf("shell in %s/%s [%s] closed (%s)", m.ns, m.pod, m.container, m.err.Error()), false)
	case strings.Contains(strings.ToLower(m.err.Error()), "forbidden"):
		a.setStatus("not allowed to exec into "+m.pod+": "+m.err.Error(), true)
	default:
		a.setStatus("shell ended: "+m.err.Error(), true)
	}
	return a.refresh()
}

// debugShell attaches an ephemeral busybox container that targets the container's processes.
func (a *App) debugShell(ns, pod, container string) tea.Cmd {
	return kubectlExec(a.client, "debug", "-it", pod, "-n", ns, "--image="+a.debugImage(), "--target="+container, "--profile=general", "--", "sh")
}

// debugImage is the configured debug container image (busybox by default).
func (a *App) debugImage() string {
	if a.cfg.DebugImage != "" {
		return a.cfg.DebugImage
	}
	return "busybox:1.36"
}
