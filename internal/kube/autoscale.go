package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Autoscaler is an HPA, KEDA ScaledObject or VPA.
type Autoscaler struct {
	Kind, Namespace, Name, Target string
	Current, Min, Max             int32
	Metrics                       string
	Status                        string
	LastScale                     string
	H                             Health
}

var (
	kedaGVR = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	vpaGVR  = schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}
)

func hpaMetric(spec autoscalingv2.MetricSpec, cur *autoscalingv2.MetricStatus) string {
	name, target, now := string(spec.Type), "", "?"
	switch spec.Type {
	case autoscalingv2.ResourceMetricSourceType:
		name = string(spec.Resource.Name)
		if u := spec.Resource.Target.AverageUtilization; u != nil {
			target = fmt.Sprintf("%d%%", *u)
		} else if v := spec.Resource.Target.AverageValue; v != nil {
			target = v.String()
		}
		if cur != nil && cur.Resource != nil {
			if u := cur.Resource.Current.AverageUtilization; u != nil {
				now = fmt.Sprintf("%d%%", *u)
			} else if v := cur.Resource.Current.AverageValue; v != nil {
				now = v.String()
			}
		}
	case autoscalingv2.ExternalMetricSourceType:
		name = spec.External.Metric.Name
		if v := spec.External.Target.AverageValue; v != nil {
			target = v.String()
		} else if v := spec.External.Target.Value; v != nil {
			target = v.String()
		}
		if cur != nil && cur.External != nil {
			if v := cur.External.Current.AverageValue; v != nil {
				now = v.String()
			} else if v := cur.External.Current.Value; v != nil {
				now = v.String()
			}
		}
	case autoscalingv2.PodsMetricSourceType:
		name = spec.Pods.Metric.Name
		if v := spec.Pods.Target.AverageValue; v != nil {
			target = v.String()
		}
		if cur != nil && cur.Pods != nil {
			now = cur.Pods.Current.AverageValue.String()
		}
	}
	return fmt.Sprintf("%s %s/%s", name, now, target)
}

// Autoscalers lists HPAs (with KEDA's generated ones folded into their ScaledObject) and VPAs.
func (c *Client) Autoscalers(ctx context.Context) ([]Autoscaler, error) {
	hpas, err := listAcross(ctx, c, c.Namespace, func(n string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
		l, err := c.Clientset.AutoscalingV2().HorizontalPodAutoscalers(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if err != nil {
		return nil, err
	}
	lastScale := map[string]string{}
	nsDone := map[string]bool{}
	for _, h := range hpas {
		if nsDone[h.Namespace] {
			continue
		}
		nsDone[h.Namespace] = true
		if evs, err := c.Clientset.CoreV1().Events(h.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "reason=SuccessfulRescale"}); err == nil {
			sort.Slice(evs.Items, func(i, j int) bool { return evs.Items[i].LastTimestamp.After(evs.Items[j].LastTimestamp.Time) })
			for _, e := range evs.Items {
				k := e.Namespace + "/" + e.InvolvedObject.Name
				if _, ok := lastScale[k]; !ok {
					lastScale[k] = Age(e.LastTimestamp) + " ago: " + firstLine(e.Message)
				}
			}
		}
	}
	kedaHPA := map[string]bool{}
	var out []Autoscaler
	if c.Dynamic != nil {
		if sos, err := listAcross(ctx, c, c.Namespace, func(n string) ([]unstructured.Unstructured, error) {
			l, err := c.Dynamic.Resource(kedaGVR).Namespace(n).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			return l.Items, nil
		}); err == nil {
			for _, so := range sos {
				o := so.Object
				a := Autoscaler{Kind: "ScaledObject", Namespace: so.GetNamespace(), Name: so.GetName()}
				a.Target, _, _ = unstructured.NestedString(o, "spec", "scaleTargetRef", "name")
				if mn, ok, _ := unstructured.NestedInt64(o, "spec", "minReplicaCount"); ok {
					a.Min = int32(mn)
				}
				a.Max = 100
				if mx, ok, _ := unstructured.NestedInt64(o, "spec", "maxReplicaCount"); ok {
					a.Max = int32(mx)
				}
				trig, _, _ := unstructured.NestedSlice(o, "spec", "triggers")
				var ts []string
				for _, t := range trig {
					if m, ok := t.(map[string]any); ok {
						ts = append(ts, fmt.Sprint(m["type"]))
					}
				}
				a.Metrics = "triggers: " + strings.Join(ts, ",")
				hpaName, _, _ := unstructured.NestedString(o, "status", "hpaName")
				if hpaName == "" {
					hpaName = "keda-hpa-" + so.GetName()
				}
				kedaHPA[so.GetNamespace()+"/"+hpaName] = true
				for _, h := range hpas {
					if h.Namespace == so.GetNamespace() && h.Name == hpaName {
						a.Current = h.Status.CurrentReplicas
						for i, m := range h.Spec.Metrics {
							var cur *autoscalingv2.MetricStatus
							if i < len(h.Status.CurrentMetrics) {
								cur = &h.Status.CurrentMetrics[i]
							}
							a.Metrics += " · " + hpaMetric(m, cur)
						}
					}
				}
				a.LastScale = lastScale[so.GetNamespace()+"/"+hpaName]
				st, reason, msg := readyCondition(o)
				a.Status, a.H = "Ready="+st, HealthOK
				if st != "True" {
					a.Status, a.H = "not ready: "+nonEmptyStr(reason, firstLine(msg)), HealthErr
				}
				if paused := so.GetAnnotations()["autoscaling.keda.sh/paused-replicas"]; paused != "" {
					a.Status, a.H = "paused at "+paused, HealthWarn
				}
				out = append(out, a)
			}
		}
	}
	for _, h := range hpas {
		if kedaHPA[h.Namespace+"/"+h.Name] {
			continue
		}
		a := Autoscaler{Kind: "HPA", Namespace: h.Namespace, Name: h.Name, Target: h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name,
			Current: h.Status.CurrentReplicas, Min: ptrOr(h.Spec.MinReplicas, 1), Max: h.Spec.MaxReplicas, H: HealthOK, Status: "ok"}
		var ms []string
		for i, m := range h.Spec.Metrics {
			var cur *autoscalingv2.MetricStatus
			if i < len(h.Status.CurrentMetrics) {
				cur = &h.Status.CurrentMetrics[i]
			}
			ms = append(ms, hpaMetric(m, cur))
		}
		a.Metrics = strings.Join(ms, " · ")
		for _, cond := range h.Status.Conditions {
			switch {
			case cond.Type == autoscalingv2.ScalingActive && cond.Status != "True":
				a.Status, a.H = "metrics unavailable: "+cond.Reason+" "+firstLine(cond.Message), HealthErr
			case cond.Type == autoscalingv2.AbleToScale && cond.Status != "True":
				a.Status, a.H = "cannot scale: "+cond.Reason, HealthErr
			case cond.Type == autoscalingv2.ScalingLimited && cond.Status == "True" && cond.Reason == "TooManyReplicas" && a.H == HealthOK:
				a.Status, a.H = "stuck at max replicas: raise maxReplicas or fix the load", HealthWarn
			}
		}
		if a.H == HealthOK && a.Current >= a.Max && a.Max > 0 {
			a.Status, a.H = "at max replicas", HealthWarn
		}
		a.LastScale = lastScale[h.Namespace+"/"+h.Name]
		out = append(out, a)
	}
	for k, v := range c.vpas(ctx) {
		_ = k
		out = append(out, v.Autoscaler)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].H != out[j].H {
			return out[i].H == HealthErr || (out[i].H == HealthWarn && out[j].H == HealthOK)
		}
		return out[i].Namespace+out[i].Name < out[j].Namespace+out[j].Name
	})
	return out, nil
}

