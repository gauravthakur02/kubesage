package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Snapshot is the comparable state of one namespace at a moment (secret values are stored as hashes only).
type Snapshot struct {
	Name      string            `json:"name"`
	Context   string            `json:"context"`
	Namespace string            `json:"namespace"`
	Taken     time.Time         `json:"taken"`
	Objects   map[string]string `json:"objects"` // "kind/name" → comparable YAML
}

// SnapshotKinds are the resources a snapshot keeps.
var SnapshotKinds = []string{"deployments", "statefulsets", "daemonsets", "cronjobs", "services", "ingresses", "configmaps", "secrets",
	"horizontalpodautoscalers", "poddisruptionbudgets", "networkpolicies", "serviceaccounts", "roles", "rolebindings"}

// TakeSnapshot records a namespace.
func (c *Client) TakeSnapshot(ctx context.Context, ns, name string) (*Snapshot, error) {
	if ns == "" {
		return nil, fmt.Errorf("pick a namespace")
	}
	s := &Snapshot{Name: name, Context: c.Context, Namespace: ns, Taken: time.Now().UTC(), Objects: map[string]string{}}
	for _, k := range SnapshotKinds {
		r, ok := Lookup(k)
		if !ok || r.GVR.Resource == "" {
			continue
		}
		l, err := c.Dynamic.Resource(r.GVR).Namespace(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue // not allowed or not served
		}
		for i := range l.Items {
			u := &l.Items[i]
			if (k == "configmaps" && u.GetName() == "kube-root-ca.crt") || (k == "secrets" && strings.HasPrefix(typeOf(u), "helm.sh/")) ||
				(k == "secrets" && typeOf(u) == "kubernetes.io/service-account-token") || (k == "serviceaccounts" && u.GetName() == "default") {
				continue
			}
			s.Objects[r.Kind+"/"+u.GetName()] = Comparable(u)
		}
	}
	return s, nil
}

func typeOf(u *unstructured.Unstructured) string {
	t, _, _ := unstructured.NestedString(u.Object, "type")
	return t
}

// SnapshotDir is where snapshots of a context are kept.
func SnapshotDir(base, kctx string) string {
	return filepath.Join(base, unsafeFile.ReplaceAllString(kctx, "_"))
}

// Save writes the snapshot as <dir>/<namespace>/<name>.json.
func (s *Snapshot) Save(base string) (string, error) {
	dir := filepath.Join(SnapshotDir(base, s.Context), unsafeFile.ReplaceAllString(s.Namespace, "_"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if s.Name == "" {
		s.Name = s.Taken.Format("2006-01-02T15-04-05")
	}
	p := filepath.Join(dir, unsafeFile.ReplaceAllString(s.Name, "_")+".json")
	b, _ := json.MarshalIndent(s, "", " ")
	return p, os.WriteFile(p, b, 0o600)
}

// LoadSnapshot reads a saved snapshot.
func LoadSnapshot(base, kctx, ns, name string) (*Snapshot, error) {
	p := filepath.Join(SnapshotDir(base, kctx), unsafeFile.ReplaceAllString(ns, "_"), unsafeFile.ReplaceAllString(name, "_")+".json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("snapshot %q of %s: %w", name, ns, err)
	}
	var s Snapshot
	return &s, json.Unmarshal(b, &s)
}

// ListSnapshots lists saved snapshot names of a namespace, newest first.
func ListSnapshots(base, kctx, ns string) ([]Snapshot, error) {
	dir := filepath.Join(SnapshotDir(base, kctx), unsafeFile.ReplaceAllString(ns, "_"))
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, e := range ents {
		if s, err := LoadSnapshot(base, kctx, ns, strings.TrimSuffix(e.Name(), ".json")); err == nil {
			s.Objects = map[string]string{"": fmt.Sprint(len(s.Objects))} // only the count is needed for listings
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Taken.After(out[j].Taken) })
	return out, nil
}

// SnapshotChange is one difference between two snapshots.
type SnapshotChange struct {
	Key, State string // added, removed, changed
	Before     string
	After      string
}

// DiffSnapshots compares an older snapshot with a newer one.
func DiffSnapshots(old, cur *Snapshot) []SnapshotChange {
	var out []SnapshotChange
	for k, v := range cur.Objects {
		switch o, ok := old.Objects[k]; {
		case !ok:
			out = append(out, SnapshotChange{Key: k, State: "added", After: v})
		case o != v:
			out = append(out, SnapshotChange{Key: k, State: "changed", Before: o, After: v})
		}
	}
	for k, v := range old.Objects {
		if _, ok := cur.Objects[k]; !ok {
			out = append(out, SnapshotChange{Key: k, State: "removed", Before: v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
