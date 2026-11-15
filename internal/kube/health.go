package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// Usage is live resource usage from metrics-server.
type Usage struct {
	CPUMilli int64
	MemBytes int64
}

// PodMetrics returns pod → container → usage. Fails if metrics-server is missing or forbidden.
func (c *Client) PodMetrics(ctx context.Context, ns string) (map[string]map[string]Usage, error) {
	if c.Dynamic == nil {
		return nil, fmt.Errorf("metrics unavailable")
	}
	items, err := listAcross(ctx, c, ns, func(n string) ([]unstructured.Unstructured, error) {
		l, err := c.Dynamic.Resource(podMetricsGVR).Namespace(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]Usage{}
	for i := range items {
		out[items[i].GetNamespace()+"/"+items[i].GetName()] = containerUsage(&items[i])
	}
	return out, nil
}

// containerUsage parses a PodMetrics object into container → usage.
func containerUsage(obj *unstructured.Unstructured) map[string]Usage {
	m := map[string]Usage{}
	ctrs, _, _ := unstructured.NestedSlice(obj.Object, "containers")
	for _, raw := range ctrs {
		cm, _ := raw.(map[string]any)
		name, _ := cm["name"].(string)
		usage, _ := cm["usage"].(map[string]any)
		var u Usage
		if s, ok := usage["cpu"].(string); ok {
			if q, err := resource.ParseQuantity(s); err == nil {
				u.CPUMilli = q.MilliValue()
			}
		}
		if s, ok := usage["memory"].(string); ok {
			if q, err := resource.ParseQuantity(s); err == nil {
				u.MemBytes = q.Value()
			}
		}
		m[name] = u
	}
	return m
}

// FormatBytes renders bytes as Ki/Mi/Gi like kubectl top.
func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%dMi", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%dKi", b>>10)
	}
	return fmt.Sprintf("%dB", b)
}

// Percent returns used/limit in percent, or -1 when there is no limit.
func Percent(used, limit int64) int {
	if limit <= 0 {
		return -1
	}
	return int(used * 100 / limit)
}

// podLimits sums container requests and limits (cpu millicores, memory bytes).
func podLimits(p *corev1.Pod) (cpuReq, cpuLim, memReq, memLim int64) {
	for _, c := range p.Spec.Containers {
		cpuReq += c.Resources.Requests.Cpu().MilliValue()
		cpuLim += c.Resources.Limits.Cpu().MilliValue()
		memReq += c.Resources.Requests.Memory().Value()
		memLim += c.Resources.Limits.Memory().Value()
	}
	return
}

// usageCell renders "85m 43%" (percent of limit) or just "85m" when there is no limit.
func usageCell(used string, pct int) string {
	if pct < 0 {
		return used
	}
	return fmt.Sprintf("%s %d%%", used, pct)
}

// ---------- pod health report ----------

// ProbeInfo describes one configured probe.
type ProbeInfo struct {
	Kind   string // liveness, readiness, startup
	Detail string
}

// ContainerHealth is the health of one container.
type ContainerHealth struct {
	Name, Image                    string
	Init                           bool
	State, StateDetail             string
	Ready                          bool
	Restarts                       int32
	LastTermination                string // "OOMKilled (exit 137) 2h ago"
	CPUReq, CPULim, MemReq, MemLim int64
	Usage                          *Usage
	Probes                         []ProbeInfo
	Warnings                       []string // configuration advice (no limits, no probes…)
}

// ConditionInfo is a pod condition.
type ConditionInfo struct {
	Type, Reason, Message string
	OK                    bool
	Since                 string
}

// PodHealth is a full health report for one pod.
type PodHealth struct {
	Name, Namespace, Node, IP, QoS, Phase, Status, Age string
	Verdict                                            Health
	Summary                                            string
	Containers                                         []ContainerHealth
	Conditions                                         []ConditionInfo
	Issues                                             []Issue
	Events                                             []string
	MetricsErr                                         error
}

