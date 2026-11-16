package kube

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// NetPolReport explains what traffic NetworkPolicies allow to and from one pod.
type NetPolReport struct {
	Pod, Namespace       string
	Policies             []string // policies selecting the pod
	IngressIsolated      bool
	EgressIsolated       bool
	Ingress, Egress      []string // allowed rules, human readable
	PeersInNamespace     []string // pods in the namespace allowed to connect (podSelector peers)
	BlockedInNamespace   int      // pods in the namespace that may NOT connect
	AllNamespacePodCount int
}

func selectorString(s *metav1.LabelSelector) string {
	if s == nil {
		return ""
	}
	sel, err := metav1.LabelSelectorAsSelector(s)
	if err != nil {
		return "?"
	}
	if sel.Empty() {
		return "all"
	}
	return sel.String()
}

func peerString(p networkingv1.NetworkPolicyPeer) string {
	switch {
	case p.IPBlock != nil:
		s := "ip " + p.IPBlock.CIDR
		if len(p.IPBlock.Except) > 0 {
			s += " except " + strings.Join(p.IPBlock.Except, ",")
		}
		return s
	case p.NamespaceSelector != nil && p.PodSelector != nil:
		return fmt.Sprintf("pods [%s] in namespaces [%s]", selectorString(p.PodSelector), selectorString(p.NamespaceSelector))
	case p.NamespaceSelector != nil:
		return fmt.Sprintf("any pod in namespaces [%s]", selectorString(p.NamespaceSelector))
	case p.PodSelector != nil:
		return fmt.Sprintf("pods [%s] in this namespace", selectorString(p.PodSelector))
	}
	return "?"
}

func portsString(ps []networkingv1.NetworkPolicyPort) string {
	if len(ps) == 0 {
		return "all ports"
	}
	var out []string
	for _, p := range ps {
		proto := "TCP"
		if p.Protocol != nil {
			proto = string(*p.Protocol)
		}
		port := "any"
		if p.Port != nil {
			port = p.Port.String()
		}
		if p.EndPort != nil {
			port += fmt.Sprintf("-%d", *p.EndPort)
		}
		out = append(out, port+"/"+proto)
	}
	return strings.Join(out, ", ")
}

func policyTypes(np *networkingv1.NetworkPolicy) (ing, egr bool) {
	if len(np.Spec.PolicyTypes) == 0 {
		return true, len(np.Spec.Egress) > 0
	}
	for _, t := range np.Spec.PolicyTypes {
		switch t {
		case networkingv1.PolicyTypeIngress:
			ing = true
		case networkingv1.PolicyTypeEgress:
			egr = true
		}
	}
	return
}

// NetworkPolicies explains the policies that apply to a pod.
func (c *Client) NetworkPolicies(ctx context.Context, ns, pod string) (*NetPolReport, error) {
	p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	nps, err := c.Clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	r := &NetPolReport{Pod: pod, Namespace: ns}
	var ingressRules []networkingv1.NetworkPolicyIngressRule
	for i := range nps.Items {
		np := &nps.Items[i]
		sel, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
		if err != nil || !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		ing, egr := policyTypes(np)
		types := []string{}
		if ing {
			types = append(types, "Ingress")
			r.IngressIsolated = true
			for _, rule := range np.Spec.Ingress {
				ingressRules = append(ingressRules, rule)
				if len(rule.From) == 0 {
					r.Ingress = append(r.Ingress, fmt.Sprintf("from anywhere on %s  (%s)", portsString(rule.Ports), np.Name))
				}
				for _, f := range rule.From {
					r.Ingress = append(r.Ingress, fmt.Sprintf("from %s on %s  (%s)", peerString(f), portsString(rule.Ports), np.Name))
				}
			}
		}
		if egr {
			types = append(types, "Egress")
			r.EgressIsolated = true
			for _, rule := range np.Spec.Egress {
				if len(rule.To) == 0 {
					r.Egress = append(r.Egress, fmt.Sprintf("to anywhere on %s  (%s)", portsString(rule.Ports), np.Name))
				}
				for _, t := range rule.To {
					r.Egress = append(r.Egress, fmt.Sprintf("to %s on %s  (%s)", peerString(t), portsString(rule.Ports), np.Name))
				}
			}
		}
		r.Policies = append(r.Policies, fmt.Sprintf("%s [%s] selects %s", np.Name, strings.Join(types, "+"), selectorString(&np.Spec.PodSelector)))
	}
	// Which pods of this namespace may connect (podSelector-only and empty peers).
	pods, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, q := range pods.Items {
			if q.Name == pod || q.Status.Phase == corev1.PodSucceeded || q.Status.Phase == corev1.PodFailed {
				continue
			}
			r.AllNamespacePodCount++
			allowed := !r.IngressIsolated
			for _, rule := range ingressRules {
				if len(rule.From) == 0 {
					allowed = true
				}
				for _, f := range rule.From {
					if f.PodSelector != nil && f.NamespaceSelector == nil {
						if s, err := metav1.LabelSelectorAsSelector(f.PodSelector); err == nil && s.Matches(labels.Set(q.Labels)) {
							allowed = true
						}
					}
					if f.NamespaceSelector != nil && selectorString(f.NamespaceSelector) == "all" && (f.PodSelector == nil || selectorString(f.PodSelector) == "all") {
						allowed = true
					}
				}
			}
			if allowed {
				r.PeersInNamespace = append(r.PeersInNamespace, q.Name)
			} else {
				r.BlockedInNamespace++
			}
		}
	}
	return r, nil
}
