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

// StaleConfig is a workload whose pods started before a ConfigMap or Secret they use last changed.
type StaleConfig struct {
	Namespace, Workload string // Workload: Kind/name (or Pod/name)
	Pods                int
	Object              string // ConfigMap/name or Secret/name
	Changed             time.Time
	PodStarted          time.Time // oldest affected pod
	UsedAs              string    // env, volume, subPath
	NeedsRestart        bool      // env and subPath never update in a running pod
}

// lastChange is the newest managedFields time of an object (falls back to creation).
func lastChange(meta metav1.ObjectMeta) time.Time {
	t := meta.CreationTimestamp.Time
	for _, mf := range meta.ManagedFields {
		if mf.Time != nil && mf.Time.After(t) {
			t = mf.Time.Time
		}
	}
	return t
}

type configUse struct{ kind, name, how string }

func podConfigUses(spec *corev1.PodSpec) []configUse {
	var out []configUse
	subPathVols := map[string]bool{}
	for _, ct := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		for _, vm := range ct.VolumeMounts {
			if vm.SubPath != "" || vm.SubPathExpr != "" {
				subPathVols[vm.Name] = true
			}
		}
		for _, ef := range ct.EnvFrom {
			if ef.ConfigMapRef != nil {
				out = append(out, configUse{"ConfigMap", ef.ConfigMapRef.Name, "env"})
			}
			if ef.SecretRef != nil {
				out = append(out, configUse{"Secret", ef.SecretRef.Name, "env"})
			}
		}
		for _, e := range ct.Env {
			if e.ValueFrom == nil {
				continue
			}
			if r := e.ValueFrom.ConfigMapKeyRef; r != nil {
				out = append(out, configUse{"ConfigMap", r.Name, "env"})
			}
			if r := e.ValueFrom.SecretKeyRef; r != nil {
				out = append(out, configUse{"Secret", r.Name, "env"})
			}
		}
	}
	for _, v := range spec.Volumes {
		how := "volume"
		if subPathVols[v.Name] {
			how = "subPath"
		}
		switch {
		case v.ConfigMap != nil:
			out = append(out, configUse{"ConfigMap", v.ConfigMap.Name, how})
		case v.Secret != nil:
			out = append(out, configUse{"Secret", v.Secret.SecretName, how})
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					out = append(out, configUse{"ConfigMap", s.ConfigMap.Name, how})
				}
				if s.Secret != nil {
					out = append(out, configUse{"Secret", s.Secret.Name, how})
				}
			}
		}
	}
	return out
}

func podStart(p *corev1.Pod) time.Time {
	t := p.CreationTimestamp.Time
	if p.Status.StartTime != nil {
		t = p.Status.StartTime.Time
	}
	for _, cs := range p.Status.ContainerStatuses {
		if r := cs.State.Running; r != nil && r.StartedAt.After(t) {
			t = r.StartedAt.Time // a restarted container re-reads env
		}
	}
	return t
}

