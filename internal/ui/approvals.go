package ui

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/kube"
	"github.com/gauravthakur02/kubeweave/internal/report"
)

const approvalsRes = "approvals"

func (a *App) approvalsNS() string {
	if a.cfg.Approvals.Namespace != "" {
		return a.cfg.Approvals.Namespace
	}
	return "kubeweave-approvals"
}

func (a *App) approvalExpiry() time.Duration {
	if a.cfg.Approvals.ExpireMinutes > 0 {
		return time.Duration(a.cfg.Approvals.ExpireMinutes) * time.Minute
	}
	return time.Hour
}

// needsApproval reports whether changes on this context go through the two-person rule.
func (a *App) needsApproval() bool {
	re := a.cfg.Approvals.Contexts
	if re == "" || a.runApproved {
		return false
	}
	m, err := regexp.MatchString(re, a.client.Context)
	return err == nil && m
}

type approvalRequestedMsg struct {
	ap  kube.Approval
	err error
}

type approvalsPolledMsg struct {
	list []kube.Approval
	err  error
}

// requestApproval replaces a confirmed change by an approval request; the change itself waits in this session.
func (a *App) requestApproval(question string, fn func() tea.Cmd) {
	c, ns, target := a.client, a.client.Namespace, a.target.name
	reason := a.changeReason
	a.ov, a.confirm, a.inputHint = ovConfirm, "Changes on "+c.Context+" need a second person's approval. Request approval for: "+question+" (y/n)", ""
	a.onConfirm = func() tea.Cmd {
		hook := os.Getenv(a.cfg.Reports.WebhookEnv)
		approvalsNS := a.approvalsNS()
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			ap, err := c.RequestApproval(ctx, approvalsNS, kube.Approval{Context: c.Context, Namespace: ns, Target: target, Action: question, Reason: reason})
			if err == nil && hook != "" {
				_ = report.Post(ctx, hook, fmt.Sprintf("kubeweave: %s asks to %q on %s. Approve in kubeweave (:approvals) or run: kubeweave approve %s --context %s",
					ap.Requester, question, c.Context, ap.ID, c.Context))
			}
			if err == nil {
				ap.Action = question
			}
			return approvalRequestedMsg{ap: ap, err: err}
		}
	}
	if a.awaitingFn == nil {
		a.awaitingFn = map[string]func() tea.Cmd{}
	}
	a.pendingFn = fn
}

func (a *App) handleApprovalRequested(m approvalRequestedMsg) tea.Cmd {
	if m.err != nil {
		a.pendingFn = nil
		a.setStatus("approval request failed: "+m.err.Error(), true)
		return nil
	}
	a.awaitingFn[m.ap.ID] = a.pendingFn
	a.pendingFn = nil
	a.audit("approval requested: "+m.ap.Action, m.ap.Context, m.ap.Namespace, m.ap.ID, nil)
	a.setStatus("⏳ approval requested ("+m.ap.ID+"): a teammate approves in :approvals or with `kubeweave approve`", false)
	return nil
}

// pollApprovals checks the state of this session's requests (every 15s while any wait).
func (a *App) pollApprovals() tea.Cmd {
	if len(a.awaitingFn) == 0 || time.Since(a.approvalsAt) < 15*time.Second {
		return nil
	}
	a.approvalsAt = time.Now()
	c, ns, exp := a.client, a.approvalsNS(), a.approvalExpiry()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		l, err := c.Approvals(ctx, ns, exp)
		return approvalsPolledMsg{list: l, err: err}
	}
}

func (a *App) handleApprovalsPolled(m approvalsPolledMsg) tea.Cmd {
	if m.err != nil {
		return nil
	}
	for _, ap := range m.list {
		if _, mine := a.awaitingFn[ap.ID]; !mine {
			continue
		}
		switch ap.Status {
		case "approved":
			if !a.notified[ap.ID] {
				if a.notified == nil {
					a.notified = map[string]bool{}
				}
				a.notified[ap.ID] = true
				a.setStatus("✓ "+ap.Approver+" approved \""+ap.Action+"\" · :approvals → enter runs it", false)
				fmt.Fprint(os.Stderr, "\a")
			}
		case "rejected", "expired", "done":
			delete(a.awaitingFn, ap.ID)
			if ap.Status != "done" {
				a.setStatus("✗ request "+ap.ID+" was "+ap.Status, true)
			}
		}
	}
	if a.res.Name == approvalsRes && a.mode == modeTable {
		return a.refresh()
	}
	return nil
}

