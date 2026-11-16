package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PolicyViolation is one result from Kyverno, Gatekeeper or Pod Security Admission.
type PolicyViolation struct {
	Engine, Policy, Result, Namespace, Object, Message string
	H                                                  Health
}

var (
	polReportGVR  = schema.GroupVersionResource{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "policyreports"}
	cPolReportGVR = schema.GroupVersionResource{Group: "wgpolicyk8s.io", Version: "v1alpha2", Resource: "clusterpolicyreports"}
)

// PolicyReports collects failing Kyverno (PolicyReport) results, Gatekeeper constraint violations and
// namespaces' Pod Security Admission levels. engines lists which were found.
func (c *Client) PolicyReports(ctx context.Context) (out []PolicyViolation, engines []string, err error) {
	if c.Dynamic == nil {
		return nil, nil, fmt.Errorf("dynamic client unavailable")
	}
	// Kyverno / any wgpolicyk8s.io producer
	reports, rerr := listAcross(ctx, c, c.Namespace, func(n string) ([]unstructured.Unstructured, error) {
		l, err := c.Dynamic.Resource(polReportGVR).Namespace(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if c.Namespace == AllNamespaces {
		if l, err := c.Dynamic.Resource(cPolReportGVR).List(ctx, metav1.ListOptions{}); err == nil {
			reports = append(reports, l.Items...)
		}
	}
	if rerr == nil || len(reports) > 0 {
		engines = append(engines, "PolicyReports (Kyverno)")
	}
	for _, r := range reports {
		results, _, _ := unstructured.NestedSlice(r.Object, "results")
		for _, x := range results {
			m, _ := x.(map[string]any)
			res := fmt.Sprint(m["result"])
			if res != "fail" && res != "warn" && res != "error" {
				continue
			}
			h := HealthWarn
			if res == "fail" || res == "error" {
				h = HealthErr
			}
			obj := r.GetNamespace()
			if rs, ok := m["resources"].([]any); ok && len(rs) > 0 {
				if rm, ok := rs[0].(map[string]any); ok {
					obj = fmt.Sprintf("%v/%v", rm["kind"], rm["name"])
				}
			} else if scope, ok := r.Object["scope"].(map[string]any); ok {
				obj = fmt.Sprintf("%v/%v", scope["kind"], scope["name"])
			}
			pol := fmt.Sprint(m["policy"])
			if rule, ok := m["rule"]; ok {
				pol += " / " + fmt.Sprint(rule)
			}
			out = append(out, PolicyViolation{Engine: fmt.Sprint(nonNil(m["source"], "kyverno")), Policy: pol, Result: res, Namespace: r.GetNamespace(),
				Object: obj, Message: firstLine(fmt.Sprint(m["message"])), H: h})
		}
	}
	// Gatekeeper constraints
	if rl, err := c.Clientset.Discovery().ServerResourcesForGroupVersion("constraints.gatekeeper.sh/v1beta1"); err == nil {
		engines = append(engines, "Gatekeeper")
		for _, ar := range rl.APIResources {
			if strings.Contains(ar.Name, "/") {
				continue
			}
			l, err := c.Dynamic.Resource(schema.GroupVersionResource{Group: "constraints.gatekeeper.sh", Version: "v1beta1", Resource: ar.Name}).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for _, cons := range l.Items {
				action, _, _ := unstructured.NestedString(cons.Object, "spec", "enforcementAction")
				vs, _, _ := unstructured.NestedSlice(cons.Object, "status", "violations")
				for _, v := range vs {
					m, _ := v.(map[string]any)
					ns := fmt.Sprint(nonNil(m["namespace"], ""))
					if c.Namespace != AllNamespaces && ns != c.Namespace {
						continue
					}
					h := HealthErr
					if action == "warn" || action == "dryrun" {
						h = HealthWarn
					}
					out = append(out, PolicyViolation{Engine: "gatekeeper", Policy: ar.Kind + "/" + cons.GetName(), Result: nonEmptyAction(action), Namespace: ns,
						Object: fmt.Sprintf("%v/%v", m["kind"], m["name"]), Message: firstLine(fmt.Sprint(m["message"])), H: h})
				}
			}
		}
	}
	// Pod Security Admission levels
	if nss, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err == nil {
		engines = append(engines, "Pod Security Admission")
		for _, n := range nss.Items {
			if c.Namespace != AllNamespaces && n.Name != c.Namespace {
				continue
			}
			l := n.Labels
			enf, warn, audit := l["pod-security.kubernetes.io/enforce"], l["pod-security.kubernetes.io/warn"], l["pod-security.kubernetes.io/audit"]
			if enf == "" && warn == "" && audit == "" {
				continue
			}
			h := HealthOK
			if enf == "privileged" || enf == "" {
				h = HealthWarn
			}
			out = append(out, PolicyViolation{Engine: "pod-security", Policy: "namespace labels", Result: "enforce=" + nonEmptyStr(enf, "none"), Namespace: n.Name,
				Object: "Namespace/" + n.Name, Message: fmt.Sprintf("warn=%s audit=%s", nonEmptyStr(warn, "none"), nonEmptyStr(audit, "none")), H: h})
		}
	}
	if len(engines) == 0 && rerr != nil && !apierrors.IsNotFound(rerr) {
		return nil, nil, rerr
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].H > out[j].H })
	return out, engines, nil
}

func nonNil(v any, d string) any {
	if v == nil {
		return d
	}
	return v
}

func nonEmptyStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func nonEmptyAction(a string) string {
	if a == "" {
		return "deny"
	}
	return a
}

// QuotaLine is one resource of a ResourceQuota.
type QuotaLine struct {
	Namespace, Quota, Resource string
	Used, Hard                 string
	Percent                    int
}

// PoolCapacity is the request-based utilisation of a node pool.
type PoolCapacity struct {
	Pool                   string
	Nodes                  int
	CPUAlloc, CPUReq       int64 // millicores
	MemAlloc, MemReq       int64 // bytes
	Removable              int   // nodes that could go while keeping 20% headroom
	MaxNodeCPU, MaxNodeMem int64
}

// Capacity returns ResourceQuota usage in scope and request-based node pool utilisation.
func (c *Client) Capacity(ctx context.Context) ([]QuotaLine, []string, []PoolCapacity, error) {
	var quotas []QuotaLine
	var limits []string
	if qs, err := listAcross(ctx, c, c.Namespace, func(n string) ([]corev1.ResourceQuota, error) {
		l, err := c.Clientset.CoreV1().ResourceQuotas(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, q := range qs {
			for res, hard := range q.Status.Hard {
				used := q.Status.Used[res]
				pct := -1
				if hard.MilliValue() > 0 {
					pct = int(used.MilliValue() * 100 / hard.MilliValue())
				}
				quotas = append(quotas, QuotaLine{q.Namespace, q.Name, string(res), used.String(), hard.String(), pct})
			}
		}
		sort.Slice(quotas, func(i, j int) bool { return quotas[i].Percent > quotas[j].Percent })
	}
	if lrs, err := listAcross(ctx, c, c.Namespace, func(n string) ([]corev1.LimitRange, error) {
		l, err := c.Clientset.CoreV1().LimitRanges(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, lr := range lrs {
			for _, it := range lr.Spec.Limits {
				var parts []string
				for k, v := range it.DefaultRequest {
					parts = append(parts, "default request "+string(k)+"="+v.String())
				}
				for k, v := range it.Default {
					parts = append(parts, "default limit "+string(k)+"="+v.String())
				}
				for k, v := range it.Max {
					parts = append(parts, "max "+string(k)+"="+v.String())
				}
				sort.Strings(parts)
				limits = append(limits, fmt.Sprintf("%s/%s (%s): %s", lr.Namespace, lr.Name, it.Type, strings.Join(parts, ", ")))
			}
		}
	}
	nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return quotas, limits, nil, nil // node capacity needs cluster-wide node list
	}
	pods, err := c.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "status.phase!=Succeeded,status.phase!=Failed"})
	if err != nil {
		return quotas, limits, nil, nil
	}
	nodePool := map[string]string{}
	by := map[string]*PoolCapacity{}
	for _, n := range nodes.Items {
		pool := n.Labels["kubernetes.azure.com/agentpool"]
		if pool == "" {
			pool = nonEmptyStr(n.Labels["agentpool"], nonEmptyStr(n.Labels["eks.amazonaws.com/nodegroup"], nonEmptyStr(n.Labels["cloud.google.com/gke-nodepool"], "default")))
		}
		nodePool[n.Name] = pool
		pc := by[pool]
		if pc == nil {
			pc = &PoolCapacity{Pool: pool}
			by[pool] = pc
		}
		pc.Nodes++
		cpu, mem := n.Status.Allocatable.Cpu().MilliValue(), n.Status.Allocatable.Memory().Value()
		pc.CPUAlloc += cpu
		pc.MemAlloc += mem
		pc.MaxNodeCPU, pc.MaxNodeMem = max(pc.MaxNodeCPU, cpu), max(pc.MaxNodeMem, mem)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		pc := by[nodePool[p.Spec.NodeName]]
		if pc == nil {
			continue
		}
		cr, _, mr, _ := podLimits(p)
		pc.CPUReq += cr
		pc.MemReq += mr
	}
	var pools []PoolCapacity
	for _, pc := range by {
		// keep 20% headroom; a node can go if the rest still fits both CPU and memory
		for k := pc.Nodes - 1; k >= 1; k-- {
			if float64(pc.CPUReq) <= 0.8*float64(pc.CPUAlloc-int64(pc.Nodes-k)*pc.MaxNodeCPU) &&
				float64(pc.MemReq) <= 0.8*float64(pc.MemAlloc-int64(pc.Nodes-k)*pc.MaxNodeMem) {
				pc.Removable = pc.Nodes - k
				break
			}
		}
		pools = append(pools, *pc)
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Pool < pools[j].Pool })
	return quotas, limits, pools, nil
}

// Backup is a Velero backup or schedule.
type Backup struct {
	Kind, Namespace, Name, Phase string
	Started, Completed, Expires  time.Time
	Errors, Warnings             int64
	Schedule, Includes           string
	Paused                       bool
	H                            Health
}

var (
	veleroBackupGVR   = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	veleroScheduleGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "schedules"}
)