// StaleConfigs finds running pods that started before a ConfigMap/Secret they use changed.
// Secrets are only checked where you may list them.
func (c *Client) StaleConfigs(ctx context.Context) ([]StaleConfig, error) {
	pods, err := c.listPodsAcross(ctx, c.Namespace)
	if err != nil {
		return nil, err
	}
	changed := map[string]time.Time{} // kind/ns/name
	nsSeen := map[string]bool{}
	for _, p := range pods {
		if nsSeen[p.Namespace] {
			continue
		}
		nsSeen[p.Namespace] = true
		if l, err := c.Clientset.CoreV1().ConfigMaps(p.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
			for _, cm := range l.Items {
				changed["ConfigMap/"+cm.Namespace+"/"+cm.Name] = lastChange(cm.ObjectMeta)
			}
		}
		if l, err := c.Clientset.CoreV1().Secrets(p.Namespace).List(ctx, metav1.ListOptions{}); err == nil {
			for _, s := range l.Items {
				changed["Secret/"+s.Namespace+"/"+s.Name] = lastChange(s.ObjectMeta)
			}
		}
	}
	by := map[string]*StaleConfig{}
	var keys []string
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		started := podStart(p)
		wl := "Pod/" + p.Name
		if len(p.OwnerReferences) > 0 {
			o := p.OwnerReferences[0]
			wl = o.Kind + "/" + o.Name
			if o.Kind == "ReplicaSet" {
				if h := p.Labels["pod-template-hash"]; h != "" && strings.HasSuffix(o.Name, "-"+h) {
					wl = "Deployment/" + strings.TrimSuffix(o.Name, "-"+h)
				}
			}
		}
		for _, u := range podConfigUses(&p.Spec) {
			t, ok := changed[u.kind+"/"+p.Namespace+"/"+u.name]
			if !ok || !t.After(started.Add(5*time.Second)) {
				continue
			}
			k := p.Namespace + "|" + wl + "|" + u.kind + "/" + u.name
			s := by[k]
			if s == nil {
				s = &StaleConfig{Namespace: p.Namespace, Workload: wl, Object: u.kind + "/" + u.name, Changed: t, PodStarted: started, UsedAs: u.how}
				by[k] = s
				keys = append(keys, k)
			}
			s.Pods++
			if started.Before(s.PodStarted) {
				s.PodStarted = started
			}
			if u.how != "volume" {
				s.NeedsRestart = true
				if !strings.Contains(s.UsedAs, u.how) {
					s.UsedAs = strings.TrimPrefix(s.UsedAs+","+u.how, ",")
				}
			}
		}
	}
	out := make([]StaleConfig, 0, len(keys))
	for _, k := range keys {
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NeedsRestart != out[j].NeedsRestart {
			return out[i].NeedsRestart
		}
		return out[i].Changed.After(out[j].Changed)
	})
	return out, nil
}

// StartupPhase is one step of a pod's start.
type StartupPhase struct {
	Name     string
	Duration time.Duration
	Detail   string
}

// StartupReport explains where a pod's start time went and flags risky probes.
type StartupReport struct {
	Pod      string
	Total    time.Duration
	Ready    bool
	Phases   []StartupPhase
	Advice   []string
	Restarts int32
}

func condTime(p *corev1.Pod, t corev1.PodConditionType) (time.Time, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == t {
			return c.LastTransitionTime.Time, c.Status == corev1.ConditionTrue
		}
	}
	return time.Time{}, false
}

func probeSummary(kind string, pr *corev1.Probe) string {
	if pr == nil {
		return kind + ": none"
	}
	return fmt.Sprintf("%s: delay %ds, period %ds, timeout %ds, failures %d", kind, pr.InitialDelaySeconds, max(pr.PeriodSeconds, 10), max(pr.TimeoutSeconds, 1), max(pr.FailureThreshold, 3))
}

