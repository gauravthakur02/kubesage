package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Labels on namespaces created by CloneNamespace.
const (
	CloneOfLabel     = "kubeweave.io/clone-of"
	CloneExpiresAnno = "kubeweave.io/expires"
)

var cloneKinds = []struct {
	name string
	gvr  schema.GroupVersionResource
}{
	{"serviceaccounts", schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}},
	{"configmaps", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}},
	{"secrets", schema.GroupVersionResource{Version: "v1", Resource: "secrets"}},
	{"services", schema.GroupVersionResource{Version: "v1", Resource: "services"}},
	{"deployments", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}},
	{"statefulsets", schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}},
	{"cronjobs", schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}},
}

// cleanForCopy strips server-set fields so an object can be created in another namespace.
func cleanForCopy(u *unstructured.Unstructured, ns string) bool {
	switch u.GetKind() {
	case "ServiceAccount":
		if u.GetName() == "default" {
			return false
		}
	case "ConfigMap":
		if u.GetName() == "kube-root-ca.crt" {
			return false
		}
	case "Secret":
		t, _, _ := unstructured.NestedString(u.Object, "type")
		if t == string(corev1.SecretTypeServiceAccountToken) || strings.HasPrefix(t, "helm.sh/") {
			return false
		}
	case "Service":
		unstructured.RemoveNestedField(u.Object, "spec", "clusterIP")
		unstructured.RemoveNestedField(u.Object, "spec", "clusterIPs")
		unstructured.RemoveNestedField(u.Object, "spec", "healthCheckNodePort")
		if ports, ok, _ := unstructured.NestedSlice(u.Object, "spec", "ports"); ok {
			for _, p := range ports {
				if m, ok := p.(map[string]any); ok {
					delete(m, "nodePort")
				}
			}
			_ = unstructured.SetNestedSlice(u.Object, ports, "spec", "ports")
		}
		t, _, _ := unstructured.NestedString(u.Object, "spec", "type")
		if t == "LoadBalancer" || t == "NodePort" { // no public IPs or node ports for a throwaway copy
			_ = unstructured.SetNestedField(u.Object, "ClusterIP", "spec", "type")
			unstructured.RemoveNestedField(u.Object, "spec", "externalTrafficPolicy")
			unstructured.RemoveNestedField(u.Object, "spec", "loadBalancerIP")
		}
	}
	if len(u.GetOwnerReferences()) > 0 {
		return false // created by a controller that is copied too
	}
	u.SetNamespace(ns)
	u.SetResourceVersion("")
	u.SetUID("")
	u.SetCreationTimestamp(metav1.Time{})
	u.SetManagedFields(nil)
	u.SetGeneration(0)
	an := u.GetAnnotations()
	delete(an, "kubectl.kubernetes.io/last-applied-configuration")
	delete(an, "deployment.kubernetes.io/revision")
	if len(an) == 0 {
		an = nil
	}
	u.SetAnnotations(an)
	delete(u.Object, "status")
	return true
}

// CloneResult reports what CloneNamespace copied.
type CloneResult struct {
	Copied  map[string]int
	Skipped []string
}

// CloneNamespace copies workloads, services, config and secrets of src into a new namespace dst, labelled
// with its origin and an expiry time. Ingresses, PVCs and Helm release records are not copied.
func (c *Client) CloneNamespace(ctx context.Context, src, dst string, ttl time.Duration) (CloneResult, error) {
	res := CloneResult{Copied: map[string]int{}}
	if src == "" || dst == "" || src == dst {
		return res, fmt.Errorf("give a source and a different target namespace")
	}
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: dst, Labels: map[string]string{CloneOfLabel: src, "app.kubernetes.io/managed-by": "kubeweave"},
		Annotations: map[string]string{CloneExpiresAnno: time.Now().Add(ttl).UTC().Format(time.RFC3339)}}}
	if _, err := c.Clientset.CoreV1().Namespaces().Create(ctx, nsObj, metav1.CreateOptions{FieldManager: "kubeweave"}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return res, fmt.Errorf("namespace %s already exists; pick a new name", dst)
		}
		return res, err
	}
	for _, k := range cloneKinds {
		l, err := c.Dynamic.Resource(k.gvr).Namespace(src).List(ctx, metav1.ListOptions{})
		if err != nil {
			res.Skipped = append(res.Skipped, k.name+": "+firstLine(err.Error()))
			continue
		}
		for i := range l.Items {
			u := l.Items[i].DeepCopy()
			if !cleanForCopy(u, dst) {
				continue
			}
			if _, err := c.Dynamic.Resource(k.gvr).Namespace(dst).Create(ctx, u, metav1.CreateOptions{FieldManager: "kubeweave"}); err != nil {
				res.Skipped = append(res.Skipped, fmt.Sprintf("%s/%s: %s", k.name, u.GetName(), firstLine(err.Error())))
				continue
			}
			res.Copied[k.name]++
		}
	}
	return res, nil
}

// Clone is a namespace created by CloneNamespace.
type Clone struct {
	Name, Of string
	Created  time.Time
	Expires  time.Time
}

// Clones lists namespaces created as clones.
func (c *Client) Clones(ctx context.Context) ([]Clone, error) {
	l, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: CloneOfLabel})
	if err != nil {
		return nil, err
	}
	var out []Clone
	for _, n := range l.Items {
		exp, _ := time.Parse(time.RFC3339, n.Annotations[CloneExpiresAnno])
		out = append(out, Clone{Name: n.Name, Of: n.Labels[CloneOfLabel], Created: n.CreationTimestamp.Time, Expires: exp})
	}
	return out, nil
}

// DeleteClone deletes a namespace, but only one that CloneNamespace created.
func (c *Client) DeleteClone(ctx context.Context, name string) error {
	n, err := c.Clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if n.Labels[CloneOfLabel] == "" {
		return fmt.Errorf("%s was not created by kubeweave :clone; refusing to delete it", name)
	}
	return c.Clientset.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
}
