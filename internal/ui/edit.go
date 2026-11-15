package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"kubesage/internal/kube"
)

// editState is a safe-edit session: edit in $EDITOR → diff + server dry-run → apply on request.
type editState struct {
	res              kube.Resource
	ns, name         string
	path             string
	original, edited string
	dry              kube.ApplyResult
	dryErr           error
	checking         bool
}

type editLoadedMsg struct {
	yaml string
	err  error
}
type editDoneMsg struct{ err error }
type dryRunMsg struct {
	res kube.ApplyResult
	err error
}
type applyDoneMsg struct {
	res kube.ApplyResult
	err error
}

// editorCommand picks $KUBE_EDITOR, $EDITOR or $VISUAL, falling back to vi / notepad.
func editorCommand(path string) *exec.Cmd {
	for _, env := range []string{"KUBE_EDITOR", "EDITOR", "VISUAL"} {
		if v := strings.Fields(os.Getenv(env)); len(v) > 0 {
			return exec.Command(v[0], append(v[1:], path)...)
		}
	}
	if runtime.GOOS == "windows" {
		return exec.Command("notepad", path)
	}
	return exec.Command("vi", path)
}

// execEditorFn launches the editor (swapped in tests).
var execEditorFn = func(path string, done func(error) tea.Msg) tea.Cmd {
	return tea.ExecProcess(editorCommand(path), done)
}

// openEdit starts a safe edit of an object.
func (a *App) openEdit(r kube.Resource, ns, name string) tea.Cmd {
	if !a.guardWrite() {
		return nil
	}
	a.edit = &editState{res: r, ns: ns, name: name}
	c := a.client
	a.setStatus(fmt.Sprintf("loading %s/%s for editing…", strings.ToLower(r.Kind), name), false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		y, err := c.EditableYAML(ctx, r, ns, name)
		return editLoadedMsg{yaml: y, err: err}
	}
}

func (a *App) handleEditLoaded(m editLoadedMsg) tea.Cmd {
	e := a.edit
	if e == nil {
		return nil
	}
	if m.err != nil {
		a.edit = nil
		a.setStatus("cannot edit: "+m.err.Error(), true)
		return nil
	}
	f, err := os.CreateTemp("", "kubesage-"+strings.ToLower(e.res.Kind)+"-"+e.name+"-*.yaml")
	if err == nil {
		_, err = f.WriteString(m.yaml)
		f.Close()
	}
	if err != nil {
		a.edit = nil
		a.setStatus("cannot create temp file: "+err.Error(), true)
		return nil
	}
	e.path, e.original, e.edited = f.Name(), m.yaml, m.yaml
	return execEditorFn(e.path, func(err error) tea.Msg { return editDoneMsg{err: err} })
}

func (a *App) handleEditDone(m editDoneMsg) tea.Cmd {
	e := a.edit
	if e == nil {
		return nil
	}
	if m.err != nil {
		a.setStatus("editor failed: "+m.err.Error()+" (set $EDITOR)", true)
		a.endEdit()
		return nil
	}
	b, err := os.ReadFile(e.path)
	if err != nil {
		a.setStatus(err.Error(), true)
		a.endEdit()
		return nil
	}
	e.edited = string(b)
	if strings.TrimSpace(e.edited) == strings.TrimSpace(e.original) {
		a.setStatus("no changes", false)
		a.endEdit()
		return nil
	}
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind = modeText, "editdiff"
	e.checking = true
	a.renderEdit()
	c := a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res, err := c.UpdateFromYAML(ctx, e.res, e.ns, e.name, e.edited, true)
		return dryRunMsg{res: res, err: err}
	}
}

func (a *App) handleDryRun(m dryRunMsg) {
	if a.edit == nil {
		return
	}
	a.edit.checking, a.edit.dry, a.edit.dryErr = false, m.res, m.err
	if a.mode == modeText && a.textKind == "editdiff" {
		a.renderEdit()
	}
}

// renderEdit shows the dry-run verdict, server warnings and the diff.
func (a *App) renderEdit() {
	e, s := a.edit, a.st
	d := diffLines(strings.Split(e.original, "\n"), strings.Split(e.edited, "\n"), 3)
	add, del := diffStats(d)
	a.textTitle = fmt.Sprintf("edit %s/%s · %s", strings.ToLower(e.res.Kind), e.name, diffHeader(add, del))
	var lines []string
	switch {
	case e.checking:
		lines = append(lines, s.Muted.Render("⋯ server-side dry-run running…"))
	case e.dryErr != nil:
		lines = append(lines, s.ErrText.Bold(true).Render("✗ The API server would reject this change:"), s.ErrText.Render("  "+e.dryErr.Error()),
			s.Muted.Render("  e: edit again · esc: discard"))
	default:
		lines = append(lines, s.OKText.Bold(true).Render("✓ Server-side dry-run passed (validation, admission webhooks and policies)"))
		ctx := a.client.Context
		lines = append(lines, s.Muted.Render("  A: apply to "+ctx+" · e: edit again · esc: discard"))
	}
	for _, w := range e.dry.Warnings {
		lines = append(lines, s.WarnText.Render("▲ warning: "+w))
	}
	lines = append(lines, "")
	lines = append(lines, a.renderDiffLines(d)...)
	a.textLines = lines
	a.renderText(true)
}

func (a *App) endEdit() {
	if a.edit != nil && a.edit.path != "" {
		os.Remove(a.edit.path)
	}
	a.edit = nil
}

// updateEdit handles keys in the edit diff view; it returns handled=false for normal scrolling.
func (a *App) updateEdit(key string) (tea.Cmd, bool) {
	e := a.edit
	if e == nil || a.textKind != "editdiff" {
		return nil, false
	}
	switch key {
	case "e":
		return execEditorFn(e.path, func(err error) tea.Msg { return editDoneMsg{err: err} }), true
	case "A":
		if e.checking || e.dryErr != nil {
			a.setStatus("fix the problems first (the dry-run must pass)", true)
			return nil, true
		}
		if !a.guardWrite() {
			return nil, true
		}
		c := a.client
		a.askConfirm(fmt.Sprintf("Apply this change to %s %s on %s?", strings.ToLower(e.res.Kind), objName(e.ns, e.name), c.Context), e.name, func() tea.Cmd {
			return func() tea.Msg {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				res, err := c.UpdateFromYAML(ctx, e.res, e.ns, e.name, e.edited, false)
				return applyDoneMsg{res: res, err: err}
			}
		})
		return nil, true
	case "esc", "q":
		a.endEdit()
		a.setStatus("edit discarded", false)
		a.mode = modeTable
		return a.refresh(), true
	}
	return nil, false
}

func (a *App) handleApplyDone(m applyDoneMsg) tea.Cmd {
	e := a.edit
	if e == nil {
		return nil
	}
	if m.err != nil {
		msg := m.err.Error()
		if strings.Contains(msg, "the object has been modified") {
			msg = "it changed on the server since you opened it. Press e to edit again (your changes are kept), or esc to discard"
		}
		a.setStatus("apply failed: "+msg, true)
		return nil
	}
	name := e.name
	a.endEdit()
	a.mode = modeTable
	status := "✓ applied " + name
	if len(m.res.Warnings) > 0 {
		status += " (with " + fmt.Sprint(len(m.res.Warnings)) + " warning(s))"
	}
	a.setStatus(status, false)
	return a.refresh()
}
