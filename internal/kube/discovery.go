package kube

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/jsonpath"
)

// GenericResource browses any API resource through the dynamic client.
// cols are extra printer columns (e.g. from a CRD's additionalPrinterColumns).
func GenericResource(g schema.GroupVersionResource, kind string, namespaced bool, group string, aliases []string, cols []PrinterColumn) Resource {
	name := g.Resource
	if g.Group != "" {
		name += "." + g.Group // what kubectl understands for describe/edit
	}
	return Resource{Name: name, Kind: kind, Short: kind, Aliases: aliases, Namespaced: namespaced, GVR: g, Group: group,
		list: func(ctx context.Context, c *Client, ns string) (Table, error) {
			return genericList(ctx, c, g, namespaced, ns, cols)
		}}
}

func genericList(ctx context.Context, c *Client, g schema.GroupVersionResource, namespaced bool, ns string, cols []PrinterColumn) (Table, error) {
	if c.Dynamic == nil {
		return Table{}, fmt.Errorf("no API connection")
	}
	ri := c.Dynamic.Resource(g)
	var list *unstructured.UnstructuredList
	var err error
	if namespaced {
		list, err = ri.Namespace(ns).List(ctx, metav1.ListOptions{})
	} else {
		list, err = ri.List(ctx, metav1.ListOptions{})
	}
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME"}}
	parsed := make([]*jsonpath.JSONPath, len(cols))
	for i, col := range cols {
		t.Columns = append(t.Columns, col.Name)
		jp := jsonpath.New(col.Name).AllowMissingKeys(true)
		if jp.Parse("{"+col.JSONPath+"}") == nil {
			parsed[i] = jp
		}
	}
	if len(cols) == 0 {
		t.Columns = append(t.Columns, "STATUS")
	}
	t.Columns = append(t.Columns, "AGE")
	for i := range list.Items {
		it := &list.Items[i]
		cells := []string{it.GetName()}
		for _, jp := range parsed {
			v := ""
			if jp != nil {
				var buf bytes.Buffer
				if jp.Execute(&buf, it.Object) == nil {
					v = buf.String()
				}
			}
			cells = append(cells, v)
		}
		status, h := objectStatus(it)
		if len(cols) == 0 {
			cells = append(cells, status)
		}
		cells = append(cells, Age(it.GetCreationTimestamp()))
		t.Rows = append(t.Rows, Row{Namespace: it.GetNamespace(), Name: it.GetName(), Health: h, Cells: cells})
	}
	sortRows(t.Rows)
	return t, nil
}

// objectStatus reads the common status conventions: conditions[type=Ready|Available], or status.phase.
func objectStatus(u *unstructured.Unstructured) (string, Health) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, want := range []string{"Ready", "Available", "Synced", "Healthy"} {
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] != want {
				continue
			}
			if m["status"] == "True" {
				return want, HealthOK
			}
			reason, _ := m["reason"].(string)
			return "Not" + want + " " + reason, HealthErr
		}
	}
	if phase, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok && phase != "" {
		switch strings.ToLower(phase) {
		case "failed", "error":
			return phase, HealthErr
		case "pending", "progressing":
			return phase, HealthWarn
		}
		return phase, HealthOK
	}
	return "", HealthNeutral
}

// Discover lists custom resources (from CRDs, with printer columns) and every other listable
// API resource the cluster serves that kubeweave has no built-in view for.
func (c *Client) Discover(ctx context.Context) ([]Resource, error) {
	known := map[string]bool{}
	for _, r := range registry {
		known[r.GVR.Group+"/"+r.GVR.Resource] = true
	}
	var out []Resource
	if c.Dynamic != nil {
		if crds, err := c.Dynamic.Resource(gvr("apiextensions.k8s.io", "v1", "customresourcedefinitions")).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range crds.Items {
				if r, ok := CRDResource(&crds.Items[i]); ok && !known[r.GVR.Group+"/"+r.GVR.Resource] {
					known[r.GVR.Group+"/"+r.GVR.Resource] = true
					out = append(out, r)
				}
			}
		}
	}
	lists, err := c.Clientset.Discovery().ServerPreferredResources()
	if err != nil && len(lists) == 0 && len(out) == 0 {
		return nil, err
	}
	for _, l := range lists {
		gv, perr := schema.ParseGroupVersion(l.GroupVersion)
		if perr != nil {
			continue
		}
		for _, ar := range l.APIResources {
			if strings.Contains(ar.Name, "/") || !hasVerb(ar.Verbs, "list") || known[gv.Group+"/"+ar.Name] {
				continue
			}
			known[gv.Group+"/"+ar.Name] = true
			aliases := append([]string{strings.ToLower(ar.Kind), ar.SingularName, ar.Name}, ar.ShortNames...)
			out = append(out, GenericResource(gv.WithResource(ar.Name), ar.Kind, ar.Namespaced, "Other APIs", aliases, nil))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func hasVerb(verbs []string, v string) bool {
	for _, x := range verbs {
		if x == v {
			return true
		}
	}
	return false
}

// FindIn looks a name/alias up in a list of (discovered) resources.
func FindIn(rs []Resource, name string) (Resource, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, r := range rs {
		if r.Name == name || strings.EqualFold(r.Kind, name) || r.GVR.Resource == name {
			return r, true
		}
		for _, a := range r.Aliases {
			if a == name {
				return r, true
			}
		}
	}
	return Resource{}, false
}
