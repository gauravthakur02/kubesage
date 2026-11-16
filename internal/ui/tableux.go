package ui

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// sortSpec is the sort order of one resource's table.
type sortSpec struct {
	col  string
	desc bool
}

var (
	ageRe = regexp.MustCompile(`^(?:(\d+)y)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?(?: ago)?$`)
	numRe = regexp.MustCompile(`^(-?\d+(?:\.\d+)?)\s*([a-zA-Z%]*)`)
	units = map[string]float64{"": 1, "%": 1, "m": 0.001, "k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12,
		"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "B": 1}
)

// sortValue turns a cell into a number when it looks like an age (2d16h), a quantity (184Mi, 250m, 3.0Gi),
// a ratio (1/2) or a count; ok=false means compare as text.
func sortValue(cell string) (float64, bool) {
	c := strings.TrimSpace(ansi.Strip(cell))
	if c == "" || c == "-" {
		return -1, true
	}
	if m := ageRe.FindStringSubmatch(c); m != nil && c != "" && strings.ContainsAny(c, "ydhms") && !strings.ContainsAny(c, ".") {
		mult := []float64{365 * 86400, 86400, 3600, 60, 1}
		var v float64
		for i, x := range m[1:] {
			if x != "" {
				n, _ := strconv.ParseFloat(x, 64)
				v += n * mult[i]
			}
		}
		return v, true
	}
	if a, b, ok := strings.Cut(c, "/"); ok {
		x, e1 := strconv.ParseFloat(a, 64)
		y, e2 := strconv.ParseFloat(strings.Fields(b + " ")[0], 64)
		if e1 == nil && e2 == nil {
			return x*1000 + y, true
		}
	}
	if m := numRe.FindStringSubmatch(c); m != nil {
		if mult, ok := units[m[2]]; ok {
			n, _ := strconv.ParseFloat(m[1], 64)
			// "Mi" etc. are bytes, "m" millicores: compare within one column only, so scale is consistent
			return n * mult, true
		}
	}
	return 0, false
}

// applySort orders a.visible by the resource's sort column (stable; numbers and ages compare numerically).
func (a *App) applySort() {
	sp, ok := a.sorts[a.res.Name]
	if !ok || sp.col == "" {
		return
	}
	ci := -1
	for i, c := range a.table.Columns {
		if c == sp.col {
			ci = i
		}
	}
	if ci < 0 {
		return
	}
	cell := func(idx int) string {
		if r := a.table.Rows[idx]; ci < len(r.Cells) {
			return r.Cells[ci]
		}
		return ""
	}
	sort.SliceStable(a.visible, func(i, j int) bool {
		x, y := cell(a.visible[i]), cell(a.visible[j])
		vx, okx := sortValue(x)
		vy, oky := sortValue(y)
		var less bool
		if okx && oky {
			less = vx < vy
		} else {
			less = strings.ToLower(ansi.Strip(x)) < strings.ToLower(ansi.Strip(y))
		}
		if sp.desc {
			return !less && x != y
		}
		return less
	})
}

// cycleSort moves the sort to the next visible column (( key), or reverses it () key).
func (a *App) cycleSort(reverse bool) {
	if a.sorts == nil {
		a.sorts = map[string]sortSpec{}
	}
	sp := a.sorts[a.res.Name]
	cols := a.visibleColumns()
	if reverse {
		if sp.col == "" && len(cols) > 0 {
			sp.col = cols[0]
		}
		sp.desc = !sp.desc
	} else {
		next := ""
		for i, c := range cols {
			if c == sp.col && i+1 < len(cols) {
				next = cols[i+1]
			}
		}
		if sp.col == "" && len(cols) > 0 {
			next = cols[0]
		}
		sp.col, sp.desc = next, false
	}
	a.sorts[a.res.Name] = sp
	a.applyFilter()
	if sp.col == "" {
		a.setStatus("sort: off (original order)", false)
	} else {
		dir := "ascending"
		if sp.desc {
			dir = "descending"
		}
		a.setStatus(fmt.Sprintf("sorted by %s, %s · ( next column · ) reverse", sp.col, dir), false)
	}
}

func (a *App) sortByColumn(col string) {
	if a.sorts == nil {
		a.sorts = map[string]sortSpec{}
	}
	sp := a.sorts[a.res.Name]
	if sp.col == col {
		sp.desc = !sp.desc
	} else {
		sp = sortSpec{col: col}
	}
	a.sorts[a.res.Name] = sp
	a.applyFilter()
}

// ---------- column hiding ----------

func (a *App) hiddenColumns() map[string]bool {
	h := map[string]bool{}
	for _, c := range a.cfg.HideColumns[a.res.Name] {
		h[strings.ToUpper(c)] = true
	}
	for _, al := range a.res.Aliases {
		for _, c := range a.cfg.HideColumns[al] {
			h[strings.ToUpper(c)] = true
		}
	}
	return h
}

func (a *App) visibleColumns() []string {
	hidden := a.hiddenColumns()
	var out []string
	for _, c := range a.table.Columns {
		if !hidden[strings.ToUpper(c)] {
			out = append(out, c)
		}
	}
	return out
}

// colsCommand toggles a column's visibility for this resource and saves it (:cols, :cols reset).
func (a *App) colsCommand(arg string) tea.Cmd {
	if arg == "reset" {
		delete(a.cfg.HideColumns, a.res.Name)
		_ = a.cfg.Save()
		a.setStatus("all columns shown", false)
		return nil
	}
	hidden := a.hiddenColumns()
	var labels []string
	for _, c := range a.table.Columns {
		mark := "☑"
		if hidden[strings.ToUpper(c)] {
			mark = "☐"
		}
		labels = append(labels, mark+" "+c)
	}
	if len(labels) == 0 {
		return nil
	}
	a.openPicker("Columns of "+a.res.Name+" · enter toggles · saved to the config", labels, func(s string) tea.Cmd {
		col := strings.TrimSpace(s[strings.Index(s, " ")+1:])
		if a.cfg.HideColumns == nil {
			a.cfg.HideColumns = map[string][]string{}
		}
		cur := a.cfg.HideColumns[a.res.Name]
		var next []string
		removed := false
		for _, c := range cur {
			if strings.EqualFold(c, col) {
				removed = true
				continue
			}
			next = append(next, c)
		}
		if !removed {
			if len(a.visibleColumns()) <= 1 {
				a.setStatus("keep at least one column", true)
				return nil
			}
			next = append(next, col)
		}
		a.cfg.HideColumns[a.res.Name] = next
		_ = a.cfg.Save()
		return a.colsCommand("")
	})
	return nil
}

// ---------- key remapping ----------

// remapKey translates a user-chosen key into kubesage's built-in key (keys: {w: g} → pressing g acts as w).
func (a *App) remapKey(k tea.KeyMsg) tea.KeyMsg {
	if len(a.cfg.Keys) == 0 || a.ov != ovNone || a.mode == modeAI {
		return k
	}
	pressed := k.String()
	for orig, custom := range a.cfg.Keys {
		if custom == pressed && orig != pressed {
			if len([]rune(orig)) == 1 {
				return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(orig)}
			}
			switch orig {
			case "enter":
				return tea.KeyMsg{Type: tea.KeyEnter}
			case "esc":
				return tea.KeyMsg{Type: tea.KeyEsc}
			}
		}
	}
	return k
}

