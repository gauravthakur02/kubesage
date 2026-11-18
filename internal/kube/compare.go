package kube

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// Comparable strips everything that always differs between clusters (status, ids, timestamps,
// generated annotations) and returns stable YAML. Secret values are replaced by a hash.
func Comparable(obj *unstructured.Unstructured) string {
	m := Clean(obj, false, false)
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		for _, k := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "selfLink", "ownerReferences"} {
			delete(md, k)
		}
		if ann, ok := md["annotations"].(map[string]any); ok {
			for _, k := range []string{"deployment.kubernetes.io/revision", "kubectl.kubernetes.io/restartedAt"} {
				delete(ann, k)
			}
			if len(ann) == 0 {
				delete(md, "annotations")
			}
		}
	}
	if obj.GetKind() == "Secret" {
		if data, ok := obj.Object["data"].(map[string]any); ok {
			hashed := map[string]any{}
			for k, v := range data {
				hashed[k] = fmt.Sprintf("<sha256 %x>", sha256.Sum256([]byte(fmt.Sprint(v))))[:22] + ">"
			}
			m["data"] = hashed
		}
	}
	hashEnvValues(m) // snapshots and diffs never hold plaintext passwords from env vars
	if spec, ok := m["spec"].(map[string]any); ok {
		if tpl, ok := spec["template"].(map[string]any); ok {
			if md, ok := tpl["metadata"].(map[string]any); ok {
				if ann, ok := md["annotations"].(map[string]any); ok {
					delete(ann, "kubectl.kubernetes.io/restartedAt")
				}
			}
		}
		delete(spec, "clusterIP")
		delete(spec, "clusterIPs")
	}
	b, _ := yaml.Marshal(m)
	return string(b)
}

// CompareEntry is one object name in a namespace comparison.
type CompareEntry struct {
	Name  string
	State string // same, differs, only-here, only-there
}

// CompareNamespace lists r in ns on both clusters and compares every object by name.
func CompareNamespace(ctx context.Context, here, there *Client, r Resource, ns string) ([]CompareEntry, error) {
	list := func(c *Client) (map[string]string, error) {
		ri := c.Dynamic.Resource(r.GVR)
		var l *unstructured.UnstructuredList
		var err error
		if r.Namespaced {
			l, err = ri.Namespace(ns).List(ctx, metav1.ListOptions{})
		} else {
			l, err = ri.List(ctx, metav1.ListOptions{})
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Context, err)
		}
		out := map[string]string{}
		for i := range l.Items {
			if len(l.Items[i].GetOwnerReferences()) > 0 {
				continue // generated (ReplicaSets, pods…): compare their owners instead
			}
			out[l.Items[i].GetName()] = Comparable(&l.Items[i])
		}
		return out, nil
	}
	a, err := list(here)
	if err != nil {
		return nil, err
	}
	b, err := list(there)
	if err != nil {
		return nil, err
	}
	var out []CompareEntry
	for n, ya := range a {
		yb, ok := b[n]
		switch {
		case !ok:
			out = append(out, CompareEntry{n, "only-here"})
		case ya == yb:
			out = append(out, CompareEntry{n, "same"})
		default:
			out = append(out, CompareEntry{n, "differs"})
		}
	}
	for n := range b {
		if _, ok := a[n]; !ok {
			out = append(out, CompareEntry{n, "only-there"})
		}
	}
	order := map[string]int{"differs": 0, "only-here": 1, "only-there": 2, "same": 3}
	sort.Slice(out, func(i, j int) bool {
		if order[out[i].State] != order[out[j].State] {
			return order[out[i].State] < order[out[j].State]
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
