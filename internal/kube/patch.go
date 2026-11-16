package kube

import (
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

// MergePatchLocally applies a YAML patch to an object's YAML without contacting the cluster: a strategic merge
// patch for built-in kinds (lists such as containers merge by name), a JSON merge patch for everything else.
func MergePatchLocally(gvk schema.GroupVersionKind, original, patch string) (string, error) {
	if strings.Contains(patch, "<redacted") {
		return "", fmt.Errorf("the patch touches a redacted value; refusing so real values are not overwritten")
	}
	origJSON, err := yaml.YAMLToJSON([]byte(original))
	if err != nil {
		return "", fmt.Errorf("current object: %w", err)
	}
	patchJSON, err := yaml.YAMLToJSON([]byte(patch))
	if err != nil {
		return "", fmt.Errorf("patch is not valid YAML: %w", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(patchJSON, &probe); err != nil || len(probe) == 0 {
		return "", fmt.Errorf("patch must be a YAML object with the fields to change")
	}
	if md, ok := probe["metadata"].(map[string]any); ok {
		if _, ok := md["name"]; ok {
			return "", fmt.Errorf("the patch changes metadata.name")
		}
		if _, ok := md["namespace"]; ok {
			return "", fmt.Errorf("the patch changes metadata.namespace")
		}
	}
	var merged []byte
	if obj, err := scheme.Scheme.New(gvk); err == nil {
		merged, err = strategicpatch.StrategicMergePatch(origJSON, patchJSON, obj)
		if err != nil {
			return "", err
		}
	} else {
		if merged, err = jsonpatch.MergePatch(origJSON, patchJSON); err != nil {
			return "", err
		}
	}
	out, err := yaml.JSONToYAML(merged)
	return string(out), err
}
