package ui

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"

	"kubesage/internal/kube"
)

// ---------- pod health ----------

type healthMsg struct {
	pane    *pane
	report  *kube.PodHealth
	err     error
	refresh bool
}

func (m healthMsg) targetPane() *pane { return m.pane }

func fetchHealth(c *kube.Client, ns, name string, refresh bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		r, err := c.PodHealthReport(ctx, ns, name)
		return healthMsg{report: r, err: err, refresh: refresh}
	}
}

func (a *App) openHealth(ns, pod string) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode = modeText
	podRes, _ := kube.Lookup("pods")
	a.target = targetRef{podRes, ns, pod}
	a.textKind, a.textTitle, a.textLines = "health", "health pod/"+pod, nil
	a.renderText(true)
	return tagPane(a.pane, fetchHealth(a.client, ns, pod, false))
}

func (a *App) handleHealth(m healthMsg) {
	if a.mode != modeText || a.textKind != "health" {
		return // user left the view
	}
	if m.err != nil {
		a.textLines = []string{a.st.ErrText.Render("✗ " + m.err.Error())}
		a.renderText(!m.refresh)
		return
	}
	a.textTitle = "health pod/" + m.report.Name + " · refreshes every " + fmt.Sprint(a.cfg.RefreshSeconds) + "s"
	var u kube.Usage
	have := false
	for _, c := range m.report.Containers {
		if c.Usage != nil {
			u.CPUMilli += c.Usage.CPUMilli
			u.MemBytes += c.Usage.MemBytes
			have = true
		}
	}
	if have {
		a.hist.record(histKey(a.client.Context, m.report.Namespace, m.report.Name), u, time.Now())
	}
	a.textLines = a.healthLines(m.report)
	a.renderText(!m.refresh)
}

func (a *App) meter(pct, w int) string {
	s := a.st
	if pct < 0 {
		return s.Muted.Render(strings.Repeat("·", w))
	}
	fill := min(w, pct*w/100)
	color := s.T.OK
	switch {
	case pct >= 90:
		color = s.T.Err
	case pct >= 75:
		color = s.T.Warn
	}
	return lipgloss.NewStyle().Foreground(color).Render(strings.Repeat("█", fill)) + s.Muted.Render(strings.Repeat("░", w-fill))
}

