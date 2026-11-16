package ui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubesage/internal/config"
	"kubesage/internal/kube"
)

// fieldPath reads a simple JSONPath-like path: spec.containers[0].image, spec.containers[*].name,
// metadata.labels.app (a leading "." or "{.…}" is accepted).
func fieldPath(obj map[string]any, path string) string {
	path = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(path), "{"), "}")
	path = strings.TrimPrefix(path, ".")
	vals := []any{obj}
	for _, part := range splitPath(path) {
		name, idx, hasIdx := part, "", false
		if i := strings.Index(part, "["); i >= 0 && strings.HasSuffix(part, "]") {
			name, idx, hasIdx = part[:i], part[i+1:len(part)-1], true
		}
		var next []any
		for _, v := range vals {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			x, ok := m[name]
			if !ok {
				continue
			}
			if !hasIdx {
				next = append(next, x)
				continue
			}
			arr, ok := x.([]any)
			if !ok {
				continue
			}
			if idx == "*" {
				next = append(next, arr...)
			} else if n, err := strconv.Atoi(idx); err == nil && n >= 0 && n < len(arr) {
				next = append(next, arr[n])
			}
		}
		vals = next
	}
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case nil:
		case map[string]any, []any:
			out = append(out, fmt.Sprintf("%v", t))
		default:
			out = append(out, fmt.Sprint(t))
		}
	}
	return strings.Join(out, ",")
}

// splitPath splits on dots outside brackets; label keys with dots can be escaped as \.
func splitPath(p string) []string {
	var parts []string
	var cur strings.Builder
	depth := 0
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case ch == '\\' && i+1 < len(p):
			cur.WriteByte(p[i+1])
			i++
		case ch == '[':
			depth++
			cur.WriteByte(ch)
		case ch == ']':
			depth--
			cur.WriteByte(ch)
		case ch == '.' && depth == 0:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// customColumns returns the configured extra columns for a resource.
func (a *App) customColumns(r kube.Resource) []config.Column {
	if cols, ok := a.cfg.Columns[r.Name]; ok {
		return cols
	}
	for _, al := range r.Aliases {
		if cols, ok := a.cfg.Columns[al]; ok {
			return cols
		}
	}
	return nil
}

// fetchListColumns lists r and appends custom columns from the raw objects (one extra list call).
func fetchListColumns(c *kube.Client, r kube.Resource, gen int, cols []config.Column) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		t, err := r.List(ctx, c)
		if err != nil || c.Dynamic == nil {
			return listMsg{gen: gen, table: t, err: err}
		}
		ri := c.Dynamic.Resource(r.GVR)

		var ns string
		if r.Namespaced {
			ns = c.Namespace
		}
		l, lerr := ri.Namespace(ns).List(ctx, metav1.ListOptions{})
		if !r.Namespaced {
			l, lerr = ri.List(ctx, metav1.ListOptions{})
		}
		byKey := map[string]map[string]any{}
		if lerr == nil {
			for i := range l.Items {
				u := &l.Items[i]
				byKey[u.GetNamespace()+"/"+u.GetName()] = u.Object
			}
		}
		for _, col := range cols {
			t.Columns = append(t.Columns, strings.ToUpper(col.Name))
		}
		for i := range t.Rows {
			obj := byKey[t.Rows[i].Namespace+"/"+t.Rows[i].Name]
			for _, col := range cols {
				v := ""
				if obj != nil {
					switch {
					case col.Label != "":
						v = fieldPath(obj, "metadata.labels."+strings.ReplaceAll(col.Label, ".", `\.`))
					case col.Annotation != "":
						v = fieldPath(obj, "metadata.annotations."+strings.ReplaceAll(col.Annotation, ".", `\.`))
					default:
						v = fieldPath(obj, col.Path)
					}
				}
				if v == "" {
					v = "-"
				}
				t.Rows[i].Cells = append(t.Rows[i].Cells, v)
			}
		}
		return listMsg{gen: gen, table: t}
	}
}
