package kube

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

func cm(name, val, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "shop", "uid": uid, "resourceVersion": uid},
		"data":     map[string]any{"LOG_LEVEL": val}}}
}

func TestCompareNamespace(t *testing.T) {
	gvr := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	kinds := map[schema.GroupVersionResource]string{gvr: "ConfigMapList"}
	here := &Client{Context: "staging", Dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds,
		cm("same", "info", "1"), cm("changed", "debug", "2"), cm("new", "x", "3"))}
	there := &Client{Context: "prod", Dynamic: dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds,
		cm("same", "info", "9"), cm("changed", "info", "8"), cm("old", "x", "7"))}
	r, _ := Lookup("configmaps")
	got, err := CompareNamespace(context.Background(), here, there, r, "shop")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"same": "same", "changed": "differs", "new": "only-here", "old": "only-there"}
	for _, e := range got {
		if want[e.Name] != e.State {
			t.Errorf("%s: %s, want %s", e.Name, e.State, want[e.Name])
		}
	}
	if got[0].State != "differs" {
		t.Errorf("differences should come first: %+v", got)
	}
	if strings.Contains(Comparable(cm("a", "b", "123")), "123") {
		t.Error("uid/resourceVersion must be ignored")
	}
}

func TestComparableHashesSecrets(t *testing.T) {
	s := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "db"}, "data": map[string]any{"password": "aHVudGVyMg=="}}}
	y := Comparable(s)
	if strings.Contains(y, "aHVudGVyMg") || !strings.Contains(y, "sha256") {
		t.Fatalf("secret must be compared by hash:\n%s", y)
	}
}

func TestSplitImage(t *testing.T) {
	cases := map[string][3]string{
		"nginx":                                 {"docker.io", "nginx", ""},
		"nginx:1.27":                            {"docker.io", "nginx", "1.27"},
		"myacr.azurecr.io/team/api:2.1":         {"myacr.azurecr.io", "team/api", "2.1"},
		"localhost:5000/app@sha256:abc":         {"localhost:5000", "app", ""},
		"ghcr.io/org/tool:v1@sha256:deadbeef00": {"ghcr.io", "org/tool", "v1"},
	}
	for in, w := range cases {
		r, repo, tag := SplitImage(in)
		if r != w[0] || repo != w[1] || tag != w[2] {
			t.Errorf("%s → %s %s %s, want %v", in, r, repo, tag, w)
		}
	}
}

func TestGitOpsOwner(t *testing.T) {
	if got := GitOpsOwner(nil, map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/web"}); got != "managed by Argo CD app shop" {
		t.Error(got)
	}
	if got := GitOpsOwner(map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"}, nil); !strings.Contains(got, "Flux Kustomization flux-system/apps") {
		t.Error(got)
	}
	if GitOpsOwner(nil, nil) != "" {
		t.Error("plain object has no owner")
	}
}