func (a *App) healthLines(h *kube.PodHealth) []string {
	s := a.st
	var out []string
	verdict := s.OKText.Bold(true).Render("✓ " + h.Summary)
	switch h.Verdict {
	case kube.HealthErr:
		verdict = s.ErrText.Bold(true).Render("✗ " + h.Summary)
	case kube.HealthWarn:
		verdict = s.WarnText.Bold(true).Render("▲ " + h.Summary)
	case kube.HealthNeutral:
		verdict = s.Muted.Bold(true).Render("○ " + h.Summary)
	}
	label := func(l string) string { return s.YAMLKey.Render(fmt.Sprintf("%-12s", l)) }
	out = append(out, "", "  "+verdict, "",
		"  "+label("Pod")+s.Base.Render(h.Namespace+"/"+h.Name),
		"  "+label("Status")+s.Base.Render(fmt.Sprintf("%s (phase %s) · QoS %s · age %s", h.Status, h.Phase, h.QoS, h.Age)),
		"  "+label("Node")+s.Base.Render(fmt.Sprintf("%s · IP %s", h.Node, h.IP)))

	var conds []string
	for _, c := range h.Conditions {
		mark := s.OKText.Render("✓")
		if !c.OK {
			mark = s.ErrText.Render("✗")
		}
		conds = append(conds, mark+" "+s.Base.Render(c.Type)+s.Muted.Render(" "+c.Since))
	}
	out = append(out, "  "+label("Conditions")+strings.Join(conds, "   "))
	for _, c := range h.Conditions {
		if !c.OK && c.Message != "" {
			out = append(out, "  "+strings.Repeat(" ", 12)+s.WarnText.Render(c.Type+": "+strings.TrimSpace(c.Reason+" "+c.Message)))
		}
	}
	if h.MetricsErr != nil {
		out = append(out, "  "+label("Usage")+s.Muted.Render("live CPU/memory unavailable (metrics-server missing or not allowed)"))
	} else if hs := a.hist.get(histKey(a.client.Context, h.Namespace, h.Name)); len(hs) >= 2 {
		span := time.Since(hs[0].at).Round(time.Minute)
		milli := func(v int64) string { return fmt.Sprintf("%dm", v) }
		out = append(out,
			"  "+label("CPU trend")+s.Title.Render(sparkline(series(hs, false), 30))+"  "+s.Muted.Render(trendSummary(series(hs, false), milli)+" · last "+span.String()),
			"  "+label("Mem trend")+s.Title.Render(sparkline(series(hs, true), 30))+"  "+s.Muted.Render(trendSummary(series(hs, true), kube.FormatBytes)+" · last "+span.String()))
	} else {
		out = append(out, "  "+label("Trend")+s.Muted.Render("collecting… (one sample every 10s, up to 15 minutes; also from the pods table)"))
	}

	for _, c := range h.Containers {
		out = append(out, "")
		ready := s.OKText.Render("ready ✓")
		if !c.Ready {
			ready = s.ErrText.Render("not ready ✗")
		}
		kind := "container"
		if c.Init {
			kind, ready = "init container", ""
		}
		state := s.Base.Render(c.State)
		switch c.State {
		case "Waiting", "Terminated":
			if !(c.Init && strings.HasPrefix(c.StateDetail, "Completed")) {
				state = s.WarnText.Render(c.State)
			}
		}
		head := fmt.Sprintf("  %s %s  %s %s  %s", s.Header.Render("▍"+kind), s.Title.Render(c.Name), state, s.Muted.Render(c.StateDetail), ready)
		out = append(out, head)
		restarts := fmt.Sprint(c.Restarts)
		if c.Restarts > 0 {
			restarts = s.WarnText.Render(restarts)
		}
		line := "    " + label("restarts") + restarts
		if c.LastTermination != "" {
			col := s.WarnText
			if strings.HasPrefix(c.LastTermination, "OOMKilled") {
				col = s.ErrText
			}
			line += s.Muted.Render("   last exit: ") + col.Render(c.LastTermination)
		}
		out = append(out, line, "    "+label("image")+s.Muted.Render(c.Image))
		if !c.Init {
			cpuUsed, memUsed, cpuPct, memPct := "?", "?", -1, -1
			cpuOf, memOf := "limit", "limit"
			if c.Usage != nil {
				cpuUsed, memUsed = fmt.Sprintf("%dm", c.Usage.CPUMilli), kube.FormatBytes(c.Usage.MemBytes)
				cpuPct, memPct = kube.Percent(c.Usage.CPUMilli, c.CPULim), kube.Percent(c.Usage.MemBytes, c.MemLim)
				// Without a limit, compare against the request instead.
				if cpuPct < 0 && c.CPUReq > 0 {
					cpuPct, cpuOf = kube.Percent(c.Usage.CPUMilli, c.CPUReq), "request"
				}
				if memPct < 0 && c.MemReq > 0 {
					memPct, memOf = kube.Percent(c.Usage.MemBytes, c.MemReq), "request"
				}
			}
			q := func(v int64, f func(int64) string) string {
				if v == 0 {
					return s.WarnText.Render("none")
				}
				return f(v)
			}
			milli := func(v int64) string { return fmt.Sprintf("%dm", v) }
			pct := func(p int, of string) string {
				if p < 0 {
					return s.Muted.Render(" no limit/request")
				}
				return fmt.Sprintf(" %3d%% of %-7s", p, of)
			}
			out = append(out,
				"    "+label("cpu")+a.meter(cpuPct, 20)+" "+s.Base.Render(fmt.Sprintf("%-7s", cpuUsed))+pct(cpuPct, cpuOf)+
					s.Muted.Render("   req ")+q(c.CPUReq, milli)+s.Muted.Render(" · limit ")+q(c.CPULim, milli),
				"    "+label("memory")+a.meter(memPct, 20)+" "+s.Base.Render(fmt.Sprintf("%-7s", memUsed))+pct(memPct, memOf)+
					s.Muted.Render("   req ")+q(c.MemReq, kube.FormatBytes)+s.Muted.Render(" · limit ")+q(c.MemLim, kube.FormatBytes))
		}
		if len(c.Probes) == 0 && !c.Init {
			out = append(out, "    "+label("probes")+s.WarnText.Render("none configured"))
		}
		for i, p := range c.Probes {
			l := "probes"
			if i > 0 {
				l = ""
			}
			out = append(out, "    "+label(l)+s.Key.Render(fmt.Sprintf("%-10s", p.Kind))+s.Base.Render(p.Detail))
		}
		for _, w := range c.Warnings {
			out = append(out, "    "+strings.Repeat(" ", 12)+s.WarnText.Render("▲ "+w))
		}
	}

	if len(h.Issues) > 0 {
		out = append(out, "", "  "+s.Title.Render("Detected issues"))
		for _, is := range h.Issues {
			icon := s.WarnText.Render("▲")
			if is.Severity == kube.HealthErr {
				icon = s.ErrText.Render("✗")
			}
			out = append(out, fmt.Sprintf("  %s %s  %s", icon, s.Base.Bold(true).Render(is.Reason), s.Base.Render(is.Message)))
			if is.Hint != "" {
				out = append(out, "    "+s.Muted.Render("→ "+is.Hint))
			}
		}
	}
	out = append(out, "", "  "+s.Title.Render("Recent warning events"))
	if len(h.Events) == 0 {
		out = append(out, "  "+s.OKText.Render("none"))
	}
	for _, e := range h.Events {
		out = append(out, "  "+s.WarnText.Render(e))
	}
	out = append(out, "", "  "+s.Muted.Render("a: ask AI about this pod · l: logs · d: describe · esc: back"))
	return out
}

