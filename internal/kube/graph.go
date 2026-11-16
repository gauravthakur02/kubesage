package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// GraphNode is one object in a relationship tree.
type GraphNode struct {
	Kind, Namespace, Name string
	Status                string
	Health                Health
	Note                  string // e.g. "managed by Argo CD app x"
	Children              []*GraphNode
}

func (n *GraphNode) add(c *GraphNode) *GraphNode { n.Children = append(n.Children, c); return c }

// nsObjects is one namespace's objects, listed once for graph building.
type nsObjects struct {
	pods     []corev1.Pod
	rs       []appsv1.ReplicaSet
	deploys  []appsv1.Deployment
	sts      []appsv1.StatefulSet
	ds       []appsv1.DaemonSet
	svcs     []corev1.Service
	ings     []networkingv1.Ingress
	pvcs     map[string]corev1.PersistentVolumeClaim
	hpaNames map[string]string // "Kind/name" → hpa name
}

func (c *Client) loadNS(ctx context.Context, ns string) (*nsObjects, error) {
	o := &nsObjects{pvcs: map[string]corev1.PersistentVolumeClaim{}, hpaNames: map[string]string{}}
	cs := c.Clientset
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	o.pods = pods.Items
	if l, err := cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{}); err == nil {
		o.rs = l.Items
	}
	if l, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{}); err == nil {
		o.deploys = l.Items
	}
	if l, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{}); err == nil {
		o.sts = l.Items
	}
	if l, err := cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{}); err == nil {
		o.ds = l.Items
	}
	if l, err := cs.CoreV1().Services(ns).List(ctx, metav1.ListOptions{}); err == nil {
		o.svcs = l.Items
	}
	if l, err := cs.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{}); err == nil {
		o.ings = l.Items
	}
	if l, err := cs.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range l.Items {
			o.pvcs[p.Name] = p
		}
	}
	if l, err := cs.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, h := range l.Items {
			o.hpaNames[h.Spec.ScaleTargetRef.Kind+"/"+h.Spec.ScaleTargetRef.Name] = fmt.Sprintf("%s (%d/%d-%d)", h.Name, h.Status.CurrentReplicas, ptrOr(h.Spec.MinReplicas, 1), h.Spec.MaxReplicas)
		}
	}
	return o, nil
}

func ptrOr(p *int32, d int32) int32 {
	if p == nil {
		return d
	}
	return *p
}

