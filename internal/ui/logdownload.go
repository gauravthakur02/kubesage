package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubeweave/internal/kube"
)

type logsDownloadedMsg struct {
	path  string
	bytes int64
	files int
	err   error
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// downloadsDir is where downloads go: downloads_dir in the config, else ~/Downloads, else the current folder.
func (a *App) downloadsDir() string {
	if d := a.cfg.DownloadsDir; d != "" {
		if strings.HasPrefix(d, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				d = filepath.Join(home, d[2:])
			}
		}
		return d
	}
	if home, err := os.UserHomeDir(); err == nil {
		if st, err := os.Stat(filepath.Join(home, "Downloads")); err == nil && st.IsDir() {
			return filepath.Join(home, "Downloads")
		}
	}
	return "."
}

func (a *App) downloadPath(parts ...string) (string, error) {
	dir := a.downloadsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := unsafeName.ReplaceAllString(strings.Join(nonEmpty(parts), "_"), "-")
	return filepath.Join(dir, fmt.Sprintf("%s-%s", name, time.Now().Format("20060102-150405"))), nil
}

// downloadContainerLog saves the complete log of one container (not just what is on screen) (D in the logs view).
func (a *App) downloadContainerLog() tea.Cmd {
	spec := a.logSpec
	if len(spec.pods) == 0 {
		return nil
	}
	if len(spec.pods) > 1 { // a merged stream: bundle those pods
		return a.downloadBundle(kube.Resource{}, spec.ns, "", spec.pods, spec.since)
	}
	c, pod := a.client, spec.pods[0]
	suffix := ""
	if spec.previous {
		suffix = "previous"
	}
	path, err := a.downloadPath("kubeweave-logs", c.Context, spec.ns, pod, spec.container, suffix)
	if err != nil {
		a.setStatus("download: "+err.Error(), true)
		return nil
	}
	path += ".log"
	a.setStatus("downloading the full log of "+pod+"…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return logsDownloadedMsg{err: err}
		}
		n, err := c.WriteLogs(ctx, f, spec.ns, pod, kube.LogOptions{Container: spec.container, Previous: spec.previous, Since: spec.since, Timestamps: spec.timestamps})
		f.Close()
		if err != nil {
			os.Remove(path)
		}
		return logsDownloadedMsg{path: path, bytes: n, files: 1, err: err}
	}
}

// downloadBundle zips the logs of every pod and container of an object (:download-logs [6h]).
// With pods given (a merged log stream) only those pods are included.
func (a *App) downloadBundle(r kube.Resource, ns, name string, pods []string, since time.Duration) tea.Cmd {
	c := a.client
	label := name
	if label == "" {
		label = fmt.Sprintf("%d-pods", len(pods))
	}
	path, err := a.downloadPath("kubeweave-logs", c.Context, ns, label)
	if err != nil {
		a.setStatus("download: "+err.Error(), true)
		return nil
	}
	path += ".zip"
	a.setStatus("collecting logs of "+label+"…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return logsDownloadedMsg{err: err}
		}
		var res kube.LogBundleResult
		if len(pods) > 0 {
			res, err = c.WriteLogBundleForPods(ctx, f, ns, pods, since)
		} else {
			res, err = c.WriteLogBundle(ctx, f, r, ns, name, since, true)
		}
		f.Close()
		if err != nil {
			os.Remove(path)
		}
		return logsDownloadedMsg{path: path, bytes: res.Bytes, files: res.Files, err: err}
	}
}

// downloadLogsCommand handles :download-logs [6h] on the selected pod or workload, or in the logs view.
func (a *App) downloadLogsCommand(arg string) tea.Cmd {
	if a.mode == modeText && a.textKind == "logs" {
		return a.downloadContainerLog()
	}
	row, ok := a.selected()
	if a.mode != modeTable || !ok || !a.res.CanLogs() {
		a.setStatus("select a pod or workload first (:download-logs [6h])", true)
		return nil
	}
	return a.downloadBundle(a.res, row.Namespace, row.Name, nil, kube.ParseSince(arg, 0))
}

func (a *App) handleLogsDownloaded(m logsDownloadedMsg) {
	if m.err != nil {
		a.setStatus("log download failed: "+m.err.Error(), true)
		return
	}
	a.setStatus(fmt.Sprintf("✓ saved %s (%d file(s), %.1f MiB)", m.path, m.files, float64(m.bytes)/(1<<20)), false)
}

func nonEmpty(parts []string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
