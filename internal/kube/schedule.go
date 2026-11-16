package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ScaledDownAnnotation remembers the replica count a workload had before a schedule scaled it to zero.
const ScaledDownAnnotation = "kubesage.io/scaled-down-replicas"

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

func parseHM(s string) (int, error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("time %q: use HH:MM", s)
	}
	return hh*60 + mm, nil
}

// ShouldBeDown says whether a down/up schedule wants workloads scaled to zero at now.
// On days not listed (default mon-fri) workloads stay down all day.
func ShouldBeDown(now time.Time, down, up string, days []string) (bool, error) {
	d, err := parseHM(down)
	if err != nil {
		return false, err
	}
	u, err := parseHM(up)
	if err != nil {
		return false, err
	}
	if len(days) == 0 {
		days = []string{"mon", "tue", "wed", "thu", "fri"}
	}
	active := false
	for _, day := range days {
		wd, ok := weekdays[strings.ToLower(day)[:min(3, len(day))]]
		if !ok {
			return false, fmt.Errorf("day %q: use mon..sun", day)
		}
		if wd == now.Weekday() {
			active = true
		}
	}
	if !active {
		return true, nil
	}
	cur := now.Hour()*60 + now.Minute()
	if d == u {
		return false, nil
	}
	if d < u { // e.g. down 01:00 up 05:00
		return cur >= d && cur < u, nil
	}
	return cur >= d || cur < u, nil // e.g. down 19:00 up 07:00 (over midnight)
}

// ScheduledWorkload is a Deployment or StatefulSet a schedule manages.
type ScheduledWorkload struct {
	Kind, Namespace, Name string
	Replicas              int32
	Saved                 int32 // replicas before scale-down (0 = not scaled down by kubesage)
}

// ScheduleTargets lists Deployments and StatefulSets in ns matching the label selector.
func (c *Client) ScheduleTargets(ctx context.Context, ns, selector string) ([]ScheduledWorkload, error) {
	opts := metav1.ListOptions{LabelSelector: selector}
	var out []ScheduledWorkload
	saved := func(an map[string]string) int32 {
		n, _ := strconv.Atoi(an[ScaledDownAnnotation])
		return int32(n)
	}
	deps, err := c.Clientset.AppsV1().Deployments(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	for _, d := range deps.Items {
		out = append(out, ScheduledWorkload{"Deployment", d.Namespace, d.Name, ptrInt32(d.Spec.Replicas, 1), saved(d.Annotations)})
	}
	if sts, err := c.Clientset.AppsV1().StatefulSets(ns).List(ctx, opts); err == nil {
		for _, s := range sts.Items {
			out = append(out, ScheduledWorkload{"StatefulSet", s.Namespace, s.Name, ptrInt32(s.Spec.Replicas, 1), saved(s.Annotations)})
		}
	}
	return out, nil
}

func ptrInt32(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func (c *Client) patchWorkload(ctx context.Context, w ScheduledWorkload, patch map[string]any, dryRun bool) error {
	b, _ := json.Marshal(patch)
	opts := metav1.PatchOptions{FieldManager: "kubesage"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	var err error
	if w.Kind == "StatefulSet" {
		_, err = c.Clientset.AppsV1().StatefulSets(w.Namespace).Patch(ctx, w.Name, types.MergePatchType, b, opts)
	} else {
		_, err = c.Clientset.AppsV1().Deployments(w.Namespace).Patch(ctx, w.Name, types.MergePatchType, b, opts)
	}
	return err
}

// ScaleToZero scales running workloads to zero and remembers their replicas. It returns what changed.
func (c *Client) ScaleToZero(ctx context.Context, ws []ScheduledWorkload, dryRun bool) ([]string, error) {
	var done []string
	for _, w := range ws {
		if w.Replicas == 0 {
			continue
		}
		patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{ScaledDownAnnotation: fmt.Sprint(w.Replicas)}},
			"spec": map[string]any{"replicas": 0}}
		if err := c.patchWorkload(ctx, w, patch, dryRun); err != nil {
			return done, fmt.Errorf("%s/%s: %w", strings.ToLower(w.Kind), w.Name, err)
		}
		done = append(done, fmt.Sprintf("%s/%s %d → 0", strings.ToLower(w.Kind), w.Name, w.Replicas))
	}
	return done, nil
}

// RestoreScale brings workloads scaled down by kubesage back to their saved replicas.
func (c *Client) RestoreScale(ctx context.Context, ws []ScheduledWorkload, dryRun bool) ([]string, error) {
	var done []string
	for _, w := range ws {
		if w.Saved == 0 {
			continue
		}
		patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{ScaledDownAnnotation: nil}},
			"spec": map[string]any{"replicas": w.Saved}}
		if err := c.patchWorkload(ctx, w, patch, dryRun); err != nil {
			return done, fmt.Errorf("%s/%s: %w", strings.ToLower(w.Kind), w.Name, err)
		}
		done = append(done, fmt.Sprintf("%s/%s 0 → %d", strings.ToLower(w.Kind), w.Name, w.Saved))
	}
	return done, nil
}
