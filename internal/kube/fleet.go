package kube

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FleetStat is a one-line health summary of one cluster (in the client's namespace scope).
type FleetStat struct {
	Version        string
	Latency        time.Duration
	Nodes, Ready   int // -1 when nodes cannot be listed
	Pods, Running  int
	Issues, Errors int
	Warnings       int // warning events currently held by the API server
}

// Fleet summarises the cluster: version, nodes, pods, detected issues and warning events.
func (c *Client) Fleet(ctx context.Context) (FleetStat, error) {
	var s FleetStat
	start := time.Now()
	v, err := c.ServerVersion()
	if err != nil {
		return s, err
	}
	s.Version, s.Latency = v, time.Since(start)
	s.Nodes, s.Ready = -1, -1
	if nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		s.Nodes, s.Ready = len(nodes.Items), 0
		for _, n := range nodes.Items {
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
					s.Ready++
				}
			}
		}
	}
	if pods, err := c.listPodsAcross(ctx, c.Namespace); err == nil {
		s.Pods = len(pods)
		for i := range pods { // "running" means really working: all containers ready (or a finished job)
			if pods[i].Status.Phase == corev1.PodSucceeded || (pods[i].Status.Phase == corev1.PodRunning && podReady(&pods[i])) {
				s.Running++
			}
		}
	}
	if issues, err := c.Scan(ctx); err == nil {
		s.Issues = len(issues)
		for _, is := range issues {
			if is.Severity == HealthErr {
				s.Errors++
			}
		}
	}
	if evs, err := c.CollectEvents(ctx); err == nil {
		for _, e := range evs {
			if e.Type == corev1.EventTypeWarning {
				s.Warnings++
			}
		}
	}
	return s, nil
}

// Health grades the summary.
func (s FleetStat) Health() Health {
	switch {
	case s.Errors > 0 || (s.Nodes > 0 && s.Ready < s.Nodes):
		return HealthErr
	case s.Issues > 0 || s.Running < s.Pods:
		return HealthWarn
	}
	return HealthOK
}
