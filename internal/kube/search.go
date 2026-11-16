package kube

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SearchHit is one object matching a search.
type SearchHit struct {
	Context, Kind, Resource, Namespace, Name, Match string
}

// matcher matches a query against a name, label pairs, images and IPs.
// "app=web" matches labels, anything else is a case-insensitive substring.
func matcher(q string) func(name string, labels map[string]string, extra ...string) string {
	q = strings.TrimSpace(q)
	lk, lv, isLabel := strings.Cut(q, "=")
	lq := strings.ToLower(q)
	return func(name string, labels map[string]string, extra ...string) string {
		if isLabel {
			if v, ok := labels[lk]; ok && (lv == "" || v == lv) {
				return "label " + lk + "=" + v
			}
			return ""
		}
		if strings.Contains(strings.ToLower(name), lq) {
			return "name"
		}
		for _, e := range extra {
			if e != "" && strings.Contains(strings.ToLower(e), lq) {
				return e
			}
		}
		return ""
	}
}

// Search finds pods, workloads, services, ingresses and configmaps in all namespaces whose name,
// label (key=value), image, IP or host matches q. Namespaces you cannot list are skipped.
func (c *Client) Search(ctx context.Context, q string) ([]SearchHit, error) {
	m := matcher(q)
	all := c.WithNamespace(AllNamespaces)
	var hits []SearchHit
	add := func(kind, res, ns, name, match string) {
		if match != "" {
			hits = append(hits, SearchHit{c.Context, kind, res, ns, name, match})
		}
	}
	pods, err := all.listPodsAcross(ctx, AllNamespaces)
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		extra := []string{p.Status.PodIP, p.Spec.NodeName}
		for _, ct := range p.Spec.Containers {
			extra = append(extra, ct.Image)
		}
		add("Pod", "pods", p.Namespace, p.Name, m(p.Name, p.Labels, extra...))
	}
	if ds, err := all.listDeploymentsAcross(ctx, AllNamespaces); err == nil {
		for _, d := range ds {
			var imgs []string
			for _, ct := range d.Spec.Template.Spec.Containers {
				imgs = append(imgs, ct.Image)
			}
			add("Deployment", "deployments", d.Namespace, d.Name, m(d.Name, d.Labels, imgs...))
		}
	}
	if ss, err := all.listStatefulSetsAcross(ctx, AllNamespaces); err == nil {
		for _, s := range ss {
			var imgs []string
			for _, ct := range s.Spec.Template.Spec.Containers {
				imgs = append(imgs, ct.Image)
			}
			add("StatefulSet", "statefulsets", s.Namespace, s.Name, m(s.Name, s.Labels, imgs...))
		}
	}
	if svcs, err := listAcross(ctx, all, AllNamespaces, func(ns string) ([]SearchHit, error) {
		l, err := c.Clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		var out []SearchHit
		for _, s := range l.Items {
			extra := append([]string{s.Spec.ClusterIP, s.Spec.ExternalName}, s.Spec.ExternalIPs...)
			for _, ing := range s.Status.LoadBalancer.Ingress {
				extra = append(extra, ing.IP, ing.Hostname)
			}
			if mt := m(s.Name, s.Labels, extra...); mt != "" {
				out = append(out, SearchHit{c.Context, "Service", "services", s.Namespace, s.Name, mt})
			}
		}
		return out, nil
	}); err == nil {
		hits = append(hits, svcs...)
	}
	if ings, err := listAcross(ctx, all, AllNamespaces, func(ns string) ([]SearchHit, error) {
		l, err := c.Clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		var out []SearchHit
		for _, i := range l.Items {
			var hosts []string
			for _, r := range i.Spec.Rules {
				hosts = append(hosts, r.Host)
			}
			if mt := m(i.Name, i.Labels, hosts...); mt != "" {
				out = append(out, SearchHit{c.Context, "Ingress", "ingresses", i.Namespace, i.Name, mt})
			}
		}
		return out, nil
	}); err == nil {
		hits = append(hits, ings...)
	}
	if cms, err := listAcross(ctx, all, AllNamespaces, func(ns string) ([]SearchHit, error) {
		l, err := c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		var out []SearchHit
		for _, cm := range l.Items {
			if mt := m(cm.Name, cm.Labels); mt != "" {
				out = append(out, SearchHit{c.Context, "ConfigMap", "configmaps", cm.Namespace, cm.Name, mt})
			}
		}
		return out, nil
	}); err == nil {
		hits = append(hits, cms...)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Kind < hits[j].Kind })
	return hits, nil
}
