package ui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

var dayIdx = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// dayAllowed parses "Mon-Fri" or "Mon,Wed,Fri".
func dayAllowed(spec string, d time.Weekday) bool {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if a, b, ok := strings.Cut(part, "-"); ok {
			from, to := dayIdx[a[:min(3, len(a))]], dayIdx[b[:min(3, len(b))]]
			for i := from; ; i = (i + 1) % 7 {
				if time.Weekday(i) == d {
					return true
				}
				if i == to {
					break
				}
			}
		} else if len(part) >= 3 && dayIdx[part[:3]] == int(d) {
			return true
		}
	}
	return false
}

func hhmm(s string) (int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hi, err1 := strconv.Atoi(h)
	mi, err2 := strconv.Atoi(m)
	return hi*60 + mi, ok && err1 == nil && err2 == nil
}

// windowAllows reports whether now falls inside a change window.
func windowAllows(w config.ChangeWindow, now time.Time) bool {
	if w.TZ != "" {
		if loc, err := time.LoadLocation(w.TZ); err == nil {
			now = now.In(loc)
		}
	}
	if !dayAllowed(w.Days, now.Weekday()) {
		return false
	}
	if w.Hours == "" {
		return true
	}
	a, b, _ := strings.Cut(w.Hours, "-")
	from, ok1 := hhmm(a)
	to, ok2 := hhmm(b)
	if !ok1 || !ok2 {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	if from <= to {
		return cur >= from && cur < to
	}
	return cur >= from || cur < to // over midnight
}

func parseWhen(s string, end bool) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		if end {
			t = t.Add(24*time.Hour - time.Second)
		}
		return t, true
	}
	return time.Time{}, false
}

// changePolicy decides whether a change to ctx is allowed now: "" = allowed; warn = allowed with a warning.
func changePolicy(cfg *config.Config, ctx string, now time.Time) (msg string, warn bool) {
	for _, f := range cfg.Freezes {
		re, err := regexp.Compile(f.Contexts)
		if err != nil || !re.MatchString(ctx) {
			continue
		}
		from, ok1 := parseWhen(f.From, false)
		to, ok2 := parseWhen(f.To, true)
		if ok1 && ok2 && !now.Before(from) && !now.After(to) {
			return fmt.Sprintf("change freeze on %s until %s: %s", ctx, f.To, nonEmptyStr(f.Reason, "no changes allowed")), false
		}
	}
	for _, w := range cfg.ChangeWindows {
		re, err := regexp.Compile(w.Contexts)
		if err != nil || !re.MatchString(ctx) {
			continue
		}
		if windowAllows(w, now) {
			return "", false
		}
		desc := strings.TrimSpace(nonEmptyStr(w.Days, "every day") + " " + w.Hours + " " + w.TZ)
		return fmt.Sprintf("outside the change window for %s (%s)", ctx, desc), w.Mode == "warn"
	}
	return "", false
}

// reasonRequired reports whether changes to the current context need a reason.
func (a *App) reasonRequired() bool {
	if a.cfg.RequireReason == "" {
		return false
	}
	re, err := regexp.Compile(a.cfg.RequireReason)
	return err == nil && re.MatchString(a.client.Context)
}

// withReason asks for a reason before running fn (when required for this context).
func (a *App) withReason(fn func() tea.Cmd) func() tea.Cmd {
	return func() tea.Cmd {
		a.openInput(ovInput, "reason for this change on "+a.client.Context+" (kept in the audit log) › ", func(r string) tea.Cmd {
			if strings.TrimSpace(r) == "" {
				a.setStatus("a reason is required for changes on this context: cancelled", true)
				return nil
			}
			a.changeReason = strings.TrimSpace(r)
			return fn()
		})
		return nil
	}
}

// ---------- port-forward profiles ----------

