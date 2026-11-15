package kube

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func listPVs(ctx context.Context, c *Client, _ string) (Table, error) {
	list, err := c.Clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "CAPACITY", "ACCESS", "RECLAIM", "STATUS", "CLAIM", "STORAGECLASS", "AGE"}}
	for _, pv := range list.Items {
		capacity := pv.Spec.Capacity[corev1.ResourceStorage]
		var modes []string
		for _, m := range pv.Spec.AccessModes {
			modes = append(modes, map[corev1.PersistentVolumeAccessMode]string{"ReadWriteOnce": "RWO", "ReadOnlyMany": "ROX", "ReadWriteMany": "RWX", "ReadWriteOncePod": "RWOP"}[m])
		}
		claim := ""
		if pv.Spec.ClaimRef != nil {
			claim = pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
		}
		h := HealthOK
		switch pv.Status.Phase {
		case corev1.VolumeFailed:
			h = HealthErr
		case corev1.VolumeReleased, corev1.VolumePending:
			h = HealthWarn
		case corev1.VolumeAvailable:
			h = HealthNeutral
		}
		t.Rows = append(t.Rows, Row{Name: pv.Name, Health: h, Cells: []string{pv.Name, capacity.String(), strings.Join(modes, ","),
			string(pv.Spec.PersistentVolumeReclaimPolicy), string(pv.Status.Phase), claim, pv.Spec.StorageClassName, Age(pv.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listStorageClasses(ctx context.Context, c *Client, _ string) (Table, error) {
	list, err := c.Clientset.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "PROVISIONER", "RECLAIM", "BINDING", "EXPANSION", "AGE"}}
	for _, sc := range list.Items {
		name := sc.Name
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			name += " (default)"
		}
		reclaim, binding := "Delete", "Immediate"
		if sc.ReclaimPolicy != nil {
			reclaim = string(*sc.ReclaimPolicy)
		}
		if sc.VolumeBindingMode != nil {
			binding = string(*sc.VolumeBindingMode)
		}
		exp := sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion
		t.Rows = append(t.Rows, Row{Name: sc.Name, Health: HealthNeutral, Cells: []string{name, sc.Provisioner, reclaim, binding, fmt.Sprint(exp), Age(sc.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listNetworkPolicies(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "POD-SELECTOR", "TYPES", "INGRESS RULES", "EGRESS RULES", "AGE"}}
	for _, np := range list.Items {
		sel := metav1.FormatLabelSelector(&np.Spec.PodSelector)
		if sel == "<none>" {
			sel = "(all pods)"
		}
		var types []string
		for _, pt := range np.Spec.PolicyTypes {
			types = append(types, string(pt))
		}
		t.Rows = append(t.Rows, Row{Namespace: np.Namespace, Name: np.Name, Health: HealthNeutral, Cells: []string{np.Name, sel,
			strings.Join(types, ","), fmt.Sprint(len(np.Spec.Ingress)), fmt.Sprint(len(np.Spec.Egress)), Age(np.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listEndpoints(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().Endpoints(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "READY", "NOT READY", "ENDPOINTS", "AGE"}}
	for _, ep := range list.Items {
		ready, notReady := 0, 0
		var addrs []string
		for _, ss := range ep.Subsets {
			ready += len(ss.Addresses)
			notReady += len(ss.NotReadyAddresses)
			for _, a := range ss.Addresses {
				for _, p := range ss.Ports {
					addrs = append(addrs, fmt.Sprintf("%s:%d", a.IP, p.Port))
				}
			}
		}
		h := HealthOK
		switch {
		case ready == 0 && notReady == 0:
			h = HealthWarn
		case ready == 0:
			h = HealthErr
		case notReady > 0:
			h = HealthWarn
		}
		if len(addrs) > 4 {
			addrs = append(addrs[:4], fmt.Sprintf("+%d more", len(addrs)-4))
		}
		t.Rows = append(t.Rows, Row{Namespace: ep.Namespace, Name: ep.Name, Health: h, Cells: []string{ep.Name, fmt.Sprint(ready),
			fmt.Sprint(notReady), strings.Join(addrs, ","), Age(ep.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listPDBs(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "MIN AVAILABLE", "MAX UNAVAILABLE", "ALLOWED DISRUPTIONS", "HEALTHY", "AGE"}}
	for _, p := range list.Items {
		minA, maxU := "-", "-"
		if p.Spec.MinAvailable != nil {
			minA = p.Spec.MinAvailable.String()
		}
		if p.Spec.MaxUnavailable != nil {
			maxU = p.Spec.MaxUnavailable.String()
		}
		h := HealthOK
		if p.Status.DisruptionsAllowed == 0 {
			h = HealthWarn // node drains will block
		}
		t.Rows = append(t.Rows, Row{Namespace: p.Namespace, Name: p.Name, Health: h, Cells: []string{p.Name, minA, maxU,
			fmt.Sprint(p.Status.DisruptionsAllowed), fmt.Sprintf("%d/%d", p.Status.CurrentHealthy, p.Status.ExpectedPods), Age(p.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listServiceAccounts(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().ServiceAccounts(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "SECRETS", "AUTOMOUNT TOKEN", "WORKLOAD IDENTITY", "AGE"}}
	for _, sa := range list.Items {
		auto := "default"
		if sa.AutomountServiceAccountToken != nil {
			auto = fmt.Sprint(*sa.AutomountServiceAccountToken)
		}
		wi := sa.Annotations["azure.workload.identity/client-id"]
		if wi == "" {
			wi = sa.Annotations["eks.amazonaws.com/role-arn"]
		}
		if wi == "" {
			wi = sa.Annotations["iam.gke.io/gcp-service-account"]
		}
		t.Rows = append(t.Rows, Row{Namespace: sa.Namespace, Name: sa.Name, Health: HealthNeutral, Cells: []string{sa.Name,
			fmt.Sprint(len(sa.Secrets)), auto, wi, Age(sa.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

// ruleSummary renders RBAC rules compactly, flagging wildcards.
func ruleSummary(rules []rbacv1.PolicyRule) (string, Health) {
	h := HealthNeutral
	var parts []string
	for _, r := range rules {
		verbs := strings.Join(r.Verbs, ",")
		res := strings.Join(append(append([]string{}, r.Resources...), r.NonResourceURLs...), ",")
		if strings.Contains(verbs, "*") && strings.Contains(res, "*") {
			h = HealthWarn // full admin-like rule
		}
		parts = append(parts, verbs+" "+res)
	}
	return strings.Join(parts, "; "), h
}

func listRoles(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.RbacV1().Roles(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "RULES", "AGE"}}
	for _, r := range list.Items {
		sum, h := ruleSummary(r.Rules)
		t.Rows = append(t.Rows, Row{Namespace: r.Namespace, Name: r.Name, Health: h, Cells: []string{r.Name, sum, Age(r.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listClusterRoles(ctx context.Context, c *Client, _ string) (Table, error) {
	list, err := c.Clientset.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "RULES", "AGE"}}
	for _, r := range list.Items {
		sum, h := ruleSummary(r.Rules)
		if r.AggregationRule != nil {
			sum = "(aggregated) " + sum
		}
		t.Rows = append(t.Rows, Row{Name: r.Name, Health: h, Cells: []string{r.Name, sum, Age(r.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func subjects(ss []rbacv1.Subject) string {
	var out []string
	for _, s := range ss {
		n := s.Name
		if s.Namespace != "" {
			n = s.Namespace + "/" + n
		}
		out = append(out, s.Kind+":"+n)
	}
	return strings.Join(out, ", ")
}

func listRoleBindings(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.RbacV1().RoleBindings(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "ROLE", "SUBJECTS", "AGE"}}
	for _, b := range list.Items {
		h := HealthNeutral
		if b.RoleRef.Name == "cluster-admin" || b.RoleRef.Name == "admin" {
			h = HealthWarn
		}
		t.Rows = append(t.Rows, Row{Namespace: b.Namespace, Name: b.Name, Health: h, Cells: []string{b.Name,
			b.RoleRef.Kind + "/" + b.RoleRef.Name, subjects(b.Subjects), Age(b.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listClusterRoleBindings(ctx context.Context, c *Client, _ string) (Table, error) {
	list, err := c.Clientset.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "ROLE", "SUBJECTS", "AGE"}}
	for _, b := range list.Items {
		h := HealthNeutral
		if b.RoleRef.Name == "cluster-admin" {
			h = HealthWarn
		}
		t.Rows = append(t.Rows, Row{Name: b.Name, Health: h, Cells: []string{b.Name, b.RoleRef.Kind + "/" + b.RoleRef.Name,
			subjects(b.Subjects), Age(b.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

// ---------- custom resources ----------

// PrinterColumn is a CRD additionalPrinterColumn.
type PrinterColumn struct{ Name, JSONPath string }

func listCRDs(ctx context.Context, c *Client, _ string) (Table, error) {
	if c.Dynamic == nil {
		return Table{}, fmt.Errorf("no API connection")
	}
	list, err := c.Dynamic.Resource(gvr("apiextensions.k8s.io", "v1", "customresourcedefinitions")).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "KIND", "GROUP", "SCOPE", "VERSIONS", "AGE"}}
	for _, it := range list.Items {
		kind, _, _ := unstructured.NestedString(it.Object, "spec", "names", "kind")
		group, _, _ := unstructured.NestedString(it.Object, "spec", "group")
		scope, _, _ := unstructured.NestedString(it.Object, "spec", "scope")
		vs, _, _ := unstructured.NestedSlice(it.Object, "spec", "versions")
		var versions []string
		for _, v := range vs {
			if m, ok := v.(map[string]any); ok {
				versions = append(versions, fmt.Sprint(m["name"]))
			}
		}
		t.Rows = append(t.Rows, Row{Name: it.GetName(), Health: crdHealth(&it), Cells: []string{it.GetName(), kind, group, scope,
			strings.Join(versions, ","), Age(it.GetCreationTimestamp())}})
	}
	sortRows(t.Rows)
	return t, nil
}

func crdHealth(u *unstructured.Unstructured) Health {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Established" && m["status"] != "True" {
			return HealthErr
		}
	}
	return HealthNeutral
}

// CRDResource builds a browsable Resource from a CRD object, including its printer columns.
func CRDResource(crd *unstructured.Unstructured) (Resource, bool) {
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "plural")
	kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	scope, _, _ := unstructured.NestedString(crd.Object, "spec", "scope")
	shorts, _, _ := unstructured.NestedStringSlice(crd.Object, "spec", "names", "shortNames")
	singular, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "singular")
	vs, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	version := ""
	var cols []PrinterColumn
	for _, v := range vs {
		m, _ := v.(map[string]any)
		served, _ := m["served"].(bool)
		storage, _ := m["storage"].(bool)
		if !served {
			continue
		}
		if version == "" || storage {
			version = fmt.Sprint(m["name"])
			cols = nil
			apc, _ := m["additionalPrinterColumns"].([]any)
			for _, pc := range apc {
				pm, _ := pc.(map[string]any)
				name, _ := pm["name"].(string)
				jp, _ := pm["jsonPath"].(string)
				if name != "" && jp != "" && !strings.EqualFold(name, "age") {
					cols = append(cols, PrinterColumn{Name: strings.ToUpper(name), JSONPath: jp})
				}
			}
		}
	}
	if plural == "" || version == "" {
		return Resource{}, false
	}
	aliases := append([]string{strings.ToLower(kind), singular, plural}, shorts...)
	return GenericResource(gvr(group, version, plural), kind, scope == "Namespaced", "Custom resources", aliases, cols), true
}