// VeleroBackups lists Velero schedules and backups (newest first). found is false without Velero.
func (c *Client) VeleroBackups(ctx context.Context) ([]Backup, bool, error) {
	if c.Dynamic == nil {
		return nil, false, nil
	}
	ts := func(o map[string]any, f ...string) time.Time {
		s, _, _ := unstructured.NestedString(o, f...)
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}
	var out []Backup
	found := false
	list := func(gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
		l, err := c.Dynamic.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}
	scheds, err := list(veleroScheduleGVR)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, true, err
	}
	if err == nil {
		found = true
	}
	for _, s := range scheds {
		b := Backup{Kind: "Schedule", Namespace: s.GetNamespace(), Name: s.GetName(), H: HealthOK}
		b.Schedule, _, _ = unstructured.NestedString(s.Object, "spec", "schedule")
		b.Paused, _, _ = unstructured.NestedBool(s.Object, "spec", "paused")
		b.Phase, _, _ = unstructured.NestedString(s.Object, "status", "phase")
		b.Started = ts(s.Object, "status", "lastBackup")
		inc, _, _ := unstructured.NestedStringSlice(s.Object, "spec", "template", "includedNamespaces")
		b.Includes = strings.Join(inc, ",")
		if b.Paused {
			b.H = HealthWarn
		}
		if !b.Started.IsZero() && time.Since(b.Started) > 48*time.Hour {
			b.H = HealthWarn
		}
		out = append(out, b)
	}
	backups, err := list(veleroBackupGVR)
	if err == nil {
		found = true
		for _, x := range backups {
			b := Backup{Kind: "Backup", Namespace: x.GetNamespace(), Name: x.GetName()}
			b.Phase, _, _ = unstructured.NestedString(x.Object, "status", "phase")
			b.Started, b.Completed, b.Expires = ts(x.Object, "status", "startTimestamp"), ts(x.Object, "status", "completionTimestamp"), ts(x.Object, "status", "expiration")
			b.Errors, _, _ = unstructured.NestedInt64(x.Object, "status", "errors")
			b.Warnings, _, _ = unstructured.NestedInt64(x.Object, "status", "warnings")
			b.Schedule = x.GetLabels()["velero.io/schedule-name"]
			switch {
			case b.Phase == "Completed" && b.Errors == 0:
				b.H = HealthOK
			case b.Phase == "InProgress" || b.Phase == "New":
				b.H = HealthNeutral
			case b.Phase == "PartiallyFailed" || b.Warnings > 0:
				b.H = HealthWarn
			default:
				b.H = HealthErr
			}
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == "Schedule"
		}
		return out[i].Started.After(out[j].Started)
	})
	return out, found, nil
}
