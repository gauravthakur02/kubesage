package ui

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

type dirMsg struct {
	ns, pod, container, path string
	entries                  []kube.FileEntry
	err                      error
}

type fileMsg struct {
	pane                     *pane
	ns, pod, container, path string
	data                     []byte
	err                      error
}

func (m fileMsg) targetPane() *pane { return m.pane }

// firstContainer picks the pod's first app container (fetched quickly).
func (a *App) firstContainer(ctx context.Context, ns, pod string) string {
	p, err := a.client.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metaGet())
	if err != nil || len(p.Spec.Containers) == 0 {
		return ""
	}
	return p.Spec.Containers[0].Name
}

// browseFiles lists a directory inside the pod's first container (B on a pod, :files [path]).
func (a *App) browseFiles(ns, pod, container, dir string) tea.Cmd {
	c := a.client
	if dir == "" {
		dir = "/"
	}
	a.setStatus("listing "+dir+" in "+pod+"…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		ct := container
		if ct == "" {
			ct = a.firstContainer(ctx, ns, pod)
		}
		es, err := c.ListDir(ctx, ns, pod, ct, dir)
		return dirMsg{ns: ns, pod: pod, container: ct, path: dir, entries: es, err: err}
	}
}

func (a *App) handleDir(m dirMsg) tea.Cmd {
	if m.err != nil {
		msg := m.err.Error()
		if kube.NoShell(m.err) {
			msg = "this image has no ls (distroless?): use the debug shell instead"
		}
		a.setStatus("cannot list "+m.path+": "+msg, true)
		return nil
	}
	labels := []string{"📁 ..  (parent)"}
	for _, e := range m.entries {
		switch {
		case e.Dir:
			labels = append(labels, "📁 "+e.Name+"/")
		case e.Link:
			labels = append(labels, "🔗 "+e.Name+" → "+e.Target)
		default:
			labels = append(labels, fmt.Sprintf("📄 %s  %s", e.Name, a.st.Muted.Render(kube.FormatBytes(e.Size))))
		}
	}
	title := fmt.Sprintf("%s:%s (%s) · enter: open · type to filter · :download / :upload copy files", m.pod, m.path, m.container)
	a.openPicker(title, labels, func(s string) tea.Cmd {
		for i, l := range labels {
			if l != s {
				continue
			}
			if i == 0 {
				return a.browseFiles(m.ns, m.pod, m.container, path.Dir(strings.TrimSuffix(m.path, "/")))
			}
			e := m.entries[i-1]
			full := path.Join(m.path, e.Name)
			if e.Dir || (e.Link && !strings.Contains(path.Base(e.Target), ".")) {
				return a.browseFiles(m.ns, m.pod, m.container, full)
			}
			return a.viewFile(m.ns, m.pod, m.container, full)
		}
		return nil
	})
	a.files = &filesState{ns: m.ns, pod: m.pod, container: m.container, dir: m.path}
	return nil
}

func (a *App) viewFile(ns, pod, container, p string) tea.Cmd {
	c := a.client
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind, a.textTitle, a.textLines = modeText, "file", "reading "+p+"…", nil
	a.renderText(true)
	return tagPane(a.pane, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		data, err := c.ReadFile(ctx, ns, pod, container, p, 2<<20)
		return fileMsg{ns: ns, pod: pod, container: container, path: p, data: data, err: err}
	})
}

func (a *App) handleFile(m fileMsg) {
	if m.err != nil {
		a.textTitle, a.textLines = "✗ "+m.path, []string{a.st.ErrText.Render(m.err.Error())}
		a.renderText(true)
		return
	}
	a.files = &filesState{ns: m.ns, pod: m.pod, container: m.container, dir: path.Dir(m.path), file: m.path}
	if !utf8.Valid(m.data) || strings.ContainsRune(string(m.data[:min(len(m.data), 4096)]), 0) {
		a.textTitle = fmt.Sprintf("%s:%s · binary file, %s · :download to save it", m.pod, m.path, kube.FormatBytes(int64(len(m.data))))
		a.textLines = []string{a.st.Muted.Render("(binary content not shown)")}
	} else {
		a.textTitle = fmt.Sprintf("%s:%s · %s · :download saves it · / search", m.pod, m.path, kube.FormatBytes(int64(len(m.data))))
		a.textLines = strings.Split(strings.TrimRight(string(m.data), "\n"), "\n")
	}
	a.renderText(false)
	a.vp.GotoTop()
}

type filesState struct{ ns, pod, container, dir, file string }

// downloadCommand: :download [remote] [local] (defaults: the open file, its name in the current folder).
func (a *App) downloadCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	fs := a.files
	ns, pod, ct := "", "", ""
	remote := ""
	if fs != nil {
		ns, pod, ct, remote = fs.ns, fs.pod, fs.container, fs.file
	} else if row, ok := a.selected(); ok && a.res.Name == "pods" {
		ns, pod = row.Namespace, row.Name
	}
	if len(f) > 0 {
		remote = f[0]
		if !strings.HasPrefix(remote, "/") && fs != nil {
			remote = path.Join(fs.dir, remote)
		}
	}
	if pod == "" || remote == "" {
		a.setStatus("usage: :download <path-in-pod> [local file]   (select a pod or open a file with B first)", true)
		return nil
	}
	local := path.Base(remote)
	if len(f) > 1 {
		local = f[1]
	}
	c := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*timeout)
		defer cancel()
		if ct == "" {
			ct = a.firstContainer(ctx, ns, pod)
		}
		n, err := c.DownloadFile(ctx, ns, pod, ct, remote, local)
		abs, _ := filepath.Abs(local)
		return actionMsg{status: fmt.Sprintf("downloaded %s:%s → %s (%s)", pod, remote, abs, kube.FormatBytes(n)), err: err}
	}
}

// uploadCommand: :upload <local> [path-in-pod] (copies into the open folder by default). It changes the pod, so it asks.
func (a *App) uploadCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	if len(f) == 0 {
		a.setStatus("usage: :upload <local file> [path-in-pod]", true)
		return nil
	}
	if !a.guardWrite() {
		return nil
	}
	local := f[0]
	if _, err := os.Stat(local); err != nil {
		a.setStatus(err.Error(), true)
		return nil
	}
	fs := a.files
	ns, pod, ct, dir := "", "", "", "/tmp"
	if fs != nil {
		ns, pod, ct, dir = fs.ns, fs.pod, fs.container, fs.dir
	} else if row, ok := a.selected(); ok && a.res.Name == "pods" {
		ns, pod = row.Namespace, row.Name
	}
	if pod == "" {
		a.setStatus("select a pod or open its files (B) first", true)
		return nil
	}
	remote := path.Join(dir, filepath.Base(local))
	if len(f) > 1 {
		remote = f[1]
	}
	c := a.client
	a.askConfirm(fmt.Sprintf("Copy %s into %s:%s on %s? (files in a container are lost when the pod is replaced)", local, pod, remote, c.Context), pod, func() tea.Cmd {
		return runAction(fmt.Sprintf("uploaded %s → %s:%s", filepath.Base(local), pod, remote), func(ctx context.Context) error {
			if ct == "" {
				ct = a.firstContainer(ctx, ns, pod)
			}
			return c.UploadFile(ctx, ns, pod, ct, local, remote)
		})
	})
	return nil
}

func metaGet() metav1.GetOptions { return metav1.GetOptions{} }
