package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/config"
)

// auditEntry is one line of the audit log (JSON lines, next to config.yaml).
type auditEntry struct {
	Time    time.Time `json:"time"`
	Context string    `json:"context"`
	NS      string    `json:"namespace,omitempty"`
	Action  string    `json:"action"`
	Target  string    `json:"target,omitempty"`
	By      string    `json:"by"` // user or ai
	Reason  string    `json:"reason,omitempty"`
	Result  string    `json:"result"`
}

var auditMu sync.Mutex

func (a *App) auditPath() string {
	p := a.cfg.Path()
	if p == "" {
		p = config.DefaultPath()
	}
	return filepath.Join(filepath.Dir(p), "audit.log")
}

// audit appends a change to the audit log (never secrets: only action names and targets).
func (a *App) audit(action, ctx, ns, target string, err error) {
	a.auditBy("user", action, ctx, ns, target, err)
}

func (a *App) auditBy(by, action, ctx, ns, target string, err error) {
	if !a.cfg.AuditEnabled() {
		return
	}
	res := "ok"
	if err != nil {
		res = "error: " + firstLineStr(err.Error(), 200)
	}
	e := auditEntry{Time: time.Now().UTC(), Context: ctx, NS: ns, Action: action, Target: target, By: by, Result: res, Reason: a.changeReason}
	a.changeReason = ""
	b, _ := json.Marshal(e)
	auditMu.Lock()
	defer auditMu.Unlock()
	path := a.auditPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, ferr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// openAudit shows the most recent changes (:audit).
func (a *App) openAudit() tea.Cmd {
	path := a.auditPath()
	f, err := os.Open(path)
	var entries []auditEntry
	if err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var e auditEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				entries = append(entries, e)
			}
		}
		f.Close()
	}
	s := a.st
	lines := []string{}
	for i := len(entries) - 1; i >= 0 && len(lines) < 500; i-- {
		e := entries[i]
		st := s.OKText
		if e.Result != "ok" {
			st = s.ErrText
		}
		by := e.By
		if by == "ai" {
			by = s.WarnText.Render("ai  ")
		} else {
			by = s.Muted.Render("user")
		}
		line := fmt.Sprintf("%s  %s  %-26s %s  %s  %s", s.Muted.Render(e.Time.Local().Format("2006-01-02 15:04:05")), by,
			e.Context, s.YAMLKey.Render(e.Action), strings.TrimSpace(e.NS+" "+e.Target), st.Render(e.Result))
		if e.Reason != "" {
			line += s.Muted.Render("  reason: " + e.Reason)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		lines = []string{s.Muted.Render("no changes recorded yet")}
	}
	state := "on"
	if !a.cfg.AuditEnabled() {
		state = "OFF (audit_log: false)"
	}
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind = modeText, "audit"
	a.textTitle = fmt.Sprintf("audit log · %d changes · newest first · %s · %s", len(entries), state, path)
	a.textLines = lines
	a.renderText(false)
	a.vp.GotoTop()
	return nil
}

// ---------- export ----------

// exportTable writes the visible rows of the table as csv, json or md (:export csv [path]).
func (a *App) exportTable(arg string) tea.Cmd {
	if a.mode != modeTable || len(a.table.Columns) == 0 {
		a.setStatus("export works on tables", true)
		return nil
	}
	fields := strings.Fields(arg)
	format := "csv"
	if len(fields) > 0 {
		format = strings.ToLower(fields[0])
	}
	if format != "csv" && format != "json" && format != "md" {
		a.setStatus("usage: :export csv|json|md [file]", true)
		return nil
	}
	name := strings.NewReplacer("/", "-", ":", "-").Replace(a.res.Name)
	path := fmt.Sprintf("kubesage-%s-%s.%s", name, time.Now().Format("20060102-150405"), format)
	if len(fields) > 1 {
		path = fields[1]
	}
	cols := a.table.Columns
	rows := make([][]string, 0, len(a.visible))
	for _, i := range a.visible {
		rows = append(rows, a.table.Rows[i].Cells)
	}
	data := renderExport(format, cols, rows)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		a.setStatus("export failed: "+err.Error(), true)
		return nil
	}
	abs, _ := filepath.Abs(path)
	a.setStatus(fmt.Sprintf("✓ exported %d rows to %s", len(rows), abs), false)
	return nil
}

func renderExport(format string, cols []string, rows [][]string) string {
	var b strings.Builder
	switch format {
	case "json":
		var out []map[string]string
		for _, r := range rows {
			m := map[string]string{}
			for i, c := range cols {
				if i < len(r) {
					m[c] = kubeStrip(r[i])
				}
			}
			out = append(out, m)
		}
		j, _ := json.MarshalIndent(out, "", "  ")
		b.Write(j)
		b.WriteString("\n")
	case "md":
		b.WriteString("| " + strings.Join(cols, " | ") + " |\n|")
		for range cols {
			b.WriteString("---|")
		}
		b.WriteString("\n")
		for _, r := range rows {
			cells := make([]string, len(cols))
			for i := range cols {
				if i < len(r) {
					cells[i] = strings.ReplaceAll(kubeStrip(r[i]), "|", "\\|")
				}
			}
			b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
	default:
		w := func(cells []string) {
			q := make([]string, len(cells))
			for i, c := range cells {
				c = kubeStrip(c)
				if strings.ContainsAny(c, ",\"\n") {
					c = `"` + strings.ReplaceAll(c, `"`, `""`) + `"`
				}
				q[i] = c
			}
			b.WriteString(strings.Join(q, ",") + "\n")
		}
		w(cols)
		for _, r := range rows {
			w(r)
		}
	}
	return b.String()
}
