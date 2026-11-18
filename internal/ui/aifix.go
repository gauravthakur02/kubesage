package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/gauravthakur02/kubeweave/internal/ai"
	"github.com/gauravthakur02/kubeweave/internal/kube"
)

// ---------- guard: local-only contexts ----------

// aiBlocked says why AI may not be used for a cluster ("" = allowed). Contexts matching ai.local_only_contexts
// only work with a local model, so cluster data never leaves this computer.
func (a *App) aiBlocked(kctx string) string {
	if a.provider == nil {
		return fmt.Sprint(a.providerErr)
	}
	re := a.cfg.AI.LocalOnlyContexts
	if re == "" {
		return ""
	}
	p := a.cfg.AI.Profiles[a.profile]
	if m, err := regexp.MatchString(re, kctx); err == nil && m && !ai.IsLocal(p.Provider, p.BaseURL) {
		return fmt.Sprintf("%s is local-only (ai.local_only_contexts): switch to a local AI profile with ctrl+o", kctx)
	}
	return ""
}

// ---------- cost hint ----------

func (a *App) countTokens(in, out string) {
	a.aiTokIn += ai.EstimateTokens(in)
	a.aiTokOut += ai.EstimateTokens(out)
}

// aiUsage is "~12k tokens · ~$0.04" for the AI pane title (cost only when the profile has prices).
func (a *App) aiUsage() string {
	if a.aiTokIn+a.aiTokOut == 0 {
		return ""
	}
	s := "~" + shortCount(a.aiTokIn+a.aiTokOut) + " tokens"
	p := a.cfg.AI.Profiles[a.profile]
	if p.PriceInPerMTok > 0 || p.PriceOutPerMTok > 0 {
		cost := float64(a.aiTokIn)/1e6*p.PriceInPerMTok + float64(a.aiTokOut)/1e6*p.PriceOutPerMTok
		s += fmt.Sprintf(" · ~$%.3f", cost)
	}
	return s
}

func shortCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return strconv.Itoa(n)
}

// ---------- AI fix with preview ----------

type aiFixMsg struct {
	edit *editState
	note string
	err  error
}

// openAIFix asks the AI for the smallest patch fixing the selected object, then shows it like a safe edit:
// server-side dry-run and diff first, applied only with A and a confirmation (:fix, or space → AI fix).
func (a *App) openAIFix() tea.Cmd {
	row, ok := a.selected()
	if a.mode != modeTable || !ok || !a.res.Watchable() {
		a.setStatus("select a Kubernetes object first (:fix works on deployments, statefulsets, pods, services, …)", true)
		return nil
	}
	if a.res.Name == "secrets" {
		a.setStatus("secrets are never sent to the AI", true)
		return nil
	}
	c, r, ns, name := a.client, a.res, row.Namespace, row.Name
	if why := a.aiBlocked(c.Context); why != "" {
		a.setStatus(why, true)
		return nil
	}
	p, redact, logLines := a.provider, a.cfg.AI.Redact, a.cfg.AI.LogLines
	a.setStatus("AI is preparing a fix for "+objName(ns, name)+"… (nothing is changed until you apply it)", false)
	return tagPane(a.pane, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		original, err := c.EditableYAML(ctx, r, ns, name)
		if err != nil {
			return aiFixMsg{err: err}
		}
		obj, err := c.Get(ctx, r, ns, name)
		if err != nil {
			return aiFixMsg{err: err}
		}
		clean := kube.Clean(obj, redact, false)
		delete(clean, "status")
		shown, _ := yaml.Marshal(clean)
		problems, _ := c.GatherContext(ctx, r, ns, name, redact, logLines/2)
		prompt := ai.FixPrompt(r.Kind, ns, name, string(shown), problems)
		reply, err := ai.Complete(ctx, p, ai.SystemPrompt, prompt)
		if err != nil {
			return aiFixMsg{err: err}
		}
		if strings.Contains(strings.ToUpper(reply), "NO CHANGE") {
			return aiFixMsg{note: "AI: no change needed · " + firstWords(reply, 30)}
		}
		patch, ok := ai.ExtractBlock(reply, "yaml", "yml", "")
		if !ok {
			return aiFixMsg{err: fmt.Errorf("the AI did not return a YAML patch")}
		}
		gvk := schema.GroupVersionKind{Group: r.GVR.Group, Version: r.GVR.Version, Kind: r.Kind}
		edited, err := kube.MergePatchLocally(gvk, original, patch)
		if err != nil {
			return aiFixMsg{err: err}
		}
		why := ""
		if i := strings.Index(reply, "Why:"); i >= 0 {
			why = strings.TrimSpace(reply[i:])
		}
		f, err := os.CreateTemp("", "kubeweave-aifix-*.yaml")
		if err != nil {
			return aiFixMsg{err: err}
		}
		f.WriteString(edited)
		f.Close()
		return aiFixMsg{edit: &editState{res: r, ns: ns, name: name, path: f.Name(), original: original, edited: edited,
			why: why, patch: patch, tokens: ai.EstimateTokens(prompt) + ai.EstimateTokens(reply)}}
	})
}

