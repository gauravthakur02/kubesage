package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Issue is a problem detected by the built-in rule engine (no LLM needed).
type Issue struct {
	Severity  Health
	Kind      string // resource name, e.g. "pods"
	Namespace string
	Name      string
	Reason    string
	Message   string
	Hint      string
}

func (i Issue) key() string { return i.Kind + "/" + i.Namespace + "/" + i.Name + "/" + i.Reason }

var waitingHints = map[string]string{
	"CrashLoopBackOff":           "Container keeps exiting. Check previous logs (p) and the exit code; often a bad config, missing env/secret or failing startup.",
	"ImagePullBackOff":           "Image cannot be pulled. Verify image name/tag exists and imagePullSecrets / registry access (ACR attach for AKS).",
	"ErrImagePull":               "Image pull failed. Verify image name/tag and registry credentials.",
	"InvalidImageName":           "Image reference is malformed. Fix spec.containers[].image.",
	"CreateContainerConfigError": "A referenced ConfigMap/Secret or key is missing. Check envFrom/valueFrom references.",
	"CreateContainerError":       "Runtime could not create the container. Check command/volumes in describe.",
	"RunContainerError":          "Container failed to start. Check command, entrypoint and mounted volumes.",
}

// PodIssues runs pod-level rules.
func PodIssues(p *corev1.Pod) []Issue {
	var out []Issue
	add := func(sev Health, reason, msg, hint string) {
		out = append(out, Issue{Severity: sev, Kind: "pods", Namespace: p.Namespace, Name: p.Name, Reason: reason, Message: msg, Hint: hint})
	}
	all := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range all {
		if w := cs.State.Waiting; w != nil {
			if hint, ok := waitingHints[w.Reason]; ok {
				msg := fmt.Sprintf("container %q is waiting", cs.Name)
				if m := firstLine(w.Message); m != "" {
					msg = fmt.Sprintf("container %q: %s", cs.Name, m)
				}
				add(HealthErr, w.Reason, msg, hint)
			}
		}
		// Between restarts a crashing container shows terminated/Error before the kubelet says CrashLoopBackOff;
		// both are the same outage, so both are errors (CI must not pass during the Error window).
		crashing := false
		restarts := p.Spec.RestartPolicy != corev1.RestartPolicyNever
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 && restarts && t.Reason != "OOMKilled" {
			add(HealthErr, "ContainerExited", fmt.Sprintf("container %q exited with code %d (%s), restarts: %d", cs.Name, t.ExitCode, nonEmpty(t.Reason, "Error"), cs.RestartCount),
				"The container exits right after starting: read its previous logs, check command, args, config and required env vars.")
			crashing = true
		} else if lt := cs.LastTerminationState.Terminated; lt != nil && lt.ExitCode != 0 && lt.Reason != "OOMKilled" && restarts &&
			cs.RestartCount >= 2 && !cs.Ready && (cs.State.Waiting == nil || waitingHints[cs.State.Waiting.Reason] == "") {
			add(HealthErr, "CrashLooping", fmt.Sprintf("container %q keeps exiting with code %d (%s), restarts: %d", cs.Name, lt.ExitCode, nonEmpty(lt.Reason, "Error"), cs.RestartCount),
				"The container exits repeatedly: read its previous logs, check command, args, config and liveness probes.")
			crashing = true
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			// A container that was OOMKilled once but is healthy again is a warning, not an outage.
			sev := HealthErr
			if cs.Ready && cs.State.Running != nil {
				sev = HealthWarn
			}
			add(sev, "OOMKilled", fmt.Sprintf("container %q was OOMKilled (restarts: %d, last at %s)", cs.Name, cs.RestartCount, Age(t.FinishedAt)),
				"Memory limit is too low or the app leaks memory. Raise resources.limits.memory or profile the app.")
		} else if !crashing && cs.RestartCount >= 5 && (cs.State.Waiting == nil || waitingHints[cs.State.Waiting.Reason] == "") {
			add(HealthWarn, "HighRestarts", fmt.Sprintf("container %q restarted %d times", cs.Name, cs.RestartCount),
				"Inspect previous logs and liveness probe settings.")
		}
		if cs.State.Running != nil && !cs.Ready && time.Since(cs.State.Running.StartedAt.Time) > 3*time.Minute {
			add(HealthWarn, "NotReady", fmt.Sprintf("container %q running for %s but not Ready", cs.Name,
				Age(cs.State.Running.StartedAt)), "Readiness probe is failing; check probe path/port and app health endpoint.")
		}
	}
	switch p.Status.Phase {
	case corev1.PodPending:
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
				add(HealthErr, "Unschedulable", firstLine(c.Message),
					"Scheduler cannot place the pod: check resource requests vs node capacity, nodeSelector/affinity, taints/tolerations and PVC binding.")
			}
		}
		if len(out) == 0 && time.Since(p.CreationTimestamp.Time) > 5*time.Minute {
			add(HealthWarn, "PendingTooLong", "pod pending for "+Age(p.CreationTimestamp), "Check events for volume mounts or image pulls.")
		}
	case corev1.PodFailed:
		add(HealthWarn, nonEmpty(p.Status.Reason, "Failed"), firstLine(p.Status.Message), "Pod failed; if Evicted, check node pressure and pod resource requests.")
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 220 {
		s = s[:220] + "…"
	}
	return s
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func deploymentIssues(d *appsv1.Deployment) []Issue {
	var out []Issue
	want := deref(d.Spec.Replicas, 1)
	if want > 0 && d.Status.AvailableReplicas < want {
		sev := HealthWarn
		if d.Status.AvailableReplicas == 0 {
			sev = HealthErr
		}
		out = append(out, Issue{Severity: sev, Kind: "deployments", Namespace: d.Namespace, Name: d.Name, Reason: "Unavailable",
			Message: fmt.Sprintf("%d/%d replicas available", d.Status.AvailableReplicas, want),
			Hint:    "Inspect the deployment's pods for crash/pull/scheduling errors."})
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			out = append(out, Issue{Severity: HealthErr, Kind: "deployments", Namespace: d.Namespace, Name: d.Name, Reason: "RolloutStuck",
				Message: firstLine(c.Message), Hint: "Rollout exceeded its deadline; fix the new ReplicaSet's pods or `kubectl rollout undo`."})
		}
	}
	return out
}