func describeProbe(kind string, p *corev1.Probe) ProbeInfo {
	var what string
	switch {
	case p.HTTPGet != nil:
		what = fmt.Sprintf("HTTP GET :%s %s", p.HTTPGet.Port.String(), nonEmpty(p.HTTPGet.Path, "/"))
	case p.TCPSocket != nil:
		what = "TCP :" + p.TCPSocket.Port.String()
	case p.GRPC != nil:
		what = fmt.Sprintf("gRPC :%d", p.GRPC.Port)
	case p.Exec != nil:
		what = "exec " + strings.Join(p.Exec.Command, " ")
		if len(what) > 40 {
			what = what[:40] + "…"
		}
	}
	return ProbeInfo{Kind: kind, Detail: fmt.Sprintf("%s · delay %ds · every %ds · timeout %ds · fail after %d",
		what, p.InitialDelaySeconds, max(p.PeriodSeconds, 10), max(p.TimeoutSeconds, 1), max(p.FailureThreshold, 3))}
}

func containerState(cs *corev1.ContainerStatus) (string, string) {
	switch {
	case cs == nil:
		return "Unknown", ""
	case cs.State.Running != nil:
		return "Running", "since " + Age(cs.State.Running.StartedAt)
	case cs.State.Waiting != nil:
		return "Waiting", strings.TrimSpace(cs.State.Waiting.Reason + " " + firstLine(cs.State.Waiting.Message))
	case cs.State.Terminated != nil:
		t := cs.State.Terminated
		return "Terminated", fmt.Sprintf("%s (exit %d)", t.Reason, t.ExitCode)
	}
	return "Unknown", ""
}

