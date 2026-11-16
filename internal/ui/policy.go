package ui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"sigs.k8s.io/yaml"

	"github.com/gauravthakur02/kubesage/internal/config"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

const policyRes = "policy"

func (a *App) policyOptions() kube.PolicyOptions { return PolicyOptionsFor(a.cfg) }

// PolicyOptionsFor builds the policy checks (built-in and custom rules) from the config.
func PolicyOptionsFor(cfg *config.Config) kube.PolicyOptions {
	o := kube.PolicyOptions{AllowedRegistries: cfg.Policy.AllowedRegistries, SkipNamespaces: cfg.Policy.SkipNamespaces}
	for _, r := range cfg.Rules {
		o.Rules = append(o.Rules, kube.CustomRule{Name: r.Name, Kinds: r.Kinds, Namespaces: r.Namespaces, Severity: r.Severity,
			RequireLabels: r.RequireLabels, RequireAnnotations: r.RequireAnnotations, Path: r.Path, Match: r.Match, Forbid: r.Forbid, Message: r.Message})
	}
	return o
}

// policyResource lists policy/security findings for the focused namespace (or all).
func (a *App) policyResource() kube.Resource {
	c, opts := a.client, a.policyOptions()
	return kube.VirtualResource(policyRes, "Finding", "Policy", func() kube.Table {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		t := kube.Table{Columns: []string{"SEV", "CHECK", "WORKLOAD", "CONTAINER", "FINDING", "FIX"}}
		fs, err := c.PolicyScan(ctx, opts)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		errs, warns, workloads := 0, 0, map[string]bool{}
		for _, f := range fs {
			sev := "info"
			switch f.Severity {
			case kube.HealthErr:
				sev = "✗ error"
				errs++
			case kube.HealthWarn:
				sev = "▲ warn"
				warns++
			}
			workloads[f.Namespace+"/"+f.Kind+"/"+f.Workload] = true
			wl := strings.TrimSuffix(f.Kind, "s") + "/" + f.Workload
			if c.Namespace == kube.AllNamespaces {
				wl = f.Namespace + "/" + wl
			}
			t.Rows = append(t.Rows, kube.Row{Namespace: f.Namespace, Name: f.Kind + "/" + f.Workload, Health: f.Severity,
				Cells: []string{sev, f.Check, wl, f.Container, f.Message, f.Hint}})
		}
		t.Note = fmt.Sprintf("%d errors · %d warnings · %d workloads", errs, warns, len(workloads))
		if len(fs) == 0 {
			t.Note = "✓ no findings"
		}
		return t
	})
}

// openPolicy shows the policy findings in the focused pane (esc returns).
func (a *App) openPolicy() tea.Cmd {
	back := a.res
	cmd := a.switchResource(a.policyResource())
	if back.Name != policyRes {
		a.backTo = &back
	}
	a.setStatus("enter: open workload YAML · a: ask AI for a fix · e: safe edit the workload · / filter (e.g. 'root')", false)
	return cmd
}

func policyTarget(row kube.Row) (kube.Resource, string, bool) {
	kind, name, ok := strings.Cut(row.Name, "/")
	if !ok {
		return kube.Resource{}, "", false
	}
	r, ok := kube.Lookup(kind)
	return r, name, ok
}

// policyRowAction handles keys on a finding: open the workload, edit it, or ask the AI for a fix.
func (a *App) policyRowAction(key string, row kube.Row) (tea.Cmd, bool) {
	r, name, ok := policyTarget(row)
	if !ok {
		return nil, false
	}
	switch key {
	case "enter", "y":
		return a.openText(r, row.Namespace, name, "yaml"), true
	case "e":
		return a.openEdit(r, row.Namespace, name), true
	case "a":
		return a.aiFixPolicy(r, row.Namespace, name), true
	}
	return nil, false
}

// aiFixPolicy asks the AI for a minimal patch fixing all findings of one workload.
func (a *App) aiFixPolicy(r kube.Resource, ns, name string) tea.Cmd {
	target, opts := a.client, a.policyOptions()
	a.openAI()
	if a.provider == nil {
		a.chat = append(a.chat, chatEntry{role: "error", display: fmt.Sprint(a.providerErr)})
		a.renderChat()
		return nil
	}
	a.stopAI()
	a.ag, a.aiClient, a.aiBusy = agentState{}, target, true
	a.chat = append(a.chat, chatEntry{role: "user", display: fmt.Sprintf("Collecting policy findings and manifest for %s %s…", strings.ToLower(r.Kind), objName(ns, name))})
	a.renderChat()
	redact := a.cfg.AI.Redact
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*timeout)
		defer cancel()
		scoped := target.WithNamespace(ns)
		all, err := scoped.PolicyScan(ctx, opts)
		if err != nil {
			return aiPromptMsg{err: err}
		}
		var mine []kube.Finding
		for _, f := range all {
			if f.Kind == r.Name && f.Workload == name {
				mine = append(mine, f)
			}
		}
		manifest := "(manifest unavailable)\n"
		if obj, err := target.Get(ctx, r, ns, name); err == nil {
			y, _ := yaml.Marshal(kube.Clean(obj, redact, false))
			manifest = string(y)
		}
		prompt := fmt.Sprintf(`Cluster (kube-context): %s
These policy/security findings were reported for %s %s/%s:

%s
Current manifest:
`+"```yaml\n%s```"+`

Propose the smallest change that fixes them without breaking the app. Explain any finding that should NOT be fixed
(e.g. an agent that really needs host access). Give the result as a strategic-merge patch (YAML) and the exact
kubectl patch command. Do not apply anything.`, target.Context, r.Kind, ns, name, kube.FormatFindings(mine), manifest)
		return aiPromptMsg{display: fmt.Sprintf("Fix %d policy findings in %s %s", len(mine), strings.ToLower(r.Kind), objName(ns, name)), prompt: prompt}
	}
}