// Scan runs every rule against the client's namespace (or all namespaces).
func (c *Client) Scan(ctx context.Context) ([]Issue, error) {
	ns := c.Namespace
	var out []Issue
	pods, err := c.listPodsAcross(ctx, ns)
	if err != nil {
		return nil, err
	}
	readySince := map[string]time.Time{} // pods that became Ready after their warnings (startup probe failures etc.)
	for i := range pods {
		out = append(out, PodIssues(&pods[i])...)
		if t, ok := readyAt(&pods[i]); ok {
			readySince[pods[i].Namespace+"/"+pods[i].Name] = t
		}
	}
	if deps, err := c.listDeploymentsAcross(ctx, ns); err == nil {
		for i := range deps {
			out = append(out, deploymentIssues(&deps[i])...)
		}
	}
	if sts, err := c.listStatefulSetsAcross(ctx, ns); err == nil {
		for _, s := range sts {
			want := deref(s.Spec.Replicas, 1)
			if s.Status.ReadyReplicas < want {
				out = append(out, Issue{Severity: HealthWarn, Kind: "statefulsets", Namespace: s.Namespace, Name: s.Name, Reason: "NotReady",
					Message: fmt.Sprintf("%d/%d replicas ready", s.Status.ReadyReplicas, want), Hint: "StatefulSets roll one pod at a time; check the lowest-ordinal unready pod."})
			}
		}
	}
	if dss, err := c.listDaemonSetsAcross(ctx, ns); err == nil {
		for _, d := range dss {
			if d.Status.NumberUnavailable > 0 {
				out = append(out, Issue{Severity: HealthWarn, Kind: "daemonsets", Namespace: d.Namespace, Name: d.Name, Reason: "Unavailable",
					Message: fmt.Sprintf("%d pods unavailable", d.Status.NumberUnavailable), Hint: "Check taints on nodes without a running pod and the pod errors."})
			}
		}
	}
	if pvcs, err := c.listPVCsAcross(ctx, ns); err == nil {
		for _, p := range pvcs {
			if p.Status.Phase == corev1.ClaimPending && time.Since(p.CreationTimestamp.Time) > 2*time.Minute {
				out = append(out, Issue{Severity: HealthWarn, Kind: "persistentvolumeclaims", Namespace: p.Namespace, Name: p.Name, Reason: "Pending",
					Message: "PVC not bound", Hint: "Check storageClassName exists and the provisioner is healthy (WaitForFirstConsumer binds on pod schedule)."})
			}
		}
	}
	if jobs, err := c.listJobsAcross(ctx, ns); err == nil {
		for i := range jobs {
			if j := &jobs[i]; jobFailed(j) {
				out = append(out, Issue{Severity: HealthWarn, Kind: "jobs", Namespace: j.Namespace, Name: j.Name, Reason: "JobFailed",
					Message: fmt.Sprintf("%d failed pods", j.Status.Failed), Hint: "Check logs of the job's failed pods; consider backoffLimit."})
			}
		}
	}
	if nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for i := range nodes.Items {
			n := &nodes.Items[i]
			if !NodeReady(n) {
				out = append(out, Issue{Severity: HealthErr, Kind: "nodes", Name: n.Name, Reason: "NodeNotReady",
					Message: "kubelet reports NotReady", Hint: "Check kubelet/container runtime on the node and cloud VM health."})
			}
			for _, cond := range n.Status.Conditions {
				if cond.Type != corev1.NodeReady && cond.Status == corev1.ConditionTrue {
					out = append(out, Issue{Severity: HealthWarn, Kind: "nodes", Name: n.Name, Reason: string(cond.Type),
						Message: firstLine(cond.Message), Hint: "Node under pressure; pods may be evicted. Rebalance or add capacity."})
				}
			}
		}
	}
	// Recent warning events not already explained by a rule above.
	if evs, err := c.listWarningEventsAcross(ctx, ns); err == nil {
		seen := map[string]bool{}
		for _, is := range out {
			seen[is.Kind+"/"+is.Namespace+"/"+is.Name] = true
		}
		for i := range evs {
			e := &evs[i]
			if time.Since(eventTime(e)) > 30*time.Minute {
				continue
			}
			if e.InvolvedObject.Kind == "Pod" { // a warning from before the pod turned Ready is history, not a problem
				if t, ok := readySince[e.InvolvedObject.Namespace+"/"+e.InvolvedObject.Name]; ok && !eventTime(e).After(t) {
					continue
				}
			}
			r, ok := Lookup(e.InvolvedObject.Kind)
			if !ok || seen[r.Name+"/"+e.InvolvedObject.Namespace+"/"+e.InvolvedObject.Name] {
				continue
			}
			out = append(out, Issue{Severity: HealthWarn, Kind: r.Name, Namespace: e.InvolvedObject.Namespace, Name: e.InvolvedObject.Name,
				Reason: e.Reason, Message: firstLine(e.Message), Hint: "Recent warning event (x" + fmt.Sprint(max(e.Count, 1)) + ")."})
		}
	}
	return dedupe(out), nil
}

