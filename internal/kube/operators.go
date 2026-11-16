package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// ---------- Argo Rollouts ----------

var RolloutGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}

// ArgoRollout is one progressive-delivery Rollout.
type ArgoRollout struct {
	Namespace, Name, Strategy, Phase, Message string
	Step, Steps                               int
	Desired, Ready, Updated                   int64
	Paused, Aborted                           bool
	Image                                     string
}

func nestedInt(o map[string]any, f ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(o, f...)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// ArgoRollouts lists Rollouts in scope (found=false when Argo Rollouts is not installed).
func (c *Client) ArgoRollouts(ctx context.Context) ([]ArgoRollout, bool, error) {
	items, err := listAcross(ctx, c, c.Namespace, func(n string) ([]unstructured.Unstructured, error) {
		l, err := c.Dynamic.Resource(RolloutGVR).Namespace(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "the server could not find") || strings.Contains(err.Error(), "not found") {
			return nil, false, nil
		}
		return nil, true, err
	}
	var out []ArgoRollout
	for _, u := range items {
		o := u.Object
		r := ArgoRollout{Namespace: u.GetNamespace(), Name: u.GetName(), Strategy: "rollingUpdate"}
		if _, ok, _ := unstructured.NestedMap(o, "spec", "strategy", "canary"); ok {
			r.Strategy = "canary"
			steps, _, _ := unstructured.NestedSlice(o, "spec", "strategy", "canary", "steps")
			r.Steps = len(steps)
		} else if _, ok, _ := unstructured.NestedMap(o, "spec", "strategy", "blueGreen"); ok {
			r.Strategy = "blueGreen"
		}
		r.Phase, _, _ = unstructured.NestedString(o, "status", "phase")
		r.Message, _, _ = unstructured.NestedString(o, "status", "message")
		r.Step = int(nestedInt(o, "status", "currentStepIndex"))
		r.Desired = nestedInt(o, "spec", "replicas")
		r.Ready = nestedInt(o, "status", "readyReplicas")
		r.Updated = nestedInt(o, "status", "updatedReplicas")
		r.Paused, _, _ = unstructured.NestedBool(o, "spec", "paused")
		if pc, _, _ := unstructured.NestedSlice(o, "status", "pauseConditions"); len(pc) > 0 {
			r.Paused = true
		}
		r.Aborted, _, _ = unstructured.NestedBool(o, "status", "abort")
		if ctrs, _, _ := unstructured.NestedSlice(o, "spec", "template", "spec", "containers"); len(ctrs) > 0 {
			if m, ok := ctrs[0].(map[string]any); ok {
				r.Image, _ = m["image"].(string)
			}
		}
		out = append(out, r)
	}
	return out, true, nil
}

// RolloutAction runs promote, promote-full, abort or retry on a Rollout (the same patches the kubectl plugin sends).
func (c *Client) RolloutAction(ctx context.Context, ns, name, action string) error {
	ri := c.Dynamic.Resource(RolloutGVR).Namespace(ns)
	var status, spec map[string]any
	switch action {
	case "promote":
		status, spec = map[string]any{"pauseConditions": nil}, map[string]any{"paused": false}
	case "promote-full":
		status, spec = map[string]any{"promoteFull": true}, map[string]any{"paused": false}
	case "abort":
		status = map[string]any{"abort": true}
	case "retry":
		status = map[string]any{"abort": false}
	default:
		return fmt.Errorf("unknown rollout action %q", action)
	}
	opts := metav1.PatchOptions{FieldManager: "kubesage"}
	if spec != nil {
		b, _ := json.Marshal(map[string]any{"spec": spec})
		if _, err := ri.Patch(ctx, name, types.MergePatchType, b, opts); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(map[string]any{"status": status})
	_, err := ri.Patch(ctx, name, types.MergePatchType, b, opts, "status")
	return err
}

// ---------- Karpenter ----------

var (
	KarpenterPoolGVR  = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	KarpenterClaimGVR = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodeclaims"}
)

// KarpenterPool summarises a Karpenter NodePool with its NodeClaims.
type KarpenterPool struct {
	Name, Limits, Used, Disruption, Ready string
	Claims, ClaimsReady                   int
	Types                                 []string
}

// KarpenterPools lists NodePools and counts their NodeClaims (found=false when Karpenter is not installed).
func (c *Client) KarpenterPools(ctx context.Context) ([]KarpenterPool, bool, error) {
	pools, err := c.Dynamic.Resource(KarpenterPoolGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "could not find") || strings.Contains(err.Error(), "not found") {
			return nil, false, nil
		}
		return nil, true, err
	}
	claims, _ := c.Dynamic.Resource(KarpenterClaimGVR).List(ctx, metav1.ListOptions{})
	byPool := map[string][]unstructured.Unstructured{}
	if claims != nil {
		for _, cl := range claims.Items {
			p := cl.GetLabels()["karpenter.sh/nodepool"]
			byPool[p] = append(byPool[p], cl)
		}
	}
	kv := func(m map[string]any) string {
		var parts []string
		for k, v := range m {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(parts)
		return strings.Join(parts, " ")
	}
	var out []KarpenterPool
	for _, p := range pools.Items {
		kp := KarpenterPool{Name: p.GetName()}
		lim, _, _ := unstructured.NestedMap(p.Object, "spec", "limits")
		kp.Limits = kv(lim)
		used, _, _ := unstructured.NestedMap(p.Object, "status", "resources")
		kp.Used = kv(used)
		pol, _, _ := unstructured.NestedString(p.Object, "spec", "disruption", "consolidationPolicy")
		after, _, _ := unstructured.NestedString(p.Object, "spec", "disruption", "consolidateAfter")
		kp.Disruption = strings.TrimSpace(pol + " " + after)
		st, reason, _ := readyCondition(p.Object)
		kp.Ready = st
		if st != "True" && reason != "" {
			kp.Ready = st + " (" + reason + ")"
		}
		for _, cl := range byPool[kp.Name] {
			kp.Claims++
			if s, _, _ := readyCondition(cl.Object); s == "True" {
				kp.ClaimsReady++
			}
			it := cl.GetLabels()["node.kubernetes.io/instance-type"]
			kp.Types = addUnique(kp.Types, it)
		}
		out = append(out, kp)
	}
	return out, true, nil
}

// ---------- Crossplane ----------

// ManagedResource is a Crossplane managed resource (cloud object) with its Ready/Synced state.
type ManagedResource struct {
	Kind, Group, Name, External, Ready, Synced, Message string
}

// CrossplaneResources lists managed resources: every API resource in the "managed" category (cluster scoped).
func (c *Client) CrossplaneResources(ctx context.Context, max int) ([]ManagedResource, bool, error) {
	lists, err := c.Clientset.Discovery().ServerPreferredResources()
	if err != nil && len(lists) == 0 {
		return nil, false, err
	}
	var out []ManagedResource
	found := false
	for _, l := range lists {
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, ar := range l.APIResources {
			managed := false
			for _, cat := range ar.Categories {
				if cat == "managed" {
					managed = true
				}
			}
			if !managed || strings.Contains(ar.Name, "/") {
				continue
			}
			found = true
			items, err := c.Dynamic.Resource(gv.WithResource(ar.Name)).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, u := range items.Items {
				mr := ManagedResource{Kind: ar.Kind, Group: gv.Group, Name: u.GetName(), External: u.GetAnnotations()["crossplane.io/external-name"]}
				conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
				for _, x := range conds {
					m, _ := x.(map[string]any)
					switch m["type"] {
					case "Ready":
						mr.Ready = fmt.Sprint(m["status"])
					case "Synced":
						mr.Synced = fmt.Sprint(m["status"])
						if mr.Synced != "True" {
							mr.Message = firstLine(fmt.Sprint(m["message"]))
						}
					}
				}
				out = append(out, mr)
				if len(out) >= max {
					return out, true, nil
				}
			}
		}
	}
	return out, found, nil
}

// ---------- Cilium Hubble ----------

// HubbleFlow is one network flow observed by Hubble.
type HubbleFlow struct {
	Time                  time.Time
	Source, Dest, Verdict string
	Summary               string
}

// HubbleFlows runs `hubble observe` for a pod (needs the hubble CLI and a reachable Hubble Relay,
// e.g. `cilium hubble port-forward`).
func HubbleFlows(ctx context.Context, ns, pod string, last int) ([]HubbleFlow, error) {
	if _, err := exec.LookPath("hubble"); err != nil {
		return nil, fmt.Errorf("install the hubble CLI and run `cilium hubble port-forward` to see flows")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"observe", "--output", "jsonpb", "--last", fmt.Sprint(last)}
	if pod != "" {
		args = append(args, "--pod", ns+"/"+pod)
	} else {
		args = append(args, "--namespace", ns)
	}
	out, err := exec.CommandContext(ctx, "hubble", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("hubble: %s (is Hubble Relay reachable? try cilium hubble port-forward)", firstLine(string(ee.Stderr)))
		}
		return nil, err
	}
	return ParseHubble(out), nil
}

type hubbleEndpoint struct {
	Namespace string `json:"namespace"`
	PodName   string `json:"pod_name"`
}

// ParseHubble reads `hubble observe -o jsonpb` lines.
func ParseHubble(out []byte) []HubbleFlow {
	var flows []HubbleFlow
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Flow struct {
				Time        time.Time      `json:"time"`
				Verdict     string         `json:"verdict"`
				DropReason  string         `json:"drop_reason_desc"`
				Summary     string         `json:"Summary"`
				Source      hubbleEndpoint `json:"source"`
				Destination hubbleEndpoint `json:"destination"`
				IP          struct {
					Source      string `json:"source"`
					Destination string `json:"destination"`
				} `json:"IP"`
			} `json:"flow"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		f := rec.Flow
		name := func(ns, pod, ip string) string {
			if pod != "" {
				return ns + "/" + pod
			}
			return ip
		}
		sum := f.Summary
		if f.DropReason != "" {
			sum = f.DropReason + " " + sum
		}
		flows = append(flows, HubbleFlow{Time: f.Time, Source: name(f.Source.Namespace, f.Source.PodName, f.IP.Source),
			Dest: name(f.Destination.Namespace, f.Destination.PodName, f.IP.Destination), Verdict: f.Verdict, Summary: sum})
	}
	return flows
}