func (a *App) approvalsResource() kube.Resource {
	c, ns, exp, mine := a.client, a.approvalsNS(), a.approvalExpiry(), a.awaitingFn
	return kube.VirtualResource(approvalsRes, "Approval", "Approvals", func() kube.Table {
		t := kube.Table{Columns: []string{"STATUS", "ACTION", "REQUESTER", "APPROVER", "CONTEXT", "AGE", "ID"}}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		l, err := c.Approvals(ctx, ns, exp)
		if err != nil {
			t.Note = "✗ " + err.Error()
			return t
		}
		pending := 0
		for _, ap := range l {
			h := kube.HealthNeutral
			status := ap.Status
			switch ap.Status {
			case "pending":
				h, pending = kube.HealthWarn, pending+1
			case "approved":
				h = kube.HealthOK
				if _, ok := mine[ap.ID]; ok {
					status = "approved · enter runs it"
				}
			case "rejected":
				h = kube.HealthErr
			}
			t.Rows = append(t.Rows, kube.Row{Name: ap.ID, Health: h, Cells: []string{status, ap.Action, ap.Requester, ap.Approver, ap.Context,
				kube.Age(metav1.NewTime(ap.Created)), ap.ID}})
		}
		t.Note = fmt.Sprintf("%d pending · requests live in namespace %s", pending, ns)
		return t
	})
}

func (a *App) openApprovals() tea.Cmd {
	return a.openVirtual(a.approvalsResource(), "y approve · n reject · enter run your approved change")
}

func (a *App) decide(row kube.Row, approve bool) tea.Cmd {
	c, ns, exp := a.client, a.approvalsNS(), a.approvalExpiry()
	verb := "reject"
	if approve {
		verb = "approve"
	}
	a.askConfirmPlain(fmt.Sprintf("%s request %s? (y/n)", verb, row.Name), func() tea.Cmd {
		return runAction(verb+" "+row.Name, func(ctx context.Context) error {
			_, err := c.DecideApproval(ctx, ns, row.Name, approve, exp)
			return err
		})
	})
	return nil
}

// runApprovedChange runs the change this session asked approval for, after one last confirmation.
func (a *App) runApprovedChange(row kube.Row) tea.Cmd {
	fn, ok := a.awaitingFn[row.Name]
	if !ok || len(row.Cells) == 0 || row.Cells[0] != "approved · enter runs it" {
		a.setStatus("only your own approved requests can be run from here", true)
		return nil
	}
	c, ns, id := a.client, a.approvalsNS(), row.Name
	a.askConfirmPlain("Run the approved change \""+row.Cells[1]+"\" now? (y/n)", func() tea.Cmd {
		delete(a.awaitingFn, id)
		a.runApproved = true
		cmd := fn()
		a.runApproved = false
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_ = c.CloseApproval(ctx, ns, id, "run")
		}()
		return cmd
	})
	return nil
}

// askConfirmPlain asks y/n without the approval rule or reasons (approval bookkeeping itself).
func (a *App) askConfirmPlain(question string, fn func() tea.Cmd) {
	a.ov, a.confirm, a.onConfirm, a.inputHint = ovConfirm, question, fn, ""
}

func init() {
	registerScreen(approvalsRes, screenDef{
		title: func(a *App) string { return "Approvals · " + a.client.Context + " · " + a.table.Note },
		hints: []kv{{"y", "approve"}, {"n", "reject"}, {"enter", "run approved"}, {"r", "refresh"}, {"esc", "back"}},
		action: func(a *App, key string, row kube.Row) (tea.Cmd, bool) {
			switch key {
			case "y":
				return a.decide(row, true), true
			case "n":
				return a.decide(row, false), true
			case "enter":
				return a.runApprovedChange(row), true
			}
			return nil, false
		},
	})
}