func dedupe(in []Issue) []Issue {
	seen := map[string]bool{}
	var out []Issue
	for _, i := range in {
		if !seen[i.key()] {
			seen[i.key()] = true
			out = append(out, i)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Severity != out[b].Severity {
			return out[a].Severity > out[b].Severity // Err before Warn
		}
		return out[a].key() < out[b].key()
	})
	return out
}

// FormatIssues renders issues as plain text for an LLM prompt.
func FormatIssues(issues []Issue) string {
	var b strings.Builder
	for _, i := range issues {
		sev := "WARN"
		if i.Severity == HealthErr {
			sev = "ERROR"
		}
		obj := i.Kind + "/" + i.Name
		if i.Namespace != "" {
			obj = i.Namespace + "/" + obj
		}
		fmt.Fprintf(&b, "- [%s] %s %s: %s\n", sev, obj, i.Reason, i.Message)
	}
	return b.String()
}

// GatherContext collects everything an LLM needs to troubleshoot one object:
// cleaned manifest, events, detected issues and (for pods) recent logs.
func (c *Client) GatherContext(ctx context.Context, r Resource, ns, name string, redact bool, logLines int) (string, error) {
	var b strings.Builder
	obj, err := c.Get(ctx, r, ns, name)
	if err != nil {
		return "", err
	}
	y, _ := yaml.Marshal(Clean(obj, redact, false))
	fmt.Fprintf(&b, "## %s manifest (live)\n```yaml\n%s```\n", r.Kind, truncate(string(y), 14000))
	ev, _ := c.EventsFor(ctx, r.Kind, ns, name)
	fmt.Fprintf(&b, "\n## Events\n```\n%s```\n", truncate(ev, 4000))

	if r.Name != "pods" && !r.HasSelector() {
		return b.String(), nil
	}
	pods, err := c.PodsFor(ctx, r, ns, name)
	if err != nil {
		return b.String(), nil
	}
	if r.Name != "pods" {
		b.WriteString("\n## Pods\n")
		for i := range pods {
			p := &pods[i]
			fmt.Fprintf(&b, "- %s  status=%s  node=%s  age=%s\n", p.Name, PodStatus(p), p.Spec.NodeName, Age(p.CreationTimestamp))
		}
	}
	// Pick the most interesting pod: the first with issues, else the first one.
	var target *corev1.Pod
	var issues []Issue
	for i := range pods {
		if is := PodIssues(&pods[i]); len(is) > 0 {
			target, issues = &pods[i], is
			break
		}
	}
	if target == nil && len(pods) > 0 {
		target = &pods[0]
	}
	if target == nil {
		return b.String(), nil
	}
	if len(issues) > 0 {
		fmt.Fprintf(&b, "\n## Detected issues (pod %s)\n%s", target.Name, FormatIssues(issues))
	}
	if r.Name != "pods" {
		pev, _ := c.EventsFor(ctx, "Pod", ns, target.Name)
		fmt.Fprintf(&b, "\n## Events for pod %s\n```\n%s```\n", target.Name, truncate(pev, 3000))
	}
	for _, cs := range target.Status.ContainerStatuses {
		logs, err := c.LogsText(ctx, ns, target.Name, LogOptions{Container: cs.Name, Tail: int64(logLines)})
		if err == nil && strings.TrimSpace(logs) != "" {
			fmt.Fprintf(&b, "\n## Logs: %s/%s (last %d lines)\n```\n%s```\n", target.Name, cs.Name, logLines, truncate(logs, 8000))
		}
		if cs.RestartCount > 0 {
			prev, err := c.LogsText(ctx, ns, target.Name, LogOptions{Container: cs.Name, Tail: int64(logLines), Previous: true})
			if err == nil && strings.TrimSpace(prev) != "" {
				fmt.Fprintf(&b, "\n## Previous (crashed) logs: %s/%s\n```\n%s```\n", target.Name, cs.Name, truncate(prev, 8000))
			}
		}
	}
	return b.String(), nil
}

