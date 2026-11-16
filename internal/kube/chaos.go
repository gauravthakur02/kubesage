package kube

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ChaosResult is the outcome of a pod-kill experiment.
type ChaosResult struct {
	Killed    string
	Replaced  string
	Recovered bool
	After     time.Duration
	Steps     []string
}

// ChaosKillPod deletes one ready pod of a workload (or the given pod) and waits until the workload
// has as many ready pods as before. It never retries or deletes more than one pod.
func (c *Client) ChaosKillPod(ctx context.Context, r Resource, ns, name string, wait time.Duration) (ChaosResult, error) {
	var res ChaosResult
	pods, err := c.PodsFor(ctx, r, ns, name)
	if err != nil {
		return res, err
	}
	var ready []corev1.Pod
	for _, p := range pods {
		if podReady(&p) && p.DeletionTimestamp == nil {
			ready = append(ready, p)
		}
	}
	if len(ready) == 0 {
		return res, fmt.Errorf("no ready pod to delete")
	}
	if r.Name == "pods" && len(ready[0].OwnerReferences) == 0 {
		return res, fmt.Errorf("pod %s has no owner: nothing would replace it", name)
	}
	before := len(ready)
	victim := ready[rand.Intn(len(ready))]
	owner, ownerName := r, name
	if r.Name == "pods" { // measure recovery on the pod's siblings: everything sharing its controller
		ownerName = ""
		sibs, err := c.siblingPods(ctx, &victim)
		if err != nil {
			return res, err
		}
		before = 0
		for i := range sibs {
			if podReady(&sibs[i]) && sibs[i].DeletionTimestamp == nil {
				before++
			}
		}
	}
	start := time.Now()
	if err := c.Clientset.CoreV1().Pods(ns).Delete(ctx, victim.Name, metav1.DeleteOptions{}); err != nil {
		return res, err
	}
	res.Killed = victim.Name
	res.Steps = append(res.Steps, fmt.Sprintf("%s  deleted pod %s (%d ready before)", start.Format("15:04:05"), victim.Name, before))
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		var cur []corev1.Pod
		if ownerName == "" {
			cur, err = c.siblingPods(ctx, &victim)
		} else {
			cur, err = c.PodsFor(ctx, owner, ns, ownerName)
		}
		if err != nil {
			continue
		}
		n, fresh := 0, ""
		for i := range cur {
			if cur[i].Name == victim.Name || cur[i].DeletionTimestamp != nil {
				continue
			}
			if podReady(&cur[i]) {
				n++
				if cur[i].CreationTimestamp.Time.After(start.Add(-time.Second)) {
					fresh = cur[i].Name
				}
			}
		}
		want := before
		if fresh != "" && n >= want {
			res.Recovered, res.Replaced, res.After = true, fresh, time.Since(start)
			res.Steps = append(res.Steps, fmt.Sprintf("%s  replacement %s ready · %d/%d ready", time.Now().Format("15:04:05"), fresh, n, want))
			return res, nil
		}
	}
	res.After = time.Since(start)
	res.Steps = append(res.Steps, fmt.Sprintf("%s  not recovered within %s", time.Now().Format("15:04:05"), wait))
	return res, nil
}

// siblingPods are pods with the same controller owner as p.
func (c *Client) siblingPods(ctx context.Context, p *corev1.Pod) ([]corev1.Pod, error) {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return nil, fmt.Errorf("no owner")
	}
	list, err := c.Clientset.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []corev1.Pod
	for _, q := range list.Items {
		if o := metav1.GetControllerOf(&q); o != nil && o.UID == ref.UID {
			out = append(out, q)
		}
	}
	return out, nil
}
