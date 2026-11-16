package kube

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	authv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AccessCell is one verb on one resource for the current user.
type AccessCell struct {
	Resource, Verb string
	Allowed        bool
}

// AccessMatrixResources and AccessMatrixVerbs define the "what can I do here" grid.
var (
	AccessMatrixResources = []string{"pods", "pods/log", "pods/exec", "pods/portforward", "deployments", "statefulsets", "daemonsets", "jobs", "cronjobs",
		"services", "ingresses", "configmaps", "secrets", "persistentvolumeclaims", "events", "horizontalpodautoscalers", "networkpolicies",
		"serviceaccounts", "roles", "rolebindings", "nodes", "namespaces"}
	AccessMatrixVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete"}
)

var resourceGroup = map[string]string{"deployments": "apps", "statefulsets": "apps", "daemonsets": "apps", "jobs": "batch", "cronjobs": "batch",
	"ingresses": "networking.k8s.io", "networkpolicies": "networking.k8s.io", "horizontalpodautoscalers": "autoscaling",
	"roles": "rbac.authorization.k8s.io", "rolebindings": "rbac.authorization.k8s.io"}

var clusterScoped = map[string]bool{"nodes": true, "namespaces": true}

// CanAccess asks the API server whether the current user may do verb on resource[/sub] in ns (group-aware).
func (c *Client) CanAccess(ctx context.Context, verb, resource, ns string) (bool, string, error) {
	res, sub, _ := strings.Cut(resource, "/")
	if clusterScoped[res] {
		ns = ""
	}
	ssar := &authv1.SelfSubjectAccessReview{Spec: authv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authv1.ResourceAttributes{
		Namespace: ns, Verb: verb, Group: resourceGroup[res], Resource: res, Subresource: sub}}}
	out, err := c.Clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, ssar, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	return out.Status.Allowed, out.Status.Reason, nil
}

// AccessMatrix checks every resource × verb of the grid for the current user in ns.
func (c *Client) AccessMatrix(ctx context.Context, ns string) ([]AccessCell, error) {
	var out []AccessCell
	for _, r := range AccessMatrixResources {
		verbs := AccessMatrixVerbs
		switch r {
		case "pods/log":
			verbs = []string{"get"}
		case "pods/exec", "pods/portforward":
			verbs = []string{"create", "get"}
		}
		for _, v := range verbs {
			ok, _, err := c.CanAccess(ctx, v, r, ns)
			if err != nil {
				return nil, err
			}
			out = append(out, AccessCell{Resource: r, Verb: v, Allowed: ok})
		}
	}
	return out, nil
}

// Grant is one subject that has access, and through which binding and role.
type Grant struct {
	Subject string // User/x, Group/x, ServiceAccount/ns/x
	Binding string // RoleBinding ns/x or ClusterRoleBinding x
	Role    string
}

func ruleAllows(rule rbacv1.PolicyRule, verb, group, resource string) bool {
	match := func(list []string, v string) bool {
		for _, x := range list {
			if x == "*" || x == v {
				return true
			}
		}
		return false
	}
	return match(rule.Verbs, verb) && match(rule.APIGroups, group) && match(rule.Resources, resource)
}

func subjectString(s rbacv1.Subject) string {
	if s.Kind == "ServiceAccount" {
		return "ServiceAccount/" + s.Namespace + "/" + s.Name
	}
	return s.Kind + "/" + s.Name
}

