package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

const revisionAnnotation = "deployment.kubernetes.io/revision"

// Revision is one ReplicaSet in a Deployment's history.
type Revision struct {
	Number      int64
	ReplicaSet  string
	Images      []string
	Desired     int32
	Ready       int32
	Age         string
	ChangeCause string
	Current     bool
	Template    corev1.PodTemplateSpec
}

// RolloutInfo is the history and live status of a Deployment.
type RolloutInfo struct {
	Deployment *appsv1.Deployment
	Revisions  []Revision // newest first
	Status     string
	Done       bool
}

// Rollout loads the revisions (owned ReplicaSets) and the rollout status of a Deployment.
func (c *Client) Rollout(ctx context.Context, ns, name string) (*RolloutInfo, error) {
	d, err := c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, err
	}
	rsl, err := c.Clientset.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, err
	}
	info := &RolloutInfo{Deployment: d}
	current := d.Annotations[revisionAnnotation]
	for i := range rsl.Items {
		rs := &rsl.Items[i]
		if !ownedBy(rs.OwnerReferences, d.UID) {
			continue
		}
		n, _ := strconv.ParseInt(rs.Annotations[revisionAnnotation], 10, 64)
		r := Revision{Number: n, ReplicaSet: rs.Name, Desired: deref(rs.Spec.Replicas, 0), Ready: rs.Status.ReadyReplicas,
			Age: Age(rs.CreationTimestamp), ChangeCause: rs.Annotations["kubernetes.io/change-cause"],
			Current: rs.Annotations[revisionAnnotation] == current, Template: rs.Spec.Template}
		for _, ctr := range rs.Spec.Template.Spec.Containers {
			r.Images = append(r.Images, ctr.Image)
		}
		info.Revisions = append(info.Revisions, r)
	}
	sort.Slice(info.Revisions, func(i, j int) bool { return info.Revisions[i].Number > info.Revisions[j].Number })
	info.Status, info.Done = rolloutStatus(d)
	return info, nil
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.Kind == "Deployment" && r.UID == uid {
			return true
		}
	}
	return false
}

// rolloutStatus mirrors `kubectl rollout status`.
func rolloutStatus(d *appsv1.Deployment) (string, bool) {
	want := deref(d.Spec.Replicas, 1)
	s := d.Status
	if d.Generation > s.ObservedGeneration {
		return "waiting for the controller to observe the new spec", false
	}
	for _, c := range s.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return "✗ rollout stuck: progress deadline exceeded", false
		}
	}
	switch {
	case d.Spec.Paused:
		return "paused", false
	case s.UpdatedReplicas < want:
		return fmt.Sprintf("rolling out: %d of %d new replicas updated", s.UpdatedReplicas, want), false
	case s.Replicas > s.UpdatedReplicas:
		return fmt.Sprintf("rolling out: %d old replicas pending termination", s.Replicas-s.UpdatedReplicas), false
	case s.AvailableReplicas < s.UpdatedReplicas:
		return fmt.Sprintf("rolling out: %d of %d updated replicas available", s.AvailableReplicas, s.UpdatedReplicas), false
	}
	return fmt.Sprintf("✓ rolled out: %d/%d available", s.AvailableReplicas, want), true
}

// TemplateYAML renders a pod template for diffing (the ReplicaSet hash label is dropped).
func TemplateYAML(t corev1.PodTemplateSpec) string {
	t = *t.DeepCopy()
	delete(t.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	t.CreationTimestamp = metav1.Time{}
	b, _ := yaml.Marshal(t)
	return string(b)
}

// RollbackTo restores the pod template of revision rev, like `kubectl rollout undo --to-revision`.
func (c *Client) RollbackTo(ctx context.Context, ns, name string, rev int64) error {
	info, err := c.Rollout(ctx, ns, name)
	if err != nil {
		return err
	}
	var target *Revision
	for i := range info.Revisions {
		if info.Revisions[i].Number == rev {
			target = &info.Revisions[i]
		}
	}
	if target == nil {
		return fmt.Errorf("revision %d not found", rev)
	}
	if target.Current {
		return fmt.Errorf("revision %d is already the current one", rev)
	}
	tmpl := *target.Template.DeepCopy()
	delete(tmpl.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	d := info.Deployment.DeepCopy()
	if !labels.SelectorFromSet(d.Spec.Selector.MatchLabels).Matches(labels.Set(tmpl.Labels)) {
		return fmt.Errorf("revision %d's labels no longer match the deployment selector", rev)
	}
	d.Spec.Template = tmpl
	_, err = c.Clientset.AppsV1().Deployments(ns).Update(ctx, d, metav1.UpdateOptions{})
	return err
}

// ShortImages renders image list compactly (registry host trimmed).
func ShortImages(imgs []string) string {
	out := make([]string, len(imgs))
	for i, im := range imgs {
		if j := strings.Index(im, "/"); j > 0 && strings.ContainsAny(im[:j], ".:") {
			im = im[j+1:]
		}
		out[i] = im
	}
	return strings.Join(out, ", ")
}