// VPARec is a VPA target recommendation summed over a pod's containers.
type VPARec struct {
	Autoscaler
	CPU, Mem int64 // per pod
}

func (c *Client) vpas(ctx context.Context) map[string]VPARec {
	out := map[string]VPARec{}
	if c.Dynamic == nil {
		return out
	}
	items, err := listAcross(ctx, c, c.Namespace, func(n string) ([]unstructured.Unstructured, error) {
		l, err := c.Dynamic.Resource(vpaGVR).Namespace(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if err != nil {
		return out
	}
	for _, v := range items {
		o := v.Object
		kind, _, _ := unstructured.NestedString(o, "spec", "targetRef", "kind")
		name, _, _ := unstructured.NestedString(o, "spec", "targetRef", "name")
		mode, _, _ := unstructured.NestedString(o, "spec", "updatePolicy", "updateMode")
		r := VPARec{Autoscaler: Autoscaler{Kind: "VPA", Namespace: v.GetNamespace(), Name: v.GetName(), Target: kind + "/" + name, H: HealthOK, Status: "mode " + nonEmptyStr(mode, "Auto")}}
		recs, _, _ := unstructured.NestedSlice(o, "status", "recommendation", "containerRecommendations")
		var parts []string
		for _, x := range recs {
			m, _ := x.(map[string]any)
			tgt, _ := m["target"].(map[string]any)
			if q, err := parseQty(tgt["cpu"]); err == nil {
				r.CPU += q.MilliValue()
			}
			if q, err := parseQty(tgt["memory"]); err == nil {
				r.Mem += q.Value()
			}
			parts = append(parts, fmt.Sprintf("%v cpu=%v mem=%v", m["containerName"], tgt["cpu"], tgt["memory"]))
		}
		if len(parts) == 0 {
			r.Metrics, r.H = "no recommendation yet", HealthNeutral
		} else {
			r.Metrics = "recommends " + strings.Join(parts, "; ")
		}
		out[v.GetNamespace()+"/"+kind+"/"+name] = r
	}
	return out
}

// VPARecommendations maps namespace/Kind/name to the VPA target per pod (for right-sizing).
func (c *Client) VPARecommendations(ctx context.Context) map[string][2]int64 {
	out := map[string][2]int64{}
	for k, v := range c.vpas(ctx) {
		if v.CPU > 0 || v.Mem > 0 {
			out[k] = [2]int64{v.CPU, v.Mem}
		}
	}
	return out
}
