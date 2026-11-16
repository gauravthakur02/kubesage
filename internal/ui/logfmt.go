package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var (
	levelKeys = []string{"level", "lvl", "severity", "loglevel", "log.level"}
	msgKeys   = []string{"msg", "message", "log", "text"}
	timeKeys  = []string{"time", "ts", "timestamp", "@timestamp", "t"}
)

func pick(m map[string]any, keys []string) (string, string) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return k, fmt.Sprint(v)
		}
	}
	return "", ""
}

// prettyJSONLog renders a JSON log line as "LEVEL time message key=value …" (ok=false if not JSON).
func (a *App) prettyJSONLog(l string) (string, bool) {
	t := strings.TrimSpace(l)
	if !strings.HasPrefix(t, "{") || !strings.HasSuffix(t, "}") {
		return l, false
	}
	var m map[string]any
	if json.Unmarshal([]byte(t), &m) != nil {
		return l, false
	}
	s := a.st
	lk, lvl := pick(m, levelKeys)
	mk, msg := pick(m, msgKeys)
	tk, ts := pick(m, timeKeys)
	if f, ok := m[tk].(float64); ok && tk != "" {
		sec := int64(f)
		if f > 1e12 {
			sec = int64(f / 1000)
		}
		ts = time.Unix(sec, 0).Format("15:04:05")
	} else if tm, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		ts = tm.Local().Format("15:04:05.000")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != lk && k != mk && k != tk {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var kv []string
	for _, k := range keys {
		v, _ := json.Marshal(m[k])
		vs := strings.Trim(string(v), `"`)
		kv = append(kv, s.YAMLKey.Render(k)+"="+vs)
	}
	ul := strings.ToUpper(lvl)
	lvlStyle := s.Muted
	switch {
	case strings.HasPrefix(ul, "ERR") || strings.HasPrefix(ul, "FATAL") || strings.HasPrefix(ul, "PANIC") || strings.HasPrefix(ul, "CRIT"):
		lvlStyle = s.ErrText
	case strings.HasPrefix(ul, "WARN"):
		lvlStyle = s.WarnText
	case strings.HasPrefix(ul, "INFO"):
		lvlStyle = s.OKText
	}
	out := fmt.Sprintf("%s %s %s", lvlStyle.Render(fmt.Sprintf("%-5s", nonEmptyStr(ul, "-"))), s.Muted.Render(ts), msg)
	if len(kv) > 0 {
		out += "  " + strings.Join(kv, " ")
	}
	return out, true
}

// logLineVisible applies the include/exclude filters of the logs view.
func (a *App) logLineVisible(l string) bool {
	plain := ansi.Strip(l)
	if a.logInclude != nil && !a.logInclude.MatchString(plain) {
		return false
	}
	if a.logExclude != nil && a.logExclude.MatchString(plain) {
		return false
	}
	return true
}

// logsKey handles the logs-only keys: J (pretty JSON), i/x (include/exclude regex), ctrl+s (save).
func (a *App) logsKey(k string) (tea.Cmd, bool) {
	if a.textKind != "logs" {
		return nil, false
	}
	switch k {
	case "J":
		a.logJSON = !a.logJSON
		state := "off"
		if a.logJSON {
			state = "on: JSON lines shown as LEVEL time message key=value"
		}
		a.setStatus("pretty JSON logs "+state, false)
		a.renderText(a.follow)
		return nil, true
	case "i", "x":
		which, cur := "include", a.logInclude
		if k == "x" {
			which, cur = "exclude", a.logExclude
		}
		a.openInput(ovInput, which+" lines matching (regex, empty clears) › ", func(q string) tea.Cmd {
			var re *regexp.Regexp
			if q = strings.TrimSpace(q); q != "" {
				var err error
				if re, err = regexp.Compile("(?i)" + q); err != nil {
					a.setStatus("bad regex: "+err.Error(), true)
					return nil
				}
			}
			if k == "x" {
				a.logExclude = re
			} else {
				a.logInclude = re
			}
			a.renderText(a.follow)
			a.setStatus(a.logFilterLabel(), false)
			return nil
		})
		if cur != nil {
			a.input.SetValue(strings.TrimPrefix(cur.String(), "(?i)"))
			a.input.CursorEnd()
		}
		return nil, true
	case "ctrl+s":
		name := "logs"
		if len(a.logSpec.pods) > 0 {
			name = a.logSpec.pods[0]
		}
		path := fmt.Sprintf("kubesage-logs-%s-%s.log", name, time.Now().Format("20060102-150405"))
		var b strings.Builder
		n := 0
		for _, l := range a.textLines {
			if a.logLineVisible(l) {
				b.WriteString(ansi.Strip(l) + "\n")
				n++
			}
		}
		err := os.WriteFile(path, []byte(b.String()), 0o644)
		abs, _ := filepath.Abs(path)
		if err != nil {
			a.setStatus("save failed: "+err.Error(), true)
		} else {
			a.setStatus(fmt.Sprintf("✓ saved %d lines to %s", n, abs), false)
		}
		return nil, true
	}
	return nil, false
}

func (a *App) logFilterLabel() string {
	var parts []string
	if a.logInclude != nil {
		parts = append(parts, "only /"+strings.TrimPrefix(a.logInclude.String(), "(?i)")+"/")
	}
	if a.logExclude != nil {
		parts = append(parts, "hiding /"+strings.TrimPrefix(a.logExclude.String(), "(?i)")+"/")
	}
	if len(parts) == 0 {
		return "log filters cleared"
	}
	return "log filter: " + strings.Join(parts, ", ")
}