// ---------- secrets ----------

type secretMsg struct {
	pane   *pane
	secret *corev1.Secret
	err    error
}

func (m secretMsg) targetPane() *pane { return m.pane }

const revealFor = 60 * time.Second

func fetchSecret(c *kube.Client, ns, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		s, err := c.SecretValues(ctx, ns, name)
		return secretMsg{secret: s, err: err}
	}
}

func (a *App) openSecret(ns, name string) tea.Cmd {
	a.stopLogs()
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode = modeText
	secRes, _ := kube.Lookup("secrets")
	a.target = targetRef{secRes, ns, name}
	a.reveal, a.secret = false, nil
	a.textKind, a.textTitle, a.textLines = "secret", "secret "+objName(ns, name), nil
	a.renderText(true)
	return tagPane(a.pane, fetchSecret(a.client, ns, name))
}

func (a *App) handleSecret(m secretMsg) {
	if a.mode != modeText || a.textKind != "secret" {
		return
	}
	if m.err != nil {
		a.textLines = []string{a.st.ErrText.Render("✗ " + m.err.Error())}
		a.renderText(true)
		return
	}
	a.secret = m.secret
	a.renderSecret(true)
}

// toggleReveal shows decoded values after confirmation; production-like contexts need the name typed.
func (a *App) toggleReveal() tea.Cmd {
	if a.reveal {
		a.maskSecret("masked")
		if a.textKind == "yaml" {
			return tagPane(a.pane, fetchText(a.client, a.target.res, a.target.ns, a.target.name, "yaml", false))
		}
		return nil
	}
	if a.secret == nil && a.textKind == "secret" {
		return nil
	}
	name := a.target.name
	reveal := func() tea.Cmd {
		a.reveal, a.revealedAt = true, time.Now()
		a.setStatus(fmt.Sprintf("secret %s unmasked on %s; it masks again in %ds or when you leave (x to mask now)", name, a.client.Context, int(revealFor.Seconds())), false)
		if a.textKind == "yaml" {
			return tagPane(a.pane, fetchText(a.client, a.target.res, a.target.ns, a.target.name, "yaml", true))
		}
		a.renderSecret(false)
		return nil
	}
	a.askConfirm(fmt.Sprintf("Show the decoded values of secret %s on %s?", objName(a.target.ns, name), a.client.Context), name, reveal)
	return nil
}

func (a *App) maskSecret(why string) {
	a.reveal = false
	switch a.textKind {
	case "secret":
		a.renderSecret(false)
	case "yaml":
		// re-render the masked manifest from scratch
		a.textLines = []string{a.st.Muted.Render("masking…")}
		a.renderText(false)
	}
	a.setStatus("secret "+why, false)
}

// autoMask re-masks revealed secrets after revealFor (checked on every tick, for all panes).
func (a *App) autoMask() []tea.Cmd {
	var cmds []tea.Cmd
	for _, p := range a.allPanes() {
		if p.reveal && time.Since(p.revealedAt) > revealFor {
			a.withPane(p, func() {
				a.maskSecret(fmt.Sprintf("masked again after %ds", int(revealFor.Seconds())))
				if a.textKind == "yaml" {
					cmds = append(cmds, tagPane(p, fetchText(p.client, p.target.res, p.target.ns, p.target.name, "yaml", false)))
				}
			})
		}
	}
	return cmds
}

