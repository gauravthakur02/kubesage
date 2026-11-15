package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Finding is one policy/security issue in a workload's pod template.
type Finding struct {
	Severity  Health // HealthErr (dangerous), HealthWarn (should fix), HealthNeutral (advice)
	Check     string
	Kind      string // resource name: deployments, statefulsets, daemonsets, cronjobs, pods
	Namespace string
	Workload  string
	Container string
	Message   string
	Hint      string
}

// PolicyOptions tunes the checks.
type PolicyOptions struct {
	AllowedRegistries []string // regular expressions; empty = registry check off
	SkipNamespaces    []string
}

type workloadTemplate struct {
	kind, ns, name string
	spec           corev1.PodSpec
}

// PolicyScan checks every workload's pod template in the client's namespace (or all namespaces).
func (c *Client) PolicyScan(ctx context.Context, o PolicyOptions) ([]Finding, error) {
	ns := c.Namespace
	skip := map[string]bool{}
	for _, n := range o.SkipNamespaces {
		skip[n] = true
	}
	var regs []*regexp.Regexp
	for _, r := range o.AllowedRegistries {
		re, err := regexp.Compile(r)
		if err != nil {
			return nil, fmt.Errorf("allowed_registries: %q: %w", r, err)
		}
		regs = append(regs, re)
	}
	var tpls []workloadTemplate
	deps, err := c.listDeploymentsAcross(ctx, ns)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		tpls = append(tpls, workloadTemplate{"deployments", d.Namespace, d.Name, d.Spec.Template.Spec})
	}
	if sts, err := c.listStatefulSetsAcross(ctx, ns); err == nil {
		for _, s := range sts {
			tpls = append(tpls, workloadTemplate{"statefulsets", s.Namespace, s.Name, s.Spec.Template.Spec})
		}
	}
	if dss, err := c.listDaemonSetsAcross(ctx, ns); err == nil {
		for _, d := range dss {
			tpls = append(tpls, workloadTemplate{"daemonsets", d.Namespace, d.Name, d.Spec.Template.Spec})
		}
	}
	if cjs, err := listAcross(ctx, c, ns, func(n string) ([]batchv1.CronJob, error) {
		l, err := c.Clientset.BatchV1().CronJobs(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, cj := range cjs {
			tpls = append(tpls, workloadTemplate{"cronjobs", cj.Namespace, cj.Name, cj.Spec.JobTemplate.Spec.Template.Spec})
		}
	}
	if pods, err := c.listPodsAcross(ctx, ns); err == nil { // bare pods only; owned pods are covered by their workload
		for _, p := range pods {
			if len(p.OwnerReferences) == 0 {
				tpls = append(tpls, workloadTemplate{"pods", p.Namespace, p.Name, p.Spec})
			}
		}
	}
	var out []Finding
	for _, t := range tpls {
		if skip[t.ns] {
			continue
		}
		out = append(out, checkPodSpec(t, regs)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == HealthErr || (out[i].Severity == HealthWarn && out[j].Severity == HealthNeutral)
		}
		if out[i].Namespace+out[i].Workload != out[j].Namespace+out[j].Workload {
			return out[i].Namespace+"/"+out[i].Workload < out[j].Namespace+"/"+out[j].Workload
		}
		return out[i].Check < out[j].Check
	})
	return out, nil
}

