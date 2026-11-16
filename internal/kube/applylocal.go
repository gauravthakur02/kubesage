package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// RenderLocal turns a path into YAML: a file, a kustomization folder (kubectl kustomize),
// a Helm chart folder (helm template) or a folder of .yaml files.
func RenderLocal(ctx context.Context, path string) (string, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", "", err
	}
	if !st.IsDir() {
		b, err := os.ReadFile(path)
		return string(b), "file", err
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(path, n)); return err == nil }
	run := func(name string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
				return "", fmt.Errorf("%s: %s", name, strings.TrimSpace(string(ee.Stderr)))
			}
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return string(out), nil
	}
	switch {
	case exists("kustomization.yaml") || exists("kustomization.yml") || exists("Kustomization"):
		y, err := run("kubectl", "kustomize", path)
		return y, "kustomize", err
	case exists("Chart.yaml"):
		y, err := run("helm", "template", filepath.Base(filepath.Clean(path)), path)
		return y, "helm template", err
	}
	var files []string
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", "", err
	}
	for _, e := range entries {
		if ext := strings.ToLower(filepath.Ext(e.Name())); !e.IsDir() && (ext == ".yaml" || ext == ".yml" || ext == ".json") {
			files = append(files, filepath.Join(path, e.Name()))
		}
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return "", "", err
		}
		b.WriteString("---\n")
		b.Write(data)
		b.WriteString("\n")
	}
	if len(files) == 0 {
		return "", "", fmt.Errorf("no .yaml files, kustomization or Chart.yaml in %s", path)
	}
	return b.String(), fmt.Sprintf("%d files", len(files)), nil
}

// ParseManifests splits a YAML/JSON stream into objects (List kinds are expanded).
func ParseManifests(y string) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader([]byte(y)), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: m}
		if u.IsList() {
			l, err := u.ToList()
			if err != nil {
				return nil, err
			}
			for i := range l.Items {
				out = append(out, &l.Items[i])
			}
			continue
		}
		if u.GetKind() == "" || u.GetName() == "" {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

// ApplyItem is one object of a local apply plan.
type ApplyItem struct {
	Kind, Namespace, Name string
	State                 string // new, changed, unchanged, error
	Live, Result          string // comparable YAML before / after (server-side dry-run)
	Err                   error
	obj                   *unstructured.Unstructured
	ri                    dynamic.ResourceInterface
}

// PlanApply server-side dry-runs every object (field manager "kubesage") and compares with what runs.
func (c *Client) PlanApply(ctx context.Context, objs []*unstructured.Unstructured, defaultNS string) ([]ApplyItem, error) {
	if c.Dynamic == nil {
		return nil, fmt.Errorf("dynamic client unavailable")
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(c.Clientset.Discovery()))
	var items []ApplyItem
	for _, o := range objs {
		gvk := o.GroupVersionKind()
		it := ApplyItem{Kind: o.GetKind(), Name: o.GetName(), obj: o}
		m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			it.State, it.Err = "error", fmt.Errorf("unknown kind %s: %w", gvk, err)
			items = append(items, it)
			continue
		}
		ri := dynamic.ResourceInterface(c.Dynamic.Resource(m.Resource))
		if m.Scope.Name() == "namespace" {
			if o.GetNamespace() == "" {
				o.SetNamespace(defaultNS)
			}
			it.Namespace = o.GetNamespace()
			ri = c.Dynamic.Resource(m.Resource).Namespace(o.GetNamespace())
		}
		it.ri = ri
		live, err := ri.Get(ctx, o.GetName(), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			it.State = "new"
		case err != nil:
			it.State, it.Err = "error", err
			items = append(items, it)
			continue
		default:
			it.Live = Comparable(live)
		}
		res, err := ri.Apply(ctx, o.GetName(), o, metav1.ApplyOptions{FieldManager: "kubesage", DryRun: []string{metav1.DryRunAll}})
		if err != nil {
			it.State, it.Err = "error", err
			items = append(items, it)
			continue
		}
		it.Result = Comparable(res)
		if it.State == "" {
			it.State = "unchanged"
			if stripManaged(it.Live) != stripManaged(it.Result) {
				it.State = "changed"
			}
		}
		items = append(items, it)
	}
	return items, nil
}

// stripManaged ignores fields that a dry-run apply always touches.
func stripManaged(y string) string {
	var keep []string
	for _, l := range strings.Split(y, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "generation:") || strings.HasPrefix(t, "resourceVersion:") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

// ExecuteApply applies the new and changed objects of a plan (server-side apply, no forced conflicts).
func (c *Client) ExecuteApply(ctx context.Context, items []ApplyItem) (applied int, errs []string) {
	for _, it := range items {
		if it.State != "new" && it.State != "changed" {
			continue
		}
		if _, err := it.ri.Apply(ctx, it.obj.GetName(), it.obj, metav1.ApplyOptions{FieldManager: "kubesage"}); err != nil {
			errs = append(errs, fmt.Sprintf("%s/%s: %v", it.Kind, it.Name, err))
			continue
		}
		applied++
	}
	return applied, errs
}
