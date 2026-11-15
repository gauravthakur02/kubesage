package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Cordon marks a node (un)schedulable.
func (c *Client) Cordon(ctx context.Context, node string, on bool) error {
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, on)
	_, err := c.Clientset.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// ParseTaint parses key=value:Effect, key:Effect, or (for removal) key[:Effect]-.
func ParseTaint(s string) (t corev1.Taint, remove bool, err error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "-") {
		remove, s = true, strings.TrimSuffix(s, "-")
	}
	kv, effect, hasEffect := strings.Cut(s, ":")
	t.Key, t.Value, _ = strings.Cut(kv, "=")
	if t.Key == "" {
		return t, remove, fmt.Errorf("taint needs a key, e.g. dedicated=gpu:NoSchedule")
	}
	if hasEffect {
		switch corev1.TaintEffect(effect) {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
			t.Effect = corev1.TaintEffect(effect)
		default:
			return t, remove, fmt.Errorf("effect must be NoSchedule, PreferNoSchedule or NoExecute")
		}
	} else if !remove {
		return t, remove, fmt.Errorf("an effect is required, e.g. %s:NoSchedule", kv)
	}
	return t, remove, nil
}

// SetTaint adds/updates a taint, or removes it (all effects for the key when Effect is empty).
func (c *Client) SetTaint(ctx context.Context, node string, t corev1.Taint, remove bool) error {
	n, err := c.Clientset.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var out []corev1.Taint
	found := false
	for _, x := range n.Spec.Taints {
		same := x.Key == t.Key && (t.Effect == "" || x.Effect == t.Effect)
		switch {
		case same && remove:
			found = true
		case same:
			found = true
			out = append(out, t)
		default:
			out = append(out, x)
		}
	}
	if remove && !found {
		return fmt.Errorf("node %s has no taint %s", node, t.Key)
	}
	if !remove && !found {
		out = append(out, t)
	}
	n.Spec.Taints = out
	_, err = c.Clientset.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
	return err
}

// DrainItem is one pod on a node and what a drain does with it.
type DrainItem struct {
	Namespace, Name, Owner string
	Action                 string // evict, skip, blocked
	Reason                 string
}

// DrainPlan previews a drain: it changes nothing.
type DrainPlan struct {
	Node     string
	Items    []DrainItem
	Evict    int
	Skip     int
	Blocked  int
	Warnings int
}

func ownerOf(p *corev1.Pod) string {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return o.Kind + "/" + o.Name
		}
	}
	return ""
}

// PlanDrain lists every pod on node and decides, like `kubectl drain --ignore-daemonsets`,
// what happens to it; PodDisruptionBudgets that allow no disruption are reported as blocking.
func (c *Client) PlanDrain(ctx context.Context, node string) (*DrainPlan, error) {
	pods, err := c.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, err
	}
	plan := &DrainPlan{Node: node}
	pdbs := map[string][]policyv1.PodDisruptionBudget{}
	for i := range pods.Items {
		p := &pods.Items[i]
		it := DrainItem{Namespace: p.Namespace, Name: p.Name, Owner: ownerOf(p), Action: "evict"}
		switch {
		case p.Annotations[corev1.MirrorPodAnnotationKey] != "":
			it.Action, it.Reason = "skip", "static (mirror) pod managed by the kubelet"
		case strings.HasPrefix(it.Owner, "DaemonSet/"):
			it.Action, it.Reason = "skip", "DaemonSet pod (runs on every node anyway)"
		case p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed:
			it.Reason = "finished pod, removed"
		case it.Owner == "":
			it.Action, it.Reason = "blocked", "not managed by a controller: it would be deleted and not recreated"
		}
		if it.Action == "evict" {
			for _, v := range p.Spec.Volumes {
				if v.EmptyDir != nil {
					it.Reason = strings.TrimSpace(it.Reason + " emptyDir data is lost")
					plan.Warnings++
					break
				}
			}
			if _, ok := pdbs[p.Namespace]; !ok {
				if l, err := c.Clientset.PolicyV1().PodDisruptionBudgets(p.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
					pdbs[p.Namespace] = l.Items
				} else {
					pdbs[p.Namespace] = nil
				}
			}
			for _, pdb := range pdbs[p.Namespace] {
				sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
				if err == nil && sel.Matches(labels.Set(p.Labels)) && pdb.Status.DisruptionsAllowed == 0 {
					it.Action, it.Reason = "blocked", "PodDisruptionBudget "+pdb.Name+" allows 0 disruptions right now (eviction would wait)"
				}
			}
		}
		switch it.Action {
		case "evict":
			plan.Evict++
		case "skip":
			plan.Skip++
		default:
			plan.Blocked++
		}
		plan.Items = append(plan.Items, it)
	}
	order := map[string]int{"blocked": 0, "evict": 1, "skip": 2}
	sort.SliceStable(plan.Items, func(i, j int) bool { return order[plan.Items[i].Action] < order[plan.Items[j].Action] })
	return plan, nil
}

// DrainResult reports a drain.
type DrainResult struct {
	Evicted []string
	Failed  []string
}

// Drain cordons the node and evicts the pods the plan marks "evict" (blocked pods are left alone).
// Evictions refused by a PodDisruptionBudget (HTTP 429) are retried a few times.
func (c *Client) Drain(ctx context.Context, plan *DrainPlan) (DrainResult, error) {
	var res DrainResult
	if err := c.Cordon(ctx, plan.Node, true); err != nil {
		return res, err
	}
	for _, it := range plan.Items {
		if it.Action != "evict" {
			continue
		}
		ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: it.Name, Namespace: it.Namespace}}
		var err error
		for attempt := 0; attempt < 4; attempt++ {
			err = c.Clientset.PolicyV1().Evictions(it.Namespace).Evict(ctx, ev)
			if err == nil || apierrors.IsNotFound(err) {
				err = nil
				break
			}
			if !apierrors.IsTooManyRequests(err) {
				break
			}
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if err != nil {
			res.Failed = append(res.Failed, it.Namespace+"/"+it.Name+": "+err.Error())
		} else {
			res.Evicted = append(res.Evicted, it.Namespace+"/"+it.Name)
		}
	}
	return res, nil
}

// NodeReport is a health report for one node.
type NodeReport struct {
	Node                           *corev1.Node
	Pods                           []corev1.Pod
	CPUReq, MemReq, CPULim, MemLim int64
	CPUUse, MemUse                 int64
	HasUsage                       bool
}

var nodeMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

// NodeHealth gathers conditions, capacity, requests/limits of the pods on the node and live usage.
func (c *Client) NodeHealth(ctx context.Context, name string) (*NodeReport, error) {
	n, err := c.Clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	r := &NodeReport{Node: n}
	if pods, err := c.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + name}); err == nil {
		r.Pods = pods.Items
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			cr, cl, mr, ml := podLimits(p)
			r.CPUReq, r.CPULim, r.MemReq, r.MemLim = r.CPUReq+cr, r.CPULim+cl, r.MemReq+mr, r.MemLim+ml
		}
	}
	if c.Dynamic != nil {
		if m, err := c.Dynamic.Resource(nodeMetricsGVR).Get(ctx, name, metav1.GetOptions{}); err == nil {
			usage, _ := m.Object["usage"].(map[string]any)
			if q, err := parseQty(usage["cpu"]); err == nil {
				r.CPUUse = q.MilliValue()
			}
			if q, err := parseQty(usage["memory"]); err == nil {
				r.MemUse = q.Value()
			}
			r.HasUsage = true
		}
	}
	return r, nil
}