// WhoCan lists the subjects allowed to do verb on resource in ns, from RoleBindings in ns and
// ClusterRoleBindings (needs list on roles, rolebindings, clusterroles and clusterrolebindings).
func (c *Client) WhoCan(ctx context.Context, verb, resource, ns string) ([]Grant, []string, error) {
	res, _, _ := strings.Cut(resource, "/")
	if r, ok := Lookup(res); ok {
		res = r.GVR.Resource
		if g := r.GVR.Group; g != "" {
			resourceGroup[res] = g
		}
	}
	group := resourceGroup[res]
	rbac := c.Clientset.RbacV1()
	var notes []string
	var grants []Grant
	crs := map[string][]rbacv1.PolicyRule{}
	if l, err := rbac.ClusterRoles().List(ctx, metav1.ListOptions{}); err == nil {
		for _, r := range l.Items {
			crs[r.Name] = r.Rules
		}
	} else {
		notes = append(notes, "cluster roles not readable: "+firstLine(err.Error()))
	}
	allows := func(rules []rbacv1.PolicyRule) bool {
		for _, r := range rules {
			if ruleAllows(r, verb, group, res) {
				return true
			}
		}
		return false
	}
	if ns != "" {
		roles := map[string][]rbacv1.PolicyRule{}
		if l, err := rbac.Roles(ns).List(ctx, metav1.ListOptions{}); err == nil {
			for _, r := range l.Items {
				roles[r.Name] = r.Rules
			}
		}
		rbs, err := rbac.RoleBindings(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			notes = append(notes, "role bindings in "+ns+" not readable: "+firstLine(err.Error()))
		} else {
			for _, b := range rbs.Items {
				rules := roles[b.RoleRef.Name]
				if b.RoleRef.Kind == "ClusterRole" {
					rules = crs[b.RoleRef.Name]
				}
				if allows(rules) {
					for _, s := range b.Subjects {
						grants = append(grants, Grant{subjectString(s), "RoleBinding " + ns + "/" + b.Name, b.RoleRef.Kind + "/" + b.RoleRef.Name})
					}
				}
			}
		}
	}
	if crbs, err := rbac.ClusterRoleBindings().List(ctx, metav1.ListOptions{}); err == nil {
		for _, b := range crbs.Items {
			if allows(crs[b.RoleRef.Name]) {
				for _, s := range b.Subjects {
					grants = append(grants, Grant{subjectString(s), "ClusterRoleBinding " + b.Name, "ClusterRole/" + b.RoleRef.Name})
				}
			}
		}
	} else {
		notes = append(notes, "cluster role bindings not readable: "+firstLine(err.Error()))
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].Subject < grants[j].Subject })
	if len(grants) == 0 && len(notes) > 0 {
		return nil, notes, fmt.Errorf("%s", strings.Join(notes, "; "))
	}
	return grants, notes, nil
}

// ServiceAccountRules lists the bindings and rules that apply to a service account.
func (c *Client) ServiceAccountRules(ctx context.Context, ns, sa string) ([]string, error) {
	rbac := c.Clientset.RbacV1()
	var lines []string
	describe := func(rules []rbacv1.PolicyRule) {
		for _, r := range rules {
			groups := strings.Join(r.APIGroups, ",")
			if groups == "" {
				groups = "core"
			}
			lines = append(lines, fmt.Sprintf("    %-38s %s  [%s]", strings.Join(r.Resources, ","), strings.Join(r.Verbs, ","), groups))
		}
	}
	match := func(subs []rbacv1.Subject) bool {
		for _, s := range subs {
			if s.Kind == "ServiceAccount" && s.Name == sa && (s.Namespace == ns || s.Namespace == "") {
				return true
			}
			if s.Kind == "Group" && (s.Name == "system:serviceaccounts" || s.Name == "system:serviceaccounts:"+ns) {
				return true
			}
		}
		return false
	}
	rbs, err := rbac.RoleBindings(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, b := range rbs.Items {
		if !match(b.Subjects) {
			continue
		}
		lines = append(lines, fmt.Sprintf("RoleBinding %s → %s/%s", b.Name, b.RoleRef.Kind, b.RoleRef.Name))
		if b.RoleRef.Kind == "Role" {
			if r, err := rbac.Roles(ns).Get(ctx, b.RoleRef.Name, metav1.GetOptions{}); err == nil {
				describe(r.Rules)
			}
		} else if r, err := rbac.ClusterRoles().Get(ctx, b.RoleRef.Name, metav1.GetOptions{}); err == nil {
			describe(r.Rules)
		}
	}
	if crbs, err := rbac.ClusterRoleBindings().List(ctx, metav1.ListOptions{}); err == nil {
		for _, b := range crbs.Items {
			if !match(b.Subjects) {
				continue
			}
			lines = append(lines, fmt.Sprintf("ClusterRoleBinding %s → ClusterRole/%s", b.Name, b.RoleRef.Name))
			if r, err := rbac.ClusterRoles().Get(ctx, b.RoleRef.Name, metav1.GetOptions{}); err == nil {
				describe(r.Rules)
			}
		}
	} else {
		lines = append(lines, "(cluster role bindings not readable: "+firstLine(err.Error())+")")
	}
	return lines, nil
}

var forbiddenRe = regexp.MustCompile(`cannot (\w+) resource "([^"]+)"(?: in API group "[^"]*")?(?: in the namespace "([^"]+)")?`)

// ParseForbidden extracts verb, resource and namespace from a Kubernetes "forbidden" error message.
func ParseForbidden(msg string) (verb, resource, ns string, ok bool) {
	m := forbiddenRe.FindStringSubmatch(msg)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
