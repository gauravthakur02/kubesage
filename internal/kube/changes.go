package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Change is something that changed in a namespace: a rollout, a config edit, a scale, a Helm upgrade, a GitOps sync.
type Change struct {
	Time            time.Time
	Source          string // rollout, config, scale, helm, gitops, workload
	Namespace, Name string // object
	Kind            string
	What            string
}

// Changes lists what changed in the client's namespace (or all) since the given time, newest first.
// It answers "what changed just before this broke?".
func (c *Client) Changes(ctx context.Context, since time.Time) ([]Change, error) {
	ns := c.Namespace
	var out []Change
	add := func(t time.Time, src, kind, ns, name, what string) {
		if !t.Before(since) {
			out = append(out, Change{Time: t, Source: src, Kind: kind, Namespace: ns, Name: name, What: what})
		}
	}
	rss, err := listAcross(ctx, c, ns, func(n string) ([]appsv1.ReplicaSet, error) {
		l, err := c.Clientset.AppsV1().ReplicaSets(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if err != nil {
		return nil, err
	}
	for _, rs := range rss {
		owner := rs.Name
		if ref := metav1.GetControllerOf(&rs); ref != nil {
			owner = ref.Name
		}
		var imgs []string
		for _, ct := range rs.Spec.Template.Spec.Containers {
			imgs = append(imgs, ct.Image)
		}
		add(rs.CreationTimestamp.Time, "rollout", "Deployment", rs.Namespace, owner,
			fmt.Sprintf("revision %s rolled out · %s", rs.Annotations["deployment.kubernetes.io/revision"], strings.Join(imgs, ", ")))
	}
	if revs, err := listAcross(ctx, c, ns, func(n string) ([]appsv1.ControllerRevision, error) {
		l, err := c.Clientset.AppsV1().ControllerRevisions(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, r := range revs {
			if ref := metav1.GetControllerOf(&r); ref != nil {
				add(r.CreationTimestamp.Time, "rollout", ref.Kind, r.Namespace, ref.Name, fmt.Sprintf("revision %d rolled out", r.Revision))
			}
		}
	}
	if cms, err := listAcross(ctx, c, ns, func(n string) ([]corev1.ConfigMap, error) {
		l, err := c.Clientset.CoreV1().ConfigMaps(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, cm := range cms {
			if cm.Name == "kube-root-ca.crt" {
				continue
			}
			t := lastChange(cm.ObjectMeta)
			what := "changed"
			if t.Equal(cm.CreationTimestamp.Time) {
				what = "created"
			}
			add(t, "config", "ConfigMap", cm.Namespace, cm.Name, what)
		}
	}
	if secs, err := listAcross(ctx, c, ns, func(n string) ([]corev1.Secret, error) {
		l, err := c.Clientset.CoreV1().Secrets(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, s := range secs {
			if s.Type == corev1.SecretTypeServiceAccountToken {
				continue
			}
			if s.Type == "helm.sh/release.v1" { // one Secret per Helm revision
				add(s.CreationTimestamp.Time, "helm", "HelmRelease", s.Namespace, s.Labels["name"],
					fmt.Sprintf("revision %s %s", s.Labels["version"], s.Labels["status"]))
				continue
			}
			t := lastChange(s.ObjectMeta)
			what := "changed (values not shown)"
			if t.Equal(s.CreationTimestamp.Time) {
				what = "created"
			}
			add(t, "config", "Secret", s.Namespace, s.Name, what)
		}
	}
	if evs, err := listAcross(ctx, c, ns, func(n string) ([]corev1.Event, error) {
		l, err := c.Clientset.CoreV1().Events(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, e := range evs {
			if e.Reason != "ScalingReplicaSet" && e.Reason != "SuccessfulRescale" {
				continue
			}
			t := e.LastTimestamp.Time
			if t.IsZero() {
				t = e.EventTime.Time
			}
			add(t, "scale", e.InvolvedObject.Kind, e.Namespace, e.InvolvedObject.Name, firstLine(e.Message))
		}
	}
	if c.Dynamic != nil { // Argo CD syncs and Flux reconciliations
		for _, k := range gitopsKinds {
			for _, gvr := range k.gvrs {
				items, err := listAcross(ctx, c, ns, func(n string) ([]unstructured.Unstructured, error) {
					l, err := c.Dynamic.Resource(gvr).Namespace(n).List(ctx, metav1.ListOptions{})
					if err != nil {
						return nil, err
					}
					return l.Items, nil
				})
				if err != nil {
					continue
				}
				for _, u := range items {
					if fin, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "finishedAt"); fin != "" {
						t, _ := time.Parse(time.RFC3339, fin)
						rev, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "syncResult", "revision")
						phase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
						add(t, "gitops", "Application", u.GetNamespace(), u.GetName(), strings.TrimSpace("Argo CD sync "+phase+" "+shortRev(rev)))
					}
					if rev, _, _ := unstructured.NestedString(u.Object, "status", "lastAppliedRevision"); rev != "" {
						conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
						for _, x := range conds {
							if m, ok := x.(map[string]any); ok && m["type"] == "Ready" {
								t, _ := time.Parse(time.RFC3339, fmt.Sprint(m["lastTransitionTime"]))
								add(t, "gitops", u.GetKind(), u.GetNamespace(), u.GetName(), "Flux applied "+shortRev(rev))
							}
						}
					}
				}
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func shortRev(r string) string {
	if i := strings.LastIndex(r, ":"); i >= 0 { // flux: main@sha1:abc…
		r = r[i+1:]
	}
	if len(r) > 12 {
		r = r[:12]
	}
	return r
}
