package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IncidentReport is everything unhealthy in a namespace (or the cluster), for triage.
type IncidentReport struct {
	Scope          string
	Issues         []Issue
	Warnings       []string // recent warning events, aggregated
	FailedHelm     []string
	PendingPVCs    []string
	NotReadyNodes  []string
	RecentRollouts []string
}

// Empty reports whether nothing unhealthy was found.
func (r *IncidentReport) Empty() bool {
	return len(r.Issues)+len(r.Warnings)+len(r.FailedHelm)+len(r.PendingPVCs)+len(r.NotReadyNodes) == 0
}

// Incident collects issues, warning events (last hour), failed Helm releases, pending PVCs,
// not-ready nodes and rollouts of the last two hours.
func (c *Client) Incident(ctx context.Context) (*IncidentReport, error) {
	r := &IncidentReport{Scope: c.NamespaceLabel()}
	issues, err := c.Scan(ctx)
	if err != nil {
		return nil, err
	}
	r.Issues = issues
	if evs, err := c.listWarningEventsAcross(ctx, c.Namespace); err == nil {
		type agg struct {
			n    int32
			last time.Time
			msg  string
		}
		by := map[string]*agg{}
		for _, e := range evs {
			t := e.LastTimestamp.Time
			if t.IsZero() {
				t = e.EventTime.Time
			}
			if time.Since(t) > time.Hour {
				continue
			}
			k := fmt.Sprintf("%s %s/%s/%s", e.Reason, e.InvolvedObject.Kind, e.Namespace, e.InvolvedObject.Name)
			g := by[k]
			if g == nil {
				g = &agg{}
				by[k] = g
			}
			g.n += max(e.Count, 1)
			if t.After(g.last) {
				g.last, g.msg = t, firstLine(e.Message)
			}
		}
		keys := make([]string, 0, len(by))
		for k := range by {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return by[keys[i]].last.After(by[keys[j]].last) })
		for i, k := range keys {
			if i >= 40 {
				break
			}
			g := by[k]
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s ×%d (last %s ago): %s", k, g.n, Age(metav1.NewTime(g.last)), truncate(g.msg, 200)))
		}
	}
	if rels, err := c.HelmReleases(ctx, c.Namespace); err == nil {
		for _, h := range rels {
			if h.Status != "deployed" && h.Status != "superseded" && h.Status != "uninstalled" {
				r.FailedHelm = append(r.FailedHelm, fmt.Sprintf("%s/%s rev %d %s: %s", h.Namespace, h.Name, h.Revision, h.Status, truncate(h.Description, 200)))
			}
		}
	}
	if pvcs, err := c.listPVCsAcross(ctx, c.Namespace); err == nil {
		for _, p := range pvcs {
			if p.Status.Phase != corev1.ClaimBound {
				sc := ""
				if p.Spec.StorageClassName != nil {
					sc = *p.Spec.StorageClassName
				}
				r.PendingPVCs = append(r.PendingPVCs, fmt.Sprintf("%s/%s %s (storageclass %s)", p.Namespace, p.Name, p.Status.Phase, sc))
			}
		}
	}
	if nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for _, n := range nodes.Items {
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
					r.NotReadyNodes = append(r.NotReadyNodes, n.Name+": "+cond.Reason)
				}
				if cond.Type != corev1.NodeReady && cond.Status == corev1.ConditionTrue {
					r.NotReadyNodes = append(r.NotReadyNodes, fmt.Sprintf("%s: %s", n.Name, cond.Type))
				}
			}
		}
	}
	if rss, err := listAcross(ctx, c, c.Namespace, func(ns string) ([]string, error) {
		l, err := c.Clientset.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		var out []string
		for _, rs := range l.Items {
			if time.Since(rs.CreationTimestamp.Time) < 2*time.Hour && len(rs.OwnerReferences) > 0 {
				imgs := []string{}
				for _, ct := range rs.Spec.Template.Spec.Containers {
					imgs = append(imgs, ct.Image)
				}
				out = append(out, fmt.Sprintf("%s ago: %s/%s rev %s → %s", Age(rs.CreationTimestamp), rs.Namespace, rs.OwnerReferences[0].Name,
					rs.Annotations["deployment.kubernetes.io/revision"], strings.Join(imgs, ", ")))
			}
		}
		return out, nil
	}); err == nil {
		r.RecentRollouts = rss
	}
	return r, nil
}

// Format renders the report as plain text (for the AI prompt and the screen).
func (r *IncidentReport) Format() string {
	var b strings.Builder
	sec := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s (%d)\n", title, len(items))
		for _, it := range items {
			b.WriteString("- " + it + "\n")
		}
		b.WriteString("\n")
	}
	if len(r.Issues) > 0 {
		fmt.Fprintf(&b, "## Detected issues (%d)\n%s\n", len(r.Issues), FormatIssues(r.Issues))
	}
	sec("Warning events, last hour", r.Warnings)
	sec("Helm releases not deployed", r.FailedHelm)
	sec("PVCs not bound", r.PendingPVCs)
	sec("Node problems", r.NotReadyNodes)
	sec("Rollouts in the last 2 hours", r.RecentRollouts)
	if b.Len() == 0 {
		return "Nothing unhealthy found.\n"
	}
	return b.String()
}
