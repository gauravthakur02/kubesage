package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GitOpsApp is an Argo CD Application or a Flux Kustomization/HelmRelease.
type GitOpsApp struct {
	Tool, Kind, Namespace, Name string
	Sync, Health                string
	Revision, Source            string
	Message                     string
	H                           Health
}

var gitopsKinds = []struct {
	tool, kind string
	gvrs       []schema.GroupVersionResource
}{
	{"Argo CD", "Application", []schema.GroupVersionResource{{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}}},
	{"Flux", "Kustomization", []schema.GroupVersionResource{{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Resource: "kustomizations"}}},
	{"Flux", "HelmRelease", []schema.GroupVersionResource{{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2beta2", Resource: "helmreleases"}}},
}

func str(o map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(o, path...)
	return s
}

func readyCondition(o map[string]any) (status, reason, msg string) {
	conds, _, _ := unstructured.NestedSlice(o, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			return fmt.Sprint(m["status"]), fmt.Sprint(m["reason"]), fmt.Sprint(m["message"])
		}
	}
	return "Unknown", "", ""
}

// GitOpsApps lists Argo CD Applications and Flux Kustomizations/HelmReleases in scope.
// found is false when neither tool is installed.
func (c *Client) GitOpsApps(ctx context.Context) (apps []GitOpsApp, found bool, err error) {
	if c.Dynamic == nil {
		return nil, false, fmt.Errorf("dynamic client unavailable")
	}
	var lastErr error
	for _, k := range gitopsKinds {
		for _, gvr := range k.gvrs {
			items, lerr := listAcross(ctx, c, c.Namespace, func(n string) ([]unstructured.Unstructured, error) {
				l, err := c.Dynamic.Resource(gvr).Namespace(n).List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				return l.Items, nil
			})
			if lerr != nil && len(items) == 0 {
				if !apierrors.IsNotFound(lerr) {
					lastErr = lerr
					found = true
				}
				continue
			}
			found = true
			for _, u := range items {
				o := u.Object
				a := GitOpsApp{Tool: k.tool, Kind: k.kind, Namespace: u.GetNamespace(), Name: u.GetName()}
				switch k.kind {
				case "Application":
					a.Sync, a.Health = str(o, "status", "sync", "status"), str(o, "status", "health", "status")
					a.Revision = str(o, "status", "sync", "revision")
					a.Source = strings.Trim(str(o, "spec", "source", "repoURL")+" "+str(o, "spec", "source", "path")+str(o, "spec", "source", "chart"), " ")
					a.Message = str(o, "status", "operationState", "message")
					if a.Health == "Degraded" || a.Health == "Missing" || str(o, "status", "operationState", "phase") == "Failed" {
						a.H = HealthErr
					} else if a.Sync != "Synced" || a.Health == "Progressing" {
						a.H = HealthWarn
					} else {
						a.H = HealthOK
					}
				default:
					st, reason, msg := readyCondition(o)
					a.Sync, a.Health, a.Message = reason, "Ready="+st, msg
					a.Revision = str(o, "status", "lastAppliedRevision")
					if a.Revision == "" {
						a.Revision = str(o, "status", "lastAttemptedRevision")
					}
					a.Source = str(o, "spec", "sourceRef", "kind") + "/" + str(o, "spec", "sourceRef", "name") + " " + str(o, "spec", "path")
					if k.kind == "HelmRelease" {
						a.Source = str(o, "spec", "chart", "spec", "chart") + " " + str(o, "spec", "chart", "spec", "version")
					}
					if s, _, _ := unstructured.NestedBool(o, "spec", "suspend"); s {
						a.Sync = "Suspended"
					}
					switch st {
					case "True":
						a.H = HealthOK
					case "False":
						a.H = HealthErr
					default:
						a.H = HealthWarn
					}
				}
				if len(a.Revision) > 20 {
					a.Revision = a.Revision[:20]
				}
				apps = append(apps, a)
			}
			break // first served version wins
		}
	}
	sort.SliceStable(apps, func(i, j int) bool {
		if apps[i].H != apps[j].H {
			return apps[i].H == HealthErr || (apps[i].H == HealthWarn && apps[j].H == HealthOK)
		}
		return apps[i].Name < apps[j].Name
	})
	if len(apps) == 0 && lastErr != nil {
		return nil, found, lastErr
	}
	return apps, found, nil
}