func (a *App) handleAIFix(m aiFixMsg) tea.Cmd {
	switch {
	case m.err != nil:
		a.setStatus("AI fix: "+m.err.Error(), true)
		return nil
	case m.edit == nil:
		a.setStatus(m.note, false)
		return nil
	}
	a.aiTokIn += m.edit.tokens
	a.endEdit()
	a.edit = m.edit
	if a.mode != modeText {
		a.prevMode = a.mode
	}
	a.mode, a.textKind = modeText, "editdiff"
	a.edit.checking = true
	a.renderEdit()
	e, c := a.edit, a.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res, err := c.UpdateFromYAML(ctx, e.res, e.ns, e.name, e.edited, true)
		return dryRunMsg{res: res, err: err}
	}
}

// ---------- incident narrative ----------

// openNarrative turns the timeline (with local history) of the selected object into a plain-English account (:narrative).
func (a *App) openNarrative() tea.Cmd {
	row, ok := a.selected()
	if a.mode != modeTable || !ok || !graphKinds[a.res.Kind] {
		a.setStatus("select a deployment, statefulset, pod, service or ingress first", true)
		return nil
	}
	c, kind, ns, name := a.client, a.res.Kind, row.Namespace, row.Name
	if why := a.aiBlocked(c.Context); why != "" {
		a.setStatus(why, true)
		return nil
	}
	p, history := a.provider, a.storedHistory()
	a.target = targetRef{a.res, ns, name}
	return a.openReport("narrative", "AI is writing the incident narrative…", func(context.Context) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		tl, err := c.TimelineWith(ctx, kind, ns, name, history)
		if err != nil {
			return "", "", err
		}
		var b strings.Builder
		for i, e := range tl {
			if i == 300 {
				break
			}
			fmt.Fprintf(&b, "%s  %-7s %s  %s", e.Time.Format("2006-01-02 15:04:05"), e.Source, e.Object, e.Text)
			if e.Count > 1 {
				fmt.Fprintf(&b, " (x%d)", e.Count)
			}
			b.WriteString("\n")
		}
		if b.Len() == 0 {
			return "", "", fmt.Errorf("nothing in the timeline yet")
		}
		obj := strings.ToLower(kind) + " " + objName(ns, name) + " on " + c.Context
		reply, err := ai.Complete(ctx, p, ai.SystemPrompt, ai.NarrativePrompt(obj, b.String()))
		if err != nil {
			return "", "", err
		}
		return "narrative " + objName(ns, name) + " · " + fmt.Sprint(len(tl)) + " timeline entries · :report saves Markdown", reply, nil
	})
}

// ---------- natural-language search ----------

type nlSearchMsg struct {
	spec nlSpec
	err  error
}

type nlSpec struct {
	Resource      string `json:"resource"`
	Namespace     string `json:"namespace"`
	Filter        string `json:"filter"`
	LabelSelector string `json:"label_selector"`
	MinRestarts   int    `json:"min_restarts"`
	Status        string `json:"status"`
	Explain       string `json:"explain"`
}

// openNLSearch asks the AI to turn a question into a resource, namespace and filter (:q <question>).
func (a *App) openNLSearch(q string) tea.Cmd {
	if strings.TrimSpace(q) == "" {
		a.setStatus("usage: :nl pods restarting more than 3 times in shop", true)
		return nil
	}
	if why := a.aiBlocked(a.client.Context); why != "" {
		a.setStatus(why, true)
		return nil
	}
	var names []string
	for _, r := range kube.Resources() {
		names = append(names, r.Name)
	}
	p := a.provider
	a.setStatus("AI is turning your question into a filter…", false)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		reply, err := ai.Complete(ctx, p, "You translate requests into JSON. Reply with JSON only.", ai.SearchPrompt(q, strings.Join(names, ", ")))
		if err != nil {
			return nlSearchMsg{err: err}
		}
		body, ok := ai.ExtractBlock(reply, "json", "")
		if !ok {
			body = reply
		}
		var spec nlSpec
		if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &spec); err != nil {
			return nlSearchMsg{err: fmt.Errorf("the AI answer was not valid JSON")}
		}
		return nlSearchMsg{spec: spec}
	}
}

func (a *App) handleNLSearch(m nlSearchMsg) tea.Cmd {
	if m.err != nil {
		a.setStatus("search: "+m.err.Error(), true)
		return nil
	}
	s := m.spec
	r, ok := a.lookupResource(s.Resource)
	if !ok {
		a.setStatus("search: unknown resource "+s.Resource, true)
		return nil
	}
	if r.Namespaced && s.Namespace != "" {
		ns := s.Namespace
		if ns == "all" {
			ns = kube.AllNamespaces
		}
		a.client = a.client.WithNamespace(ns)
	}
	cmd := a.switchResource(r)
	a.filter = strings.TrimSpace(s.Status + " " + s.Filter)
	if s.LabelSelector != "" {
		a.filter = strings.TrimSpace(a.filter + " " + s.LabelSelector)
	}
	a.minRestarts = s.MinRestarts
	note := s.Explain
	if a.minRestarts > 0 {
		note += fmt.Sprintf(" · restarts ≥ %d", a.minRestarts)
	}
	a.setStatus("🔎 "+note+" · esc clears", false)
	return cmd
}

// rowRestarts reads the RESTARTS column ("7" or "7 (3m ago)").
func (a *App) rowRestarts(r kube.Row) int {
	for i, c := range a.table.Columns {
		if c == "RESTARTS" && i < len(r.Cells) {
			n, _ := strconv.Atoi(strings.Fields(r.Cells[i] + " 0")[0])
			return n
		}
	}
	return 0
}
