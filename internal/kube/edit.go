package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// warnCollector captures API server warnings (e.g. admission policy warnings) instead of
// letting client-go print them to stderr, which would also corrupt the terminal UI.
type warnCollector struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnCollector) HandleWarningHeader(code int, agent string, text string) {
	if code != 299 || text == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, text)
}

func (w *warnCollector) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.msgs
	w.msgs = nil
	return out
}

// EditableYAML returns the object as YAML for editing: managedFields and status are removed,
// resourceVersion is kept so a concurrent change is detected when applying.
func (c *Client) EditableYAML(ctx context.Context, r Resource, ns, name string) (string, error) {
	obj, err := c.Get(ctx, r, ns, name)
	if err != nil {
		return "", err
	}
	m := obj.DeepCopy().Object
	delete(m, "status")
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
	}
	b, err := yaml.Marshal(m)
	return string(b), err
}

// ApplyResult is the outcome of a (dry-run) update.
type ApplyResult struct {
	Warnings []string
}

// UpdateFromYAML replaces the object with edited YAML. With dryRun the API server validates
// it (including admission webhooks and policies) without persisting anything.
func (c *Client) UpdateFromYAML(ctx context.Context, r Resource, ns, name, edited string, dryRun bool) (ApplyResult, error) {
	var res ApplyResult
	if c.Dynamic == nil {
		return res, fmt.Errorf("no API connection")
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(edited), &obj.Object); err != nil {
		return res, fmt.Errorf("invalid YAML: %w", err)
	}
	if obj.GetName() != name {
		return res, fmt.Errorf("metadata.name changed from %q to %q; renaming is not supported", name, obj.GetName())
	}
	if r.Namespaced && obj.GetNamespace() != "" && obj.GetNamespace() != ns {
		return res, fmt.Errorf("metadata.namespace changed; moving objects is not supported")
	}
	if !strings.EqualFold(obj.GetKind(), r.Kind) && obj.GetKind() != "" {
		return res, fmt.Errorf("kind changed from %s to %s", r.Kind, obj.GetKind())
	}
	opts := metav1.UpdateOptions{FieldManager: "kubesage"}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if c.warns != nil {
		c.warns.take()
	}
	ri := c.Dynamic.Resource(r.GVR)
	var err error
	if r.Namespaced {
		_, err = ri.Namespace(ns).Update(ctx, obj, opts)
	} else {
		_, err = ri.Update(ctx, obj, opts)
	}
	if c.warns != nil {
		res.Warnings = c.warns.take()
	}
	return res, err
}