func boolPtr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func checkPodSpec(t workloadTemplate, regs []*regexp.Regexp) []Finding {
	var out []Finding
	add := func(sev Health, check, ctr, msg, hint string) {
		out = append(out, Finding{Severity: sev, Check: check, Kind: t.kind, Namespace: t.ns, Workload: t.name, Container: ctr, Message: msg, Hint: hint})
	}
	s := t.spec
	if s.HostNetwork || s.HostPID || s.HostIPC {
		if t.kind == "daemonsets" {
			add(HealthWarn, "host namespaces", "", "uses hostNetwork/hostPID/hostIPC (usual for node agents)", "keep only if this agent really needs host access")
		} else {
			add(HealthErr, "host namespaces", "", "uses hostNetwork/hostPID/hostIPC", "remove host namespaces; only node agents (DaemonSets) normally need them")
		}
	}
	for _, v := range s.Volumes {
		if v.HostPath != nil {
			add(HealthWarn, "hostPath volume", "", "mounts host path "+v.HostPath.Path, "use a PVC, ConfigMap or emptyDir instead of the node filesystem")
		}
	}
	if boolPtr(s.AutomountServiceAccountToken, true) {
		add(HealthNeutral, "service account token", "", "API token is mounted into the pod", "set automountServiceAccountToken: false if the app does not call the Kubernetes API")
	}
	podNonRoot := s.SecurityContext != nil && boolPtr(s.SecurityContext.RunAsNonRoot, false)
	podUser := int64(-1)
	if s.SecurityContext != nil && s.SecurityContext.RunAsUser != nil {
		podUser = *s.SecurityContext.RunAsUser
	}
	for _, c := range s.Containers {
		sc := c.SecurityContext
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		if len(regs) > 0 {
			ok := false
			for _, re := range regs {
				ok = ok || re.MatchString(c.Image)
			}
			if !ok {
				add(HealthErr, "image registry", c.Name, "image "+c.Image+" is not from an allowed registry", "push the image to an approved registry")
			}
		}
		if img := c.Image[strings.LastIndex(c.Image, "/")+1:]; !strings.Contains(img, ":") && !strings.Contains(c.Image, "@") || strings.HasSuffix(c.Image, ":latest") {
			add(HealthWarn, "image tag", c.Name, "image "+c.Image+" uses :latest or no tag", "pin a version tag or digest so rollouts are reproducible")
		}
		if boolPtr(sc.Privileged, false) {
			add(HealthErr, "privileged", c.Name, "runs privileged (full access to the node)", "remove privileged: true")
		}
		user := podUser
		if sc.RunAsUser != nil {
			user = *sc.RunAsUser
		}
		nonRoot := boolPtr(sc.RunAsNonRoot, podNonRoot)
		if user == 0 || (!nonRoot && user < 0) {
			add(HealthWarn, "runs as root", c.Name, "may run as root (no runAsNonRoot / runAsUser)", "set securityContext.runAsNonRoot: true and a non-zero runAsUser")
		}
		if boolPtr(sc.AllowPrivilegeEscalation, true) {
			add(HealthWarn, "privilege escalation", c.Name, "allowPrivilegeEscalation is not false", "set securityContext.allowPrivilegeEscalation: false")
		}
		if !boolPtr(sc.ReadOnlyRootFilesystem, false) {
			add(HealthWarn, "writable root filesystem", c.Name, "root filesystem is writable", "set readOnlyRootFilesystem: true and mount emptyDir for scratch space")
		}
		dropAll := false
		if sc.Capabilities != nil {
			for _, d := range sc.Capabilities.Drop {
				dropAll = dropAll || strings.EqualFold(string(d), "ALL")
			}
		}
		if !dropAll {
			add(HealthWarn, "capabilities", c.Name, "Linux capabilities are not dropped", "set securityContext.capabilities.drop: [ALL] and add back only what is needed")
		}
		if c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero() {
			add(HealthWarn, "resource limits", c.Name, "missing CPU and/or memory limit", "set resources.limits.cpu and resources.limits.memory")
		}
		if c.Resources.Requests.Memory().IsZero() && c.Resources.Requests.Cpu().IsZero() {
			add(HealthWarn, "resource requests", c.Name, "no resource requests (BestEffort)", "set resources.requests so the scheduler can reserve capacity")
		}
		if t.kind != "cronjobs" && t.kind != "pods" {
			if c.ReadinessProbe == nil {
				add(HealthNeutral, "readiness probe", c.Name, "no readiness probe", "add a readinessProbe so traffic waits until the app is ready")
			}
			if c.LivenessProbe == nil {
				add(HealthNeutral, "liveness probe", c.Name, "no liveness probe", "add a livenessProbe so a hung process is restarted")
			}
		}
	}
	return out
}

// FormatFindings renders findings as plain text (for the CLI and AI prompts).
func FormatFindings(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		sev := map[Health]string{HealthErr: "ERROR", HealthWarn: "WARN", HealthNeutral: "INFO"}[f.Severity]
		ctr := ""
		if f.Container != "" {
			ctr = " [" + f.Container + "]"
		}
		fmt.Fprintf(&b, "- [%s] %s/%s/%s%s %s: %s\n", sev, f.Namespace, f.Kind, f.Workload, ctr, f.Check, f.Message)
	}
	return b.String()
}