// fwdCommand: :fwd (list) · :fwd save <name> · :fwd <name> (start) · :fwd stop <name> · :fwd rm <name>.
func (a *App) fwdCommand(arg string) tea.Cmd {
	f := strings.Fields(arg)
	if len(f) == 0 {
		if len(a.cfg.ForwardProfiles) == 0 {
			a.setStatus("no forward profiles yet: start forwards (F), then :fwd save <name>", false)
			return nil
		}
		var names []string
		for n, specs := range a.cfg.ForwardProfiles {
			names = append(names, fmt.Sprintf("%s (%d)", n, len(specs)))
		}
		labels := names
		a.openPicker("Port-forward profiles · enter starts one", labels, func(s string) tea.Cmd {
			name, _, _ := strings.Cut(s, " (")
			return a.startProfile(name)
		})
		return nil
	}
	switch f[0] {
	case "save":
		if len(f) < 2 {
			a.setStatus("usage: :fwd save <name>", true)
			return nil
		}
		var specs []config.ForwardSpec
		for _, fw := range kube.Forwards() {
			specs = append(specs, config.ForwardSpec{Context: fw.Context, Namespace: fw.Namespace, Resource: fw.Resource, Name: fw.Object, Local: fw.Local, Remote: fw.Exposed})
		}
		if len(specs) == 0 {
			a.setStatus("no active port-forwards to save (start some with F first)", true)
			return nil
		}
		if a.cfg.ForwardProfiles == nil {
			a.cfg.ForwardProfiles = map[string][]config.ForwardSpec{}
		}
		a.cfg.ForwardProfiles[f[1]] = specs
		if err := a.cfg.Save(); err != nil {
			a.setStatus(err.Error(), true)
			return nil
		}
		a.setStatus(fmt.Sprintf("✓ saved %d forward(s) as %q · :fwd %s starts them", len(specs), f[1], f[1]), false)
		return nil
	case "rm", "delete":
		if len(f) > 1 {
			delete(a.cfg.ForwardProfiles, f[1])
			_ = a.cfg.Save()
			a.setStatus("removed profile "+f[1], false)
		}
		return nil
	case "stop":
		if len(f) < 2 {
			return nil
		}
		n := 0
		for _, spec := range a.cfg.ForwardProfiles[f[1]] {
			for _, fw := range kube.Forwards() {
				if fw.Context == spec.Context && fw.Local == spec.Local {
					fw.Stop()
					n++
				}
			}
		}
		a.setStatus(fmt.Sprintf("stopped %d forward(s) of %s", n, f[1]), false)
		return nil
	}
	return a.startProfile(f[0])
}

func (a *App) startProfile(name string) tea.Cmd {
	specs, ok := a.cfg.ForwardProfiles[name]
	if !ok {
		a.setStatus("no forward profile "+name, true)
		return nil
	}
	var cmds []tea.Cmd
	for _, spec := range specs {
		spec := spec
		connect := a.connect(spec.Context, spec.Namespace, "probe")
		cmds = append(cmds, func() tea.Msg {
			m := connect().(connectMsg)
			if m.err != nil {
				return pfStartedMsg{err: fmt.Errorf("%s: %w", spec.Context, m.err)}
			}
			r, ok := kube.Lookup(spec.Resource)
			if !ok {
				return pfStartedMsg{err: fmt.Errorf("unknown resource %s", spec.Resource)}
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			targets, err := m.client.PortTargets(ctx, r, spec.Namespace, spec.Name)
			if err != nil {
				return pfStartedMsg{err: err}
			}
			for _, t := range targets {
				if t.ExposedAs == spec.Remote || t.Port == spec.Remote {
					return startForward(m.client, t, spec.Local, t.Port)()
				}
			}
			if len(targets) > 0 {
				return startForward(m.client, targets[0], spec.Local, targets[0].Port)()
			}
			return pfStartedMsg{err: fmt.Errorf("%s/%s has no port %d", spec.Resource, spec.Name, spec.Remote)}
		})
	}
	a.setStatus(fmt.Sprintf("starting %d forward(s) of %s…", len(specs), name), false)
	return tea.Batch(cmds...)
}