// truncate keeps the tail of long text (the newest log lines / most relevant part).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…(truncated)…\n" + s[len(s)-n:]
}

// Pulse is a cluster/namespace health summary.
type Pulse struct {
	NodesReady, NodesTotal                       int
	PodsRunning, PodsPending, PodsFailed, PodsOK int
	PodsTotal, Restarts                          int
	DeploysReady, DeploysTotal                   int
	WarningEvents                                int
	CPURequested, CPUAllocatable                 int64 // millicores
	MemRequested, MemAllocatable                 int64 // bytes
	TopReasons                                   map[string]int
}

// Pulse computes a quick health summary.
func (c *Client) Pulse(ctx context.Context) (Pulse, error) {
	p := Pulse{TopReasons: map[string]int{}}
	nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range nodes.Items {
			n := &nodes.Items[i]
			p.NodesTotal++
			if NodeReady(n) {
				p.NodesReady++
			}
			p.CPUAllocatable += n.Status.Allocatable.Cpu().MilliValue()
			p.MemAllocatable += n.Status.Allocatable.Memory().Value()
		}
	}
	pods, err := c.listPodsAcross(ctx, c.Namespace)
	if err != nil {
		return p, err
	}
	for i := range pods {
		pod := &pods[i]
		p.PodsTotal++
		st := PodStatus(pod)
		switch podHealth(pod, st) {
		case HealthOK:
			p.PodsRunning++
		case HealthNeutral:
			p.PodsOK++
		case HealthWarn:
			p.PodsPending++
		default:
			p.PodsFailed++
			p.TopReasons[st]++
		}
		for _, cs := range pod.Status.ContainerStatuses {
			p.Restarts += int(cs.RestartCount)
		}
		if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
			for _, ctr := range pod.Spec.Containers {
				p.CPURequested += ctr.Resources.Requests.Cpu().MilliValue()
				p.MemRequested += ctr.Resources.Requests.Memory().Value()
			}
		}
	}
	if deps, err := c.listDeploymentsAcross(ctx, c.Namespace); err == nil {
		for _, d := range deps {
			p.DeploysTotal++
			if d.Status.AvailableReplicas >= deref(d.Spec.Replicas, 1) {
				p.DeploysReady++
			}
		}
	}
	if evs, err := c.listWarningEventsAcross(ctx, c.Namespace); err == nil {
		for i := range evs {
			if time.Since(eventTime(&evs[i])) < time.Hour {
				p.WarningEvents++
			}
		}
	}
	return p, nil
}

// readyAt reports when a pod that is Ready now last became Ready.
func readyAt(p *corev1.Pod) (time.Time, bool) {
	if p.Status.Phase != corev1.PodRunning {
		return time.Time{}, false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}
