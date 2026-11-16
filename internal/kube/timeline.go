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

// TimelineEntry is one thing that happened to a workload or its parts.
type TimelineEntry struct {
	Time   time.Time
	Source string // event, rollout, restart, pod, scale
	Object string // Kind/name
	Health Health
	Text   string
	Count  int32
}

func walk(n *GraphNode, fn func(*GraphNode)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		walk(c, fn)
	}
}

// Timeline merges events, rollouts, container restarts and pod starts for an object and its parts, newest first.
func (c *Client) Timeline(ctx context.Context, kind, ns, name string) ([]TimelineEntry, error) {
	root, _, err := c.Relations(ctx, kind, ns, name)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	walk(root, func(n *GraphNode) {
		if n.Kind == "HPA" {
			names[strings.Fields(n.Name)[0]] = true
			return
		}
		names[n.Name] = true
	})
	var out []TimelineEntry
	evs, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, e := range evs.Items {
			if !names[e.InvolvedObject.Name] {
				continue
			}
			t := e.LastTimestamp.Time
			if t.IsZero() {
				t = e.EventTime.Time
			}
			if t.IsZero() {
				t = e.CreationTimestamp.Time
			}
			h := HealthNeutral
			if e.Type == corev1.EventTypeWarning {
				h = HealthWarn
				if strings.Contains(e.Reason, "Fail") || strings.Contains(e.Reason, "BackOff") || e.Reason == "OOMKilling" || e.Reason == "Unhealthy" {
					h = HealthErr
				}
			}
			src := "event"
			if e.InvolvedObject.Kind == "HorizontalPodAutoscaler" || e.Reason == "ScalingReplicaSet" {
				src = "scale"
			}
			out = append(out, TimelineEntry{Time: t, Source: src, Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name, Health: h,
				Text: e.Reason + ": " + firstLine(e.Message), Count: e.Count})
		}
	}
	if kind == "Deployment" || kind == "ReplicaSet" || kind == "Pod" || kind == "Service" || kind == "Ingress" {
		rss, err := c.Clientset.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, rs := range rss.Items {
				if !names[rs.Name] {
					continue
				}
				imgs := []string{}
				for _, ct := range rs.Spec.Template.Spec.Containers {
					imgs = append(imgs, ct.Image)
				}
				out = append(out, TimelineEntry{Time: rs.CreationTimestamp.Time, Source: "rollout", Object: "ReplicaSet/" + rs.Name, Health: HealthOK,
					Text: fmt.Sprintf("revision %s created · %s", rs.Annotations["deployment.kubernetes.io/revision"], strings.Join(imgs, ", "))})
			}
		}
	}
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, p := range pods.Items {
			if !names[p.Name] {
				continue
			}
			out = append(out, TimelineEntry{Time: p.CreationTimestamp.Time, Source: "pod", Object: "Pod/" + p.Name, Health: HealthNeutral, Text: "pod created"})
			for _, s := range p.Status.ContainerStatuses {
				if t := s.LastTerminationState.Terminated; t != nil {
					h := HealthErr
					if t.ExitCode == 0 {
						h = HealthWarn
					}
					out = append(out, TimelineEntry{Time: t.FinishedAt.Time, Source: "restart", Object: "Pod/" + p.Name, Health: h,
						Text: fmt.Sprintf("container %s terminated: %s (exit %d) · %d restarts total", s.Name, t.Reason, t.ExitCode, s.RestartCount)})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}