func (a *App) renderSecret(top bool) {
	s := a.st
	sec := a.secret
	if sec == nil {
		return
	}
	label := func(l string) string { return s.YAMLKey.Render(fmt.Sprintf("%-10s", l)) }
	state := s.OKText.Render("● masked") + s.Muted.Render("  (x to unmask after confirmation)")
	if a.reveal {
		left := revealFor - time.Since(a.revealedAt)
		state = s.Danger.Render(" UNMASKED ") + s.WarnText.Render(fmt.Sprintf("  masks again in %ds · x to mask now", int(left.Seconds())))
	}
	out := []string{"",
		"  " + label("Secret") + s.Base.Render(objName(sec.Namespace, sec.Name)),
		"  " + label("Type") + s.Base.Render(string(sec.Type)) + s.Muted.Render(fmt.Sprintf(" · %d keys · age %s", len(sec.Data), kube.Age(sec.CreationTimestamp))),
		"  " + label("Values") + state, ""}
	if sec.Immutable != nil && *sec.Immutable {
		out = append(out[:4], append([]string{"  " + label("") + s.Muted.Render("immutable")}, out[4:]...)...)
	}
	keys := make([]string, 0, len(sec.Data))
	for k := range sec.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		out = append(out, "  "+s.Muted.Render("(no data)"))
	}
	for _, k := range keys {
		v := sec.Data[k]
		out = append(out, "  "+s.Title.Render(k)+s.Muted.Render(fmt.Sprintf("  %d bytes · %s", len(v), describeValue(v))))
		// Public certificate details are useful even while masked.
		for _, l := range certSummary(v) {
			out = append(out, "    "+l)
		}
		if !a.reveal {
			out = append(out, "    "+s.Muted.Render(strings.Repeat("•", min(24, max(8, len(v)/2)))))
			continue
		}
		for _, l := range strings.Split(prettyValue(v), "\n") {
			out = append(out, "    "+s.Code.Render(l))
		}
	}
	out = append(out, "", "  "+s.Muted.Render("Values are decoded from base64 locally. They are never sent to the AI or written to disk."))
	a.textLines = out
	a.renderText(top)
}

func describeValue(v []byte) string {
	switch {
	case bytes.Contains(v, []byte("-----BEGIN CERTIFICATE-----")):
		return "PEM certificate"
	case bytes.Contains(v, []byte("PRIVATE KEY-----")):
		return "PEM private key"
	case json.Valid(v) && len(bytes.TrimSpace(v)) > 0 && (v[0] == '{' || v[0] == '['):
		return "JSON"
	case !utf8.Valid(v):
		return "binary"
	case bytes.Contains(v, []byte("\n")):
		return "text, multi-line"
	}
	return "text"
}

func prettyValue(v []byte) string {
	if !utf8.Valid(v) {
		const n = 48
		h := fmt.Sprintf("% x", v[:min(n, len(v))])
		if len(v) > n {
			h += " …"
		}
		return "binary (hex): " + h
	}
	if json.Valid(v) && len(v) > 0 && (v[0] == '{' || v[0] == '[') {
		var buf bytes.Buffer
		if json.Indent(&buf, v, "", "  ") == nil {
			return buf.String()
		}
	}
	return strings.TrimRight(string(v), "\n")
}

// certSummary lists subject, DNS names and expiry of PEM certificates in v.
func certSummary(v []byte) []string {
	var out []string
	rest := v
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		days := int(time.Until(c.NotAfter).Hours() / 24)
		exp := fmt.Sprintf("expires %s (in %d days)", c.NotAfter.Format("2006-01-02"), days)
		switch {
		case days < 0:
			exp = fmt.Sprintf("✗ EXPIRED %s (%d days ago)", c.NotAfter.Format("2006-01-02"), -days)
		case days < 30:
			exp = "▲ " + exp
		}
		line := fmt.Sprintf("cert: %s · %s", c.Subject.CommonName, exp)
		if len(c.DNSNames) > 0 {
			line += " · DNS " + strings.Join(c.DNSNames[:min(3, len(c.DNSNames))], ",")
		}
		out = append(out, line)
	}
	return out
}
