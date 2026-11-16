package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ResizePod changes a running container's CPU/memory requests and limits in place (the pods/resize
// subresource, Kubernetes 1.33+), without restarting the pod when the container's resizePolicy allows it.
// Empty values are left unchanged.
func (c *Client) ResizePod(ctx context.Context, ns, pod, container, cpuReq, memReq, cpuLim, memLim string, dryRun bool) error {
	res := map[string]map[string]string{}
	set := func(kind, name, v string) error {
		if v == "" {
			return nil
		}
		if _, err := resource.ParseQuantity(v); err != nil {
			return fmt.Errorf("%s %s: %w", kind, name, err)
		}
		if res[kind] == nil {
			res[kind] = map[string]string{}
		}
		res[kind][name] = v
		return nil
	}
	for _, x := range [][3]string{{"requests", "cpu", cpuReq}, {"requests", "memory", memReq}, {"limits", "cpu", cpuLim}, {"limits", "memory", memLim}} {
		if err := set(x[0], x[1], x[2]); err != nil {
			return err
		}
	}
	if len(res) == 0 {
		return fmt.Errorf("nothing to change")
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"containers": []map[string]any{{"name": container, "resources": res}}}})
	opts := metav1.PatchOptions{FieldManager: "kubesage"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	_, err := c.Clientset.CoreV1().Pods(ns).Patch(ctx, pod, types.StrategicMergePatchType, patch, opts, "resize")
	if err != nil && isNotFoundSub(err) {
		return fmt.Errorf("this cluster does not support in-place resize (Kubernetes 1.33+ needed); change the workload instead")
	}
	return err
}

func isNotFoundSub(err error) bool {
	s := err.Error()
	return strings.Contains(s, "the server could not find the requested resource") || (strings.Contains(s, "not found") && strings.Contains(s, "resize"))
}

// ResizeStatus reads the pod's resize conditions (PodResizePending / PodResizeInProgress).
func ResizeStatus(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Type == "PodResizePending" || c.Type == "PodResizeInProgress" {
			return string(c.Type) + ": " + c.Reason + " " + c.Message
		}
	}
	return ""
}
