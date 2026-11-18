package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const liveDep = `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  template:
    spec:
      containers:
      - name: app
        image: web:1
        env:
        - {name: DB_PASSWORD, value: hunter2}
        - {name: LOG_LEVEL, value: info}
`

func TestMaskAndUnmaskEnv(t *testing.T) {
	shown, err := MaskEnvYAML(liveDep, "")
	if err != nil || strings.Contains(shown, "hunter2") || !strings.Contains(shown, MaskedEnv) || !strings.Contains(shown, "info") {
		t.Fatalf("masked = %v\n%s", err, shown)
	}
	// edit something else: the password comes back from the cluster
	edited := strings.Replace(shown, "web:1", "web:2", 1)
	full, err := UnmaskEnvYAML(edited, liveDep)
	if err != nil || !strings.Contains(full, "hunter2") || !strings.Contains(full, "web:2") {
		t.Fatalf("unmasked = %v\n%s", err, full)
	}
	// a typed new value is kept and stays visible in the diff
	edited = strings.Replace(shown, MaskedEnv, "s3cret", 1)
	full, _ = UnmaskEnvYAML(edited, liveDep)
	if !strings.Contains(full, "s3cret") {
		t.Fatalf("new value lost:\n%s", full)
	}
	if again, _ := MaskEnvYAML(full, liveDep); !strings.Contains(again, "s3cret") {
		t.Fatalf("changed value masked:\n%s", again)
	}
	// a masked value for a new variable cannot be restored
	added := strings.Replace(shown, "LOG_LEVEL", "API_TOKEN", 1)
	added = strings.Replace(added, "value: info", "value: '"+MaskedEnv+"'", 1)
	if _, err := UnmaskEnvYAML(added, liveDep); err == nil {
		t.Fatal("masked value without a live value accepted")
	}
}

func TestComparableHashesEnv(t *testing.T) {
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":"DB_PASSWORD","value":"hunter2"}]}]}}}}`)); err != nil {
		t.Fatal(err)
	}
	out := Comparable(obj)
	if strings.Contains(out, "hunter2") || !strings.Contains(out, "<sha256 ") {
		t.Fatalf("comparable = %s", out)
	}
}

func TestMaskLastAppliedAnnotation(t *testing.T) {
	applied := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},"spec":{"template":{"spec":{"containers":[{"name":"app","env":[{"name":"DB_PASSWORD","value":"hunter2"}]}]}}}}`
	live := strings.Replace(liveDep, "metadata: {name: web, namespace: shop}",
		"metadata:\n  name: web\n  namespace: shop\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: '"+applied+"'", 1)
	shown, err := MaskEnvYAML(live, "")
	if err != nil || strings.Contains(shown, "hunter2") {
		t.Fatalf("password left in the edit view: %v\n%s", err, shown)
	}
	full, err := UnmaskEnvYAML(strings.Replace(shown, "web:1", "web:2", 1), live)
	if err != nil || !strings.Contains(full, applied) || !strings.Contains(full, "web:2") {
		t.Fatalf("annotation not restored exactly: %v\n%s", err, full)
	}
}