// StartupBreakdown measures scheduling, image pulls, init containers and readiness for a pod.
func (c *Client) StartupBreakdown(ctx context.Context, ns, name string) (*StartupReport, error) {
	p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	r := &StartupReport{Pod: name}
	created := p.CreationTimestamp.Time
	sched, schedOK := condTime(p, corev1.PodScheduled)
	inited, initOK := condTime(p, corev1.PodInitialized)
	ready, readyOK := condTime(p, corev1.PodReady)
	r.Ready = readyOK
	add := func(n string, from, to time.Time, detail string) {
		if from.IsZero() || to.IsZero() || to.Before(from) {
			return
		}
		r.Phases = append(r.Phases, StartupPhase{n, to.Sub(from), detail})
	}
	if schedOK {
		add("scheduling", created, sched, "until a node was picked ("+nonEmptyStr(p.Spec.NodeName, "?")+")")
	} else {
		r.Phases = append(r.Phases, StartupPhase{"scheduling", time.Since(created), "still not scheduled: check events for Insufficient cpu/memory, taints or affinity"})
	}
	// Image pulls from events ("Successfully pulled image … in 3.2s").
	if evs, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + name}); err == nil {
		for _, e := range evs.Items {
			if e.Reason != "Pulled" || !strings.Contains(e.Message, " in ") {
				continue
			}
			msg := e.Message
			img := ""
			if i := strings.Index(msg, `image "`); i >= 0 {
				img = strings.SplitN(msg[i+7:], `"`, 2)[0]
			}
			dur := ""
			if i := strings.LastIndex(msg, " in "); i >= 0 {
				dur = strings.Fields(msg[i+4:])[0]
			}
			if d, err := time.ParseDuration(strings.TrimSuffix(dur, ".")); err == nil {
				r.Phases = append(r.Phases, StartupPhase{"image pull", d, img})
				if d > 60*time.Second {
					r.Advice = append(r.Advice, fmt.Sprintf("pulling %s took %s: use a smaller image, a registry closer to the cluster, or pre-pulled images", img, d.Round(time.Second)))
				}
			}
		}
	}
	for _, ic := range p.Status.InitContainerStatuses {
		if t := ic.State.Terminated; t != nil {
			add("init "+ic.Name, t.StartedAt.Time, t.FinishedAt.Time, fmt.Sprintf("exit %d", t.ExitCode))
		} else if w := ic.State.Waiting; w != nil {
			r.Phases = append(r.Phases, StartupPhase{"init " + ic.Name, 0, "waiting: " + w.Reason})
		} else if rn := ic.State.Running; rn != nil {
			r.Phases = append(r.Phases, StartupPhase{"init " + ic.Name, time.Since(rn.StartedAt.Time), "still running"})
		}
	}
	var lastStart time.Time
	for _, cs := range p.Status.ContainerStatuses {
		r.Restarts += cs.RestartCount
		if rn := cs.State.Running; rn != nil && rn.StartedAt.After(lastStart) {
			lastStart = rn.StartedAt.Time
		}
	}
	if initOK && !lastStart.IsZero() {
		add("container start", inited, lastStart, "init done → containers running")
	}
	if readyOK && !lastStart.IsZero() {
		add("readiness", lastStart, ready, "containers running → Ready (readiness probe)")
	}
	end := ready
	if !readyOK {
		end = time.Now()
	}
	r.Total = end.Sub(created)
	startupTime := time.Duration(0)
	if readyOK && !lastStart.IsZero() {
		startupTime = ready.Sub(lastStart)
	}
	// Probe advice per container.
	for _, ct := range p.Spec.Containers {
		lp, rp, sp := ct.LivenessProbe, ct.ReadinessProbe, ct.StartupProbe
		r.Advice = append(r.Advice, fmt.Sprintf("%s · %s · %s · %s", ct.Name, probeSummary("liveness", lp), probeSummary("readiness", rp), probeSummary("startup", sp)))
		if lp != nil && sp == nil {
			budget := time.Duration(lp.InitialDelaySeconds+max(lp.PeriodSeconds, 10)*max(lp.FailureThreshold, 3)) * time.Second
			if startupTime > 0 && startupTime > budget*8/10 {
				r.Advice = append(r.Advice, fmt.Sprintf("⚠ %s: the app needs ~%s to become ready but liveness kills it after ~%s: add a startupProbe", ct.Name, startupTime.Round(time.Second), budget))
			} else {
				r.Advice = append(r.Advice, fmt.Sprintf("%s: liveness without a startupProbe; slow starts (cold caches, migrations) may be killed", ct.Name))
			}
		}
		if lp != nil && max(lp.TimeoutSeconds, 1) == 1 && lp.HTTPGet != nil {
			r.Advice = append(r.Advice, fmt.Sprintf("%s: liveness timeout is 1s; a short GC pause or load spike restarts the container. Consider 3–5s", ct.Name))
		}
		if lp != nil && rp != nil && lp.HTTPGet != nil && rp.HTTPGet != nil && lp.HTTPGet.Path == rp.HTTPGet.Path && lp.HTTPGet.Port == rp.HTTPGet.Port {
			r.Advice = append(r.Advice, fmt.Sprintf("%s: liveness and readiness hit the same endpoint; if it checks dependencies, an outage restarts every pod", ct.Name))
		}
		if rp == nil {
			r.Advice = append(r.Advice, fmt.Sprintf("%s: no readinessProbe; traffic arrives before the app is ready", ct.Name))
		}
	}
	if r.Restarts > 0 {
		if evs, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + name + ",reason=Unhealthy"}); err == nil && len(evs.Items) > 0 {
			r.Advice = append(r.Advice, fmt.Sprintf("⚠ %d restarts and probe failures: %s", r.Restarts, firstLine(evs.Items[0].Message)))
		}
	}
	return r, nil
}
