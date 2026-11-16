package ui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// openIncident collects everything unhealthy in the scope and asks the AI for a root cause
// and a fix plan; commands it proposes go through the normal approval flow (key I / :incident).
func (a *App) openIncident() tea.Cmd {
	target := a.client
	a.openAI()
	if a.provider == nil {
		a.chat = append(a.chat, chatEntry{role: "error", display: fmt.Sprint(a.providerErr)})
		a.renderChat()
		return nil
	}
	a.stopAI()
	a.ag, a.aiClient, a.aiBusy = agentState{}, target, true
	a.chat = append(a.chat, chatEntry{role: "user", display: fmt.Sprintf("🚨 Incident mode: collecting everything unhealthy in %s on %s…", target.NamespaceLabel(), target.Context)})
	a.renderChat()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*timeout)
		defer cancel()
		rep, err := target.Incident(ctx)
		if err != nil {
			return aiPromptMsg{err: err}
		}
		if rep.Empty() {
			return aiPromptMsg{err: fmt.Errorf("✓ nothing unhealthy found in %s: no failing pods, warning events, failed Helm releases, pending PVCs or node problems", rep.Scope)}
		}
		prompt := fmt.Sprintf(`Incident triage. Cluster (kube-context): %s, scope: %s.
Everything unhealthy kubesage found:

%s
Work like an on-call SRE:
1. Group the symptoms into incidents and name the most likely ROOT CAUSE of each (separate causes from symptoms;
   a recent rollout just before failures started is a strong signal).
2. Rank them by impact.
3. For each, gather any missing evidence with read-only commands, then propose the smallest fix.
   Propose changes one at a time; every change needs the user's approval.
4. End with a short summary: what is broken, why, what to do now, and what to watch afterwards.`, target.Context, rep.Scope, rep.Format())
		return aiPromptMsg{display: fmt.Sprintf("Triage %d issues, %d warning events, %d other problems in %s", len(rep.Issues), len(rep.Warnings),
			len(rep.FailedHelm)+len(rep.PendingPVCs)+len(rep.NotReadyNodes), rep.Scope), prompt: prompt}
	}
}
