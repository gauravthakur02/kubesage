package kube

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// Get fetches an object as unstructured.
func (c *Client) Get(ctx context.Context, r Resource, ns, name string) (*unstructured.Unstructured, error) {
	if c.Dynamic == nil {
		return nil, fmt.Errorf("no API connection for %s", c.Context)
	}
	ri := c.Dynamic.Resource(r.GVR)
	if r.Namespaced {
		return ri.Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}
	return ri.Get(ctx, name, metav1.GetOptions{})
}

var sensitiveName = regexp.MustCompile(`(?i)(pass|secret|token|key|credential|pwd|auth|cert|connection.?string)`)

// Clean strips noise (managedFields, last-applied) and optionally redacts secrets.
// For Secrets, reveal=true decodes base64 values; otherwise they are masked.
func Clean(obj *unstructured.Unstructured, redactEnv, reveal bool) map[string]any {
	m := obj.DeepCopy().Object
	if md, ok := m["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
			if len(ann) == 0 {
				delete(md, "annotations")
			}
		}
	}
	if obj.GetKind() == "Secret" {
		for _, field := range []string{"data", "stringData"} {
			data, ok := m[field].(map[string]any)
			if !ok {
				continue
			}
			for k, v := range data {
				s, _ := v.(string)
				switch {
				case reveal && field == "data":
					if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
						data[k] = string(dec)
					}
				case !reveal:
					data[k] = fmt.Sprintf("<redacted %d bytes>", len(s))
				}
			}
		}
	}
	if redactEnv {
		redactEnvValues(m)
	}
	return m
}

// redactEnvValues masks {name: SOME_PASSWORD, value: ...} pairs anywhere in the tree.
func redactEnvValues(v any) {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok {
			if _, has := t["value"]; has && sensitiveName.MatchString(name) {
				t["value"] = "<redacted>"
			}
		}
		for _, child := range t {
			redactEnvValues(child)
		}
	case []any:
		for _, child := range t {
			redactEnvValues(child)
		}
	}
}

// YAML returns the cleaned manifest of an object.
func (c *Client) YAML(ctx context.Context, r Resource, ns, name string, reveal bool) (string, error) {
	obj, err := c.Get(ctx, r, ns, name)
	if err != nil {
		return "", err
	}
	out, err := yaml.Marshal(Clean(obj, false, reveal))
	return string(out), err
}

// kubectlArgs prefixes args with the kubeconfig/context flags of this client.
func (c *Client) KubectlArgs(args ...string) []string {
	base := []string{"--context", c.Context}
	if c.Kubeconfig != "" {
		base = append(base, "--kubeconfig", c.Kubeconfig)
	}
	return append(base, args...)
}

// HasKubectl reports whether kubectl is on PATH.
func HasKubectl() bool {
	_, err := exec.LookPath("kubectl")
	return err == nil
}

// Describe uses `kubectl describe` when available, else YAML + events.
func (c *Client) Describe(ctx context.Context, r Resource, ns, name string) (string, error) {
	if HasKubectl() {
		args := []string{"describe", r.Name, name}
		if r.Namespaced {
			args = append(args, "-n", ns)
		}
		cmd := exec.CommandContext(ctx, "kubectl", c.KubectlArgs(args...)...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), nil
		}
	}
	y, err := c.YAML(ctx, r, ns, name, false)
	if err != nil {
		return "", err
	}
	ev, _ := c.EventsFor(ctx, r.Kind, ns, name)
	return y + "\n# Events\n" + ev, nil
}

// EventsFor renders recent events for one object, newest last.
func (c *Client) EventsFor(ctx context.Context, kind, ns, name string) (string, error) {
	sel := fields.Set{"involvedObject.name": name, "involvedObject.kind": kind}
	if ns != "" {
		sel["involvedObject.namespace"] = ns
	}
	list, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel.AsSelector().String()})
	if err != nil {
		return "", err
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(&items[i]).Before(eventTime(&items[j])) })
	if len(items) == 0 {
		return "<no events>\n", nil
	}
	var b strings.Builder
	for i := range items {
		e := &items[i]
		fmt.Fprintf(&b, "%s ago  %-7s %-22s x%d  %s\n", Age(metav1.NewTime(eventTime(e))), e.Type, e.Reason, max(e.Count, 1), e.Message)
	}
	return b.String(), nil
}

// Delete removes an object (background propagation).
func (c *Client) Delete(ctx context.Context, r Resource, ns, name string) error {
	policy := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}
	ri := c.Dynamic.Resource(r.GVR)
	if r.Namespaced {
		return ri.Namespace(ns).Delete(ctx, name, opts)
	}
	return ri.Delete(ctx, name, opts)
}

// Scale sets spec.replicas.
func (c *Client) Scale(ctx context.Context, r Resource, ns, name string, replicas int) error {
	if !r.CanScale() {
		return fmt.Errorf("%s cannot be scaled", r.Name)
	}
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	_, err := c.Dynamic.Resource(r.GVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// Restart performs `kubectl rollout restart` by bumping a template annotation.
func (c *Client) Restart(ctx context.Context, r Resource, ns, name string) error {
	if !r.CanRestart() {
		return fmt.Errorf("%s cannot be restarted", r.Name)
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339))
	_, err := c.Dynamic.Resource(r.GVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// PodsFor resolves the pods owned by a workload via its label selector.
func (c *Client) PodsFor(ctx context.Context, r Resource, ns, name string) ([]corev1.Pod, error) {
	if r.Name == "pods" {
		p, err := c.Clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return []corev1.Pod{*p}, nil
	}
	obj, err := c.Get(ctx, r, ns, name)
	if err != nil {
		return nil, err
	}
	ml, found, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	if !found || len(ml) == 0 {
		return nil, fmt.Errorf("%s/%s has no matchLabels selector", r.Name, name)
	}
	list, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(ml).String()})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// Containers lists container names of a pod (init containers last).
func Containers(p *corev1.Pod) []string {
	var out []string
	for _, c := range p.Spec.Containers {
		out = append(out, c.Name)
	}
	for _, c := range p.Spec.InitContainers {
		out = append(out, c.Name)
	}
	return out
}

// LogOptions controls a log request.
type LogOptions struct {
	Container  string
	Tail       int64
	Previous   bool
	Follow     bool
	Since      time.Duration // only lines newer than this (0 = use Tail)
	Timestamps bool          // prefix each line with its RFC3339 timestamp
}

// Logs opens a log stream for a pod container.
func (c *Client) Logs(ctx context.Context, ns, pod string, o LogOptions) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{Container: o.Container, Previous: o.Previous, Follow: o.Follow, Timestamps: o.Timestamps}
	if o.Since > 0 {
		secs := int64(o.Since.Seconds())
		opts.SinceSeconds = &secs
		limit := int64(20000) // keep memory bounded even for a busy pod
		opts.TailLines = &limit
	} else if o.Tail > 0 {
		opts.TailLines = &o.Tail
	}
	return c.Clientset.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
}

// LogsText returns logs as a string (non-follow).
func (c *Client) LogsText(ctx context.Context, ns, pod string, o LogOptions) (string, error) {
	o.Follow = false
	rc, err := c.Logs(ctx, ns, pod, o)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 512*1024))
	return string(b), err
}