// PodHealthReport gathers status, resources, live usage, probes, conditions, issues and warning events.
func (c *Client) PodHealthReport(ctx context.Context, ns, name string) (*PodHealth, error) {
	p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	h := &PodHealth{Name: p.Name, Namespace: p.Namespace, Node: p.Spec.NodeName, IP: p.Status.PodIP,
		QoS: string(p.Status.QOSClass), Phase: string(p.Status.Phase), Status: PodStatus(p), Age: Age(p.CreationTimestamp)}

	var usage map[string]Usage
	if c.Dynamic != nil {
		obj, merr := c.Dynamic.Resource(podMetricsGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if merr == nil {
			usage = containerUsage(obj)
		}
		h.MetricsErr = merr
	} else {
		h.MetricsErr = fmt.Errorf("metrics unavailable")
	}

	statusOf := map[string]*corev1.ContainerStatus{}
	for i := range p.Status.ContainerStatuses {
		statusOf[p.Status.ContainerStatuses[i].Name] = &p.Status.ContainerStatuses[i]
	}
	for i := range p.Status.InitContainerStatuses {
		statusOf["init:"+p.Status.InitContainerStatuses[i].Name] = &p.Status.InitContainerStatuses[i]
	}
	add := func(ctr corev1.Container, init bool) {
		key := ctr.Name
		if init {
			key = "init:" + ctr.Name
		}
		cs := statusOf[key]
		ch := ContainerHealth{Name: ctr.Name, Image: ctr.Image, Init: init,
			CPUReq: ctr.Resources.Requests.Cpu().MilliValue(), CPULim: ctr.Resources.Limits.Cpu().MilliValue(),
			MemReq: ctr.Resources.Requests.Memory().Value(), MemLim: ctr.Resources.Limits.Memory().Value()}
		ch.State, ch.StateDetail = containerState(cs)
		if cs != nil {
			ch.Ready, ch.Restarts = cs.Ready, cs.RestartCount
			if t := cs.LastTerminationState.Terminated; t != nil {
				ch.LastTermination = fmt.Sprintf("%s (exit %d) %s ago", nonEmpty(t.Reason, "Error"), t.ExitCode, Age(t.FinishedAt))
			}
		}
		if u, ok := usage[ctr.Name]; ok && !init {
			ch.Usage = &u
		}
		if ctr.LivenessProbe != nil {
			ch.Probes = append(ch.Probes, describeProbe("liveness", ctr.LivenessProbe))
		}
		if ctr.ReadinessProbe != nil {
			ch.Probes = append(ch.Probes, describeProbe("readiness", ctr.ReadinessProbe))
		}
		if ctr.StartupProbe != nil {
			ch.Probes = append(ch.Probes, describeProbe("startup", ctr.StartupProbe))
		}
		if !init {
			if ch.MemLim == 0 {
				ch.Warnings = append(ch.Warnings, "no memory limit: a leak can exhaust the node and get other pods evicted")
			}
			if ch.CPUReq == 0 && ch.MemReq == 0 {
				ch.Warnings = append(ch.Warnings, "no resource requests: the scheduler cannot reserve capacity (BestEffort)")
			}
			if ctr.ReadinessProbe == nil {
				ch.Warnings = append(ch.Warnings, "no readiness probe: traffic is sent before the app is ready")
			}
			if ctr.LivenessProbe == nil {
				ch.Warnings = append(ch.Warnings, "no liveness probe: a hung process is never restarted")
			}
			if ch.Usage != nil && ch.MemLim > 0 && Percent(ch.Usage.MemBytes, ch.MemLim) >= 90 {
				ch.Warnings = append(ch.Warnings, fmt.Sprintf("memory at %d%% of limit: close to OOMKilled", Percent(ch.Usage.MemBytes, ch.MemLim)))
			}
			if ch.Usage != nil && ch.CPULim > 0 && Percent(ch.Usage.CPUMilli, ch.CPULim) >= 90 {
				ch.Warnings = append(ch.Warnings, fmt.Sprintf("CPU at %d%% of limit: likely throttled", Percent(ch.Usage.CPUMilli, ch.CPULim)))
			}
		}
		h.Containers = append(h.Containers, ch)
	}
	for _, c := range p.Spec.InitContainers {
		add(c, true)
	}
	for _, c := range p.Spec.Containers {
		add(c, false)
	}

	for _, cond := range p.Status.Conditions {
		h.Conditions = append(h.Conditions, ConditionInfo{Type: string(cond.Type), OK: cond.Status == corev1.ConditionTrue,
			Reason: cond.Reason, Message: firstLine(cond.Message), Since: Age(cond.LastTransitionTime)})
	}
	sort.SliceStable(h.Conditions, func(i, j int) bool { return condOrder(h.Conditions[i].Type) < condOrder(h.Conditions[j].Type) })

	h.Issues = PodIssues(p)
	sel := fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod,type=Warning", name)
	if evs, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel}); err == nil {
		items := evs.Items
		sort.Slice(items, func(i, j int) bool { return eventTime(&items[i]).After(eventTime(&items[j])) })
		for i := range items {
			if i == 8 {
				break
			}
			e := &items[i]
			h.Events = append(h.Events, fmt.Sprintf("%s ago  %s  x%d  %s", Age(metav1.NewTime(eventTime(e))), e.Reason, max(e.Count, 1), firstLine(e.Message)))
		}
	}

	// Verdict
	notReady := 0
	for _, ch := range h.Containers {
		if !ch.Init && !ch.Ready {
			notReady++
		}
	}
	switch {
	case len(h.Issues) > 0 && h.Issues[0].Severity == HealthErr:
		h.Verdict, h.Summary = HealthErr, "Unhealthy: "+h.Issues[0].Reason
	case p.Status.Phase == corev1.PodSucceeded:
		h.Verdict, h.Summary = HealthNeutral, "Completed"
	case notReady > 0:
		h.Verdict, h.Summary = HealthWarn, fmt.Sprintf("Degraded: %d container(s) not ready", notReady)
	case len(h.Issues) > 0:
		h.Verdict, h.Summary = HealthWarn, "Degraded: "+h.Issues[0].Reason
	default:
		h.Verdict, h.Summary = HealthOK, "Healthy"
		for _, ch := range h.Containers {
			if len(ch.Warnings) > 0 && strings.Contains(strings.Join(ch.Warnings, " "), "% of limit") {
				h.Verdict, h.Summary = HealthWarn, "Running, but close to resource limits"
			}
		}
	}
	return h, nil
}

func condOrder(t string) int {
	switch t {
	case "PodScheduled":
		return 0
	case "Initialized":
		return 1
	case "ContainersReady":
		return 2
	case "Ready":
		return 3
	}
	return 4
}

// ---------- secrets ----------

// SecretValues returns a Secret's decoded data (client-go already base64-decodes .data).
func (c *Client) SecretValues(ctx context.Context, ns, name string) (*corev1.Secret, error) {
	return c.Clientset.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
}
