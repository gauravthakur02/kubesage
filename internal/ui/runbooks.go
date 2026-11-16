package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"sigs.k8s.io/yaml"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

const runbooksRes = "runbooks"

type runbook struct {
	Path, Title string
	Match       []string
	Body        string
}

func (a *App) runbookDir() string {
	d := a.cfg.Runbooks.Dir
	if strings.HasPrefix(d, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			d = filepath.Join(home, d[2:])
		}
	}
	return d
}

// loadRunbooks reads every .md file under the runbook folder.
func (a *App) loadRunbooks() ([]runbook, error) {
	dir := a.runbookDir()
	if dir == "" {
		return nil, fmt.Errorf("no runbook folder configured (runbooks: {dir: ~/runbooks} in the config)")
	}
	var out []runbook
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") && p != dir {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".md") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		out = append(out, parseRunbook(p, string(b)))
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, err
}

func parseRunbook(path, text string) runbook {
	rb := runbook{Path: path, Body: text}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			var fm struct {
				Title string   `json:"title"`
				Match []string `json:"match"`
			}
			if yaml.Unmarshal([]byte(text[4:4+end]), &fm) == nil {
				rb.Title, rb.Match = fm.Title, fm.Match
			}
			rb.Body = strings.TrimLeft(text[4+end+4:], "\n")
		}
	}
	if rb.Title == "" {
		for _, l := range strings.Split(rb.Body, "\n") {
			if strings.HasPrefix(l, "# ") {
				rb.Title = strings.TrimPrefix(l, "# ")
				break
			}
		}
	}
	if rb.Title == "" {
		rb.Title = base
	}
	if len(rb.Match) == 0 {
		rb.Match = []string{base}
	}
	return rb
}

// matchRunbooks returns runbooks whose match terms appear in text (case-insensitive), best first.
func matchRunbooks(rbs []runbook, text string, limit int) []runbook {
	lt := strings.ToLower(text)
	type scored struct {
		rb runbook
		n  int
	}
	var hits []scored
	for _, rb := range rbs {
		n := 0
		for _, m := range rb.Match {
			if m = strings.ToLower(strings.TrimSpace(m)); len(m) >= 3 && strings.Contains(lt, m) {
				n += strings.Count(lt, m)
			}
		}
		if n > 0 {
			hits = append(hits, scored{rb, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	var out []runbook
	for i, h := range hits {
		if i >= limit {
			break
		}
		out = append(out, h.rb)
	}
	return out
}

// withRunbooks appends matching team runbooks to an AI prompt.
func (a *App) withRunbooks(prompt string) (string, int) {
	if a.cfg.Runbooks.Dir == "" {
		return prompt, 0
	}
	rbs, err := a.loadRunbooks()
	if err != nil || len(rbs) == 0 {
		return prompt, 0
	}
	m := matchRunbooks(rbs, prompt, 3)
	if len(m) == 0 {
		return prompt, 0
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\nThe team has runbooks for this. Follow them first, say which runbook step applies, and only then add your own ideas:\n")
	for _, rb := range m {
		body := rb.Body
		if len(body) > 4000 {
			body = body[:4000] + "\n…(truncated)"
		}
		fmt.Fprintf(&b, "\n### Runbook: %s (%s)\n%s\n", rb.Title, filepath.Base(rb.Path), body)
	}
	return b.String(), len(m)
}

func (a *App) runbooksResource() kube.Resource {
	return kube.VirtualResource(runbooksRes, "Runbook", "Runbooks", func() kube.Table {
		t := kube.Table{Columns: []string{"TITLE", "MATCHES", "FILE"}}
		rbs, err := a.loadRunbooks()
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		for _, rb := range rbs {
			rel, _ := filepath.Rel(a.runbookDir(), rb.Path)
			t.Rows = append(t.Rows, kube.Row{Name: rb.Path, Health: kube.HealthNeutral, Cells: []string{rb.Title, strings.Join(rb.Match, ", "), rel}})
		}
		t.Note = fmt.Sprintf("%d runbooks in %s · the AI uses matching ones automatically", len(rbs), a.runbookDir())
		return t
	})
}

func (a *App) openRunbook(rb runbook) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	lines := []string{}
	for _, l := range strings.Split(strings.TrimRight(rb.Body, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "#"):
			l = a.st.Title.Render(l)
		case strings.HasPrefix(strings.TrimSpace(l), "```"):
			l = a.st.Muted.Render(l)
		}
		lines = append(lines, l)
	}
	a.mode, a.textKind, a.textTitle, a.textLines = modeText, "runbook", "runbook · "+rb.Title+" · "+rb.Path, lines
	a.renderText(false)
	a.vp.GotoTop()
	return nil
}

func (a *App) runbooksCommand(arg string) tea.Cmd {
	if arg == "pull" {
		dir := a.runbookDir()
		return runAction("updated runbooks (git pull)", func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "git", "-C", dir, "pull", "--ff-only").CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return nil
		})
	}
	return a.openVirtual(a.runbooksResource(), "enter: read · :runbooks pull updates a git folder")
}

// runbookFor opens the best runbook for an issue (r in alerts).
func (a *App) runbookFor(text string) tea.Cmd {
	rbs, err := a.loadRunbooks()
	if err != nil {
		a.setStatus(err.Error(), true)
		return nil
	}
	m := matchRunbooks(rbs, text, 1)
	if len(m) == 0 {
		a.setStatus("no runbook matches this; add one with match: [...] front matter", true)
		return nil
	}
	return a.openRunbook(m[0])
}