func ownedByName(refs []metav1.OwnerReference, kind, name string) bool {
	for _, r := range refs {
		if r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}

func podNode(p *corev1.Pod) *GraphNode {
	issues := PodIssues(p)
	h, status := HealthOK, string(p.Status.Phase)
	ready, total := 0, len(p.Spec.Containers)
	restarts := int32(0)
	for _, s := range p.Status.ContainerStatuses {
		if s.Ready {
			ready++
		}
		restarts += s.RestartCount
	}
	status = fmt.Sprintf("%s %d/%d", status, ready, total)
	if restarts > 0 {
		status += fmt.Sprintf(" · %d restarts", restarts)
	}
	if p.Status.Phase == corev1.PodSucceeded {
		h = HealthNeutral
	} else if len(issues) > 0 {
		h, status = HealthErr, issues[0].Reason
	} else if ready < total {
		h = HealthWarn
	}
	n := &GraphNode{Kind: "Pod", Namespace: p.Namespace, Name: p.Name, Status: status, Health: h}
	if p.Spec.NodeName != "" {
		n.Note = "on " + p.Spec.NodeName
	}
	return n
}

// configRefs lists ConfigMaps, Secrets and PVCs a pod template uses.
func configRefs(spec *corev1.PodSpec, ns string, o *nsObjects) []*GraphNode {
	seen := map[string]bool{}
	var out []*GraphNode
	add := func(kind, name string) {
		if name == "" || seen[kind+name] {
			return
		}
		seen[kind+name] = true
		n := &GraphNode{Kind: kind, Namespace: ns, Name: name, Health: HealthNeutral}
		if kind == "PersistentVolumeClaim" {
			if pvc, ok := o.pvcs[name]; ok {
				n.Status = string(pvc.Status.Phase)
				if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
					n.Status += " " + q.String()
				}
				if pvc.Status.Phase == corev1.ClaimBound {
					n.Health = HealthOK
				} else {
					n.Health = HealthWarn
				}
			} else {
				n.Status, n.Health = "missing", HealthErr
			}
		}
		out = append(out, n)
	}
	for _, v := range spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			add("ConfigMap", v.ConfigMap.Name)
		case v.Secret != nil:
			add("Secret", v.Secret.SecretName)
		case v.PersistentVolumeClaim != nil:
			add("PersistentVolumeClaim", v.PersistentVolumeClaim.ClaimName)
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					add("ConfigMap", s.ConfigMap.Name)
				}
				if s.Secret != nil {
					add("Secret", s.Secret.Name)
				}
			}
		}
	}
	for _, ct := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		for _, ef := range ct.EnvFrom {
			if ef.ConfigMapRef != nil {
				add("ConfigMap", ef.ConfigMapRef.Name)
			}
			if ef.SecretRef != nil {
				add("Secret", ef.SecretRef.Name)
			}
		}
		for _, e := range ct.Env {
			if e.ValueFrom == nil {
				continue
			}
			if e.ValueFrom.ConfigMapKeyRef != nil {
				add("ConfigMap", e.ValueFrom.ConfigMapKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef != nil {
				add("Secret", e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	for _, s := range spec.ImagePullSecrets {
		add("Secret", s.Name)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func workloadHealth(ready, want int32) (Health, string) {
	s := fmt.Sprintf("%d/%d ready", ready, want)
	switch {
	case want == 0:
		return HealthNeutral, "scaled to 0"
	case ready == want:
		return HealthOK, s
	case ready == 0:
		return HealthErr, s
	}
	return HealthWarn, s
}

func (o *nsObjects) podsOwned(kind, name string) []*GraphNode {
	var out []*GraphNode
	for i := range o.pods {
		if ownedByName(o.pods[i].OwnerReferences, kind, name) {
			out = append(out, podNode(&o.pods[i]))
		}
	}
	return out
}

// workloadNode builds Deployment → ReplicaSets → Pods (+ config, HPA, GitOps).
func (o *nsObjects) workloadNode(kind, name string) *GraphNode {
	n := &GraphNode{Kind: kind, Name: name}
	var spec *corev1.PodSpec
	var meta metav1.ObjectMeta
	switch kind {
	case "Deployment":
		for i := range o.deploys {
			d := &o.deploys[i]
			if d.Name != name {
				continue
			}
			meta, spec = d.ObjectMeta, &d.Spec.Template.Spec
			n.Health, n.Status = workloadHealth(d.Status.ReadyReplicas, ptrOr(d.Spec.Replicas, 1))
			rss := []*appsv1.ReplicaSet{}
			for j := range o.rs {
				if ownedByName(o.rs[j].OwnerReferences, "Deployment", name) {
					rss = append(rss, &o.rs[j])
				}
			}
			sort.Slice(rss, func(a, b int) bool { return rss[a].CreationTimestamp.After(rss[b].CreationTimestamp.Time) })
			for k, rs := range rss {
				want := ptrOr(rs.Spec.Replicas, 0)
				if want == 0 && k > 0 {
					continue // old, scaled-down revision
				}
				h, st := workloadHealth(rs.Status.ReadyReplicas, want)
				rn := n.add(&GraphNode{Kind: "ReplicaSet", Namespace: rs.Namespace, Name: rs.Name, Health: h,
					Status: st + " · rev " + rs.Annotations["deployment.kubernetes.io/revision"]})
				rn.Children = o.podsOwned("ReplicaSet", rs.Name)
			}
		}
	case "StatefulSet":
		for i := range o.sts {
			s := &o.sts[i]
			if s.Name == name {
				meta, spec = s.ObjectMeta, &s.Spec.Template.Spec
				n.Health, n.Status = workloadHealth(s.Status.ReadyReplicas, ptrOr(s.Spec.Replicas, 1))
				n.Children = o.podsOwned("StatefulSet", name)
				for _, t := range s.Spec.VolumeClaimTemplates {
					for pvc := range o.pvcs {
						if strings.HasPrefix(pvc, t.Name+"-"+name+"-") {
							spec.Volumes = append(spec.Volumes, corev1.Volume{Name: pvc, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc}}})
						}
					}
				}
			}
		}
	case "DaemonSet":
		for i := range o.ds {
			d := &o.ds[i]
			if d.Name == name {
				meta, spec = d.ObjectMeta, &d.Spec.Template.Spec
				n.Health, n.Status = workloadHealth(d.Status.NumberReady, d.Status.DesiredNumberScheduled)
				n.Children = o.podsOwned("DaemonSet", name)
			}
		}
	}
	if spec == nil {
		n.Status, n.Health = "not found", HealthErr
		return n
	}
	n.Namespace = meta.Namespace
	n.Note = GitOpsOwner(meta.Labels, meta.Annotations)
	if h, ok := o.hpaNames[kind+"/"+name]; ok {
		n.add(&GraphNode{Kind: "HPA", Namespace: n.Namespace, Name: h, Health: HealthNeutral})
	}
	n.Children = append(n.Children, configRefs(spec, n.Namespace, o)...)
	return n
}

// ownerWorkload walks a pod up to its Deployment/StatefulSet/DaemonSet/Job.
func (o *nsObjects) ownerWorkload(p *corev1.Pod) (kind, name string) {
	for _, r := range p.OwnerReferences {
		if r.Kind == "ReplicaSet" {
			for _, rs := range o.rs {
				if rs.Name == r.Name {
					for _, rr := range rs.OwnerReferences {
						if rr.Kind == "Deployment" {
							return "Deployment", rr.Name
						}
					}
				}
			}
			return "ReplicaSet", r.Name
		}
		return r.Kind, r.Name
	}
	return "", ""
}

// serviceNode builds Service → workloads → … for the pods it selects.
func (o *nsObjects) serviceNode(svc *corev1.Service) *GraphNode {
	n := &GraphNode{Kind: "Service", Namespace: svc.Namespace, Name: svc.Name, Health: HealthNeutral}
	ports := []string{}
	for _, p := range svc.Spec.Ports {
		ports = append(ports, fmt.Sprintf("%d→%s", p.Port, p.TargetPort.String()))
	}
	n.Status = string(svc.Spec.Type) + " " + strings.Join(ports, ",")
	if len(svc.Spec.Selector) == 0 {
		n.Note = "no selector (external endpoints)"
		return n
	}
	sel := labels.SelectorFromSet(svc.Spec.Selector)
	ready, total := 0, 0
	seen := map[string]bool{}
	for i := range o.pods {
		p := &o.pods[i]
		if !sel.Matches(labels.Set(p.Labels)) || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		total++
		if podReady(p) {
			ready++
		}
		kind, name := o.ownerWorkload(p)
		key := kind + "/" + name
		if kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" {
			if !seen[key] {
				seen[key] = true
				n.add(o.workloadNode(kind, name))
			}
		} else if !seen[p.Name] {
			seen[p.Name] = true
			n.add(podNode(p))
		}
	}
	switch {
	case total == 0:
		n.Health, n.Note = HealthErr, "selects no pods: traffic goes nowhere"
	case ready == 0:
		n.Health, n.Note = HealthErr, fmt.Sprintf("0/%d endpoints ready", total)
	case ready < total:
		n.Health, n.Note = HealthWarn, fmt.Sprintf("%d/%d endpoints ready", ready, total)
	default:
		n.Health, n.Note = HealthOK, fmt.Sprintf("%d endpoints ready", total)
	}
	return n
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (o *nsObjects) ingressNode(ing *networkingv1.Ingress) *GraphNode {
	n := &GraphNode{Kind: "Ingress", Namespace: ing.Namespace, Name: ing.Name, Health: HealthOK}
	hosts := []string{}
	backends := map[string]string{}
	addBackend := func(b *networkingv1.IngressBackend, path string) {
		if b != nil && b.Service != nil {
			backends[b.Service.Name] += path + " "
		}
	}
	addBackend(ing.Spec.DefaultBackend, "(default)")
	for _, r := range ing.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
		if r.HTTP != nil {
			for _, p := range r.HTTP.Paths {
				addBackend(&p.Backend, r.Host+p.Path)
			}
		}
	}
	n.Status = strings.Join(hosts, ",")
	names := make([]string, 0, len(backends))
	for s := range backends {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		var svc *corev1.Service
		for i := range o.svcs {
			if o.svcs[i].Name == s {
				svc = &o.svcs[i]
			}
		}
		if svc == nil {
			n.add(&GraphNode{Kind: "Service", Namespace: ing.Namespace, Name: s, Status: "missing", Health: HealthErr, Note: "paths " + strings.TrimSpace(backends[s])})
			n.Health = HealthErr
			continue
		}
		c := n.add(o.serviceNode(svc))
		c.Note = strings.TrimSpace("paths " + strings.TrimSpace(backends[s]) + " · " + c.Note)
		if c.Health == HealthErr {
			n.Health = HealthErr
		}
	}
	return n
}

// usedBy finds the Services and Ingresses in front of a workload's pods.
func (o *nsObjects) usedBy(podLabels []labels.Set) []*GraphNode {
	var out []*GraphNode
	for i := range o.svcs {
		s := &o.svcs[i]
		if len(s.Spec.Selector) == 0 {
			continue
		}
		sel := labels.SelectorFromSet(s.Spec.Selector)
		match := false
		for _, l := range podLabels {
			if sel.Matches(l) {
				match = true
			}
		}
		if !match {
			continue
		}
		sn := &GraphNode{Kind: "Service", Namespace: s.Namespace, Name: s.Name, Health: HealthNeutral, Status: string(s.Spec.Type)}
		for j := range o.ings {
			for _, r := range o.ings[j].Spec.Rules {
				if r.HTTP == nil {
					continue
				}
				for _, p := range r.HTTP.Paths {
					if p.Backend.Service != nil && p.Backend.Service.Name == s.Name {
						sn.add(&GraphNode{Kind: "Ingress", Namespace: s.Namespace, Name: o.ings[j].Name, Status: r.Host + p.Path, Health: HealthNeutral})
					}
				}
			}
		}
		out = append(out, sn)
	}
	return out
}

// Relations builds the relationship tree around one object. The second tree lists
// what sits in front of it (Services and Ingresses), when that applies.
func (c *Client) Relations(ctx context.Context, kind, ns, name string) (*GraphNode, *GraphNode, error) {
	o, err := c.loadNS(ctx, ns)
	if err != nil {
		return nil, nil, err
	}
	front := &GraphNode{Kind: "front", Name: "reached through"}
	podLabelsOf := func(owner func(p *corev1.Pod) bool) []labels.Set {
		var out []labels.Set
		for i := range o.pods {
			if owner(&o.pods[i]) {
				out = append(out, labels.Set(o.pods[i].Labels))
			}
		}
		return out
	}
	switch kind {
	case "Ingress":
		for i := range o.ings {
			if o.ings[i].Name == name {
				return o.ingressNode(&o.ings[i]), nil, nil
			}
		}
	case "Service":
		for i := range o.svcs {
			if o.svcs[i].Name == name {
				n := o.serviceNode(&o.svcs[i])
				front.Children = o.usedBy(nil)
				for j := range o.ings {
					ing := o.ingressNode(&o.ings[j])
					for _, ch := range ing.Children {
						if ch.Name == name {
							front.add(&GraphNode{Kind: "Ingress", Namespace: ns, Name: o.ings[j].Name, Status: ing.Status, Health: HealthNeutral})
						}
					}
				}
				return n, front, nil
			}
		}
	case "Deployment", "StatefulSet", "DaemonSet":
		n := o.workloadNode(kind, name)
		front.Children = o.usedBy(podLabelsOf(func(p *corev1.Pod) bool { k, nm := o.ownerWorkload(p); return k == kind && nm == name }))
		return n, front, nil
	case "ReplicaSet":
		for _, rs := range o.rs {
			if rs.Name == name {
				for _, r := range rs.OwnerReferences {
					if r.Kind == "Deployment" {
						return c.Relations(ctx, "Deployment", ns, r.Name)
					}
				}
				h, st := workloadHealth(rs.Status.ReadyReplicas, ptrOr(rs.Spec.Replicas, 0))
				n := &GraphNode{Kind: "ReplicaSet", Namespace: ns, Name: name, Health: h, Status: st}
				n.Children = o.podsOwned("ReplicaSet", name)
				return n, nil, nil
			}
		}
	case "Pod":
		for i := range o.pods {
			p := &o.pods[i]
			if p.Name != name {
				continue
			}
			if k, nm := o.ownerWorkload(p); k == "Deployment" || k == "StatefulSet" || k == "DaemonSet" {
				n := o.workloadNode(k, nm)
				front.Children = o.usedBy([]labels.Set{labels.Set(p.Labels)})
				return n, front, nil
			}
			n := podNode(p)
			n.Children = configRefs(&p.Spec, ns, o)
			front.Children = o.usedBy([]labels.Set{labels.Set(p.Labels)})
			return n, front, nil
		}
	default:
		return nil, nil, fmt.Errorf("relationships are available for ingresses, services, deployments, statefulsets, daemonsets, replicasets and pods")
	}
	return nil, nil, fmt.Errorf("%s %s/%s not found", strings.ToLower(kind), ns, name)
}

// GitOpsOwner reports which GitOps tool manages an object, from its labels/annotations ("" if none).
func GitOpsOwner(lbl, ann map[string]string) string {
	switch {
	case ann["argocd.argoproj.io/tracking-id"] != "":
		app, _, _ := strings.Cut(ann["argocd.argoproj.io/tracking-id"], ":")
		return "managed by Argo CD app " + app
	case lbl["argocd.argoproj.io/instance"] != "":
		return "managed by Argo CD app " + lbl["argocd.argoproj.io/instance"]
	case lbl["kustomize.toolkit.fluxcd.io/name"] != "":
		return "managed by Flux Kustomization " + lbl["kustomize.toolkit.fluxcd.io/namespace"] + "/" + lbl["kustomize.toolkit.fluxcd.io/name"]
	case lbl["helm.toolkit.fluxcd.io/name"] != "":
		return "managed by Flux HelmRelease " + lbl["helm.toolkit.fluxcd.io/namespace"] + "/" + lbl["helm.toolkit.fluxcd.io/name"]
	case lbl["app.kubernetes.io/managed-by"] == "Helm":
		return "installed by Helm release " + ann["meta.helm.sh/release-name"]
	}
	return ""
}