// keyLabel shows the remapped key in hints.
func (a *App) keyLabel(k string) string {
	if c, ok := a.cfg.Keys[k]; ok && c != "" {
		return c
	}
	return k
}

// ---------- mouse ----------

// paneAt finds the pane under screen cell (x, y) and the pane's top-left corner.
func (a *App) paneAt(x, y int) (*pane, int, int) {
	const top = 2 // header + tab bar
	if a.zoomed {
		return a.pane, 0, top
	}
	var walk func(n *layoutNode, ox, oy int) (*pane, int, int)
	walk = func(n *layoutNode, ox, oy int) (*pane, int, int) {
		if n.leaf() {
			if x >= ox && x < ox+n.ow && y >= oy && y < oy+n.oh {
				return n.p, ox, oy
			}
			return nil, 0, 0
		}
		if n.stacked {
			if p, px, py := walk(n.a, ox, oy); p != nil {
				return p, px, py
			}
			return walk(n.b, ox, oy+n.a.oh)
		}
		if p, px, py := walk(n.a, ox, oy); p != nil {
			return p, px, py
		}
		return walk(n.b, ox+n.a.ow, oy)
	}
	return walk(a.root, 0, top)
}

// handleMouse: wheel scrolls, a click selects a row or sorts by a header, and focuses that pane.
func (a *App) handleMouse(m tea.MouseMsg) tea.Cmd {
	p, ox, oy := a.paneAt(m.X, m.Y)
	if p == nil || a.ov != ovNone {
		return nil
	}
	if p != a.pane && m.Action == tea.MouseActionPress && m.Button == tea.MouseButtonLeft {
		a.focus(p)
	}
	var cmd tea.Cmd
	a.withPane(p, func() {
		switch {
		case m.Button == tea.MouseButtonWheelUp || m.Button == tea.MouseButtonWheelDown:
			step := 3
			if m.Button == tea.MouseButtonWheelUp {
				step = -3
			}
			switch a.mode {
			case modeTable:
				a.cursor = max(0, min(len(a.visible)-1, a.cursor+step))
			case modeText:
				if step < 0 {
					a.vp.ScrollUp(-step)
				} else {
					a.vp.ScrollDown(step)
				}
				if a.textKind == "logs" {
					a.follow = a.vp.AtBottom()
				}
			}
		case m.Button == tea.MouseButtonLeft && m.Action == tea.MouseActionPress && a.mode == modeTable:
			rowY := m.Y - oy - 1 // inside the border
			if rowY == 0 {       // header: sort by the clicked column
				if col := a.columnAt(m.X - ox - 1 - 4); col != "" {
					a.sortByColumn(col)
				}
				return
			}
			if i := a.offset + rowY - 1; i >= 0 && i < len(a.visible) {
				a.cursor = i
			}
		}
	})
	return cmd
}

// columnAt maps an x offset in the table to a column, using the widths of the last render.
func (a *App) columnAt(x int) string {
	pos := 0
	for i, w := range a.colWidths {
		if x >= pos && x < pos+w+2 && i < len(a.colNames) {
			return a.colNames[i]
		}
		pos += w + 2
	}
	return ""
}

func (a *App) mouseCommand() tea.Cmd {
	a.cfg.Mouse = !a.cfg.Mouse
	_ = a.cfg.Save()
	if a.cfg.Mouse {
		a.setStatus("mouse on: click rows and headers, wheel scrolls · hold Option (macOS) or Shift to select text", false)
		return tea.EnableMouseCellMotion
	}
	a.setStatus("mouse off: the terminal selects text normally", false)
	return tea.DisableMouse
}

func (a *App) mouseInit() tea.Cmd {
	if a.cfg.Mouse {
		return tea.EnableMouseCellMotion
	}
	return nil
}
