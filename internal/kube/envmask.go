package kube

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"sigs.k8s.io/yaml"
)

// MaskedEnv replaces the value of a sensitive env var (…PASSWORD, …TOKEN) in YAML shown in the browser. Saving
// YAML that still holds it keeps the value that is in the cluster.
const MaskedEnv = "<masked: unchanged>"

// envValues collects the values of sensitive {name, value} pairs, keyed by their path (list items by name).
func envValues(v any, path string, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && sensitiveName.MatchString(name) {
			if s, ok := t["value"].(string); ok {
				out[path] = s
			}
		}
		for k, child := range t {
			envValues(child, path+"/"+k, out)
		}
	case []any:
		for i, child := range t {
			envValues(child, path+"/"+itemKey(child, i), out)
		}
	}
}

func itemKey(v any, i int) string {
	if m, ok := v.(map[string]any); ok {
		if n, ok := m["name"].(string); ok {
			return "name=" + n
		}
	}
	return strconv.Itoa(i)
}

// setEnvValues rewrites sensitive values in place: f gets the path and the current value and returns the new one.
func setEnvValues(v any, path string, f func(path, val string) string) {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && sensitiveName.MatchString(name) {
			if s, ok := t["value"].(string); ok {
				t["value"] = f(path, s)
			}
		}
		for k, child := range t {
			setEnvValues(child, path+"/"+k, f)
		}
	case []any:
		for i, child := range t {
			setEnvValues(child, path+"/"+itemKey(child, i), f)
		}
	}
}

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// appliedDoc parses the last-applied annotation (a JSON copy of the manifest, env values included).
func appliedDoc(m map[string]any) (map[string]any, string, bool) {
	md, _ := m["metadata"].(map[string]any)
	ann, _ := md["annotations"].(map[string]any)
	raw, ok := ann[lastApplied].(string)
	if !ok {
		return nil, "", false
	}
	var doc map[string]any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil, raw, false
	}
	return doc, raw, true
}

func setApplied(m map[string]any, v string) {
	m["metadata"].(map[string]any)["annotations"].(map[string]any)[lastApplied] = v
}

// maskTree masks sensitive env values in a parsed manifest; with live set, only values equal to the live ones.
func maskTree(m map[string]any, live map[string]string) {
	setEnvValues(m, "", func(path, val string) string {
		if live != nil {
			if old, ok := live[path]; !ok || old != val {
				return val
			}
		}
		return MaskedEnv
	})
}

// MaskEnvYAML masks sensitive env values in a manifest. With like set (the live manifest), only values equal to the
// live ones are masked, so a change someone typed or proposed stays visible.
func MaskEnvYAML(manifest, like string) (string, error) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &m); err != nil {
		return "", err
	}
	var live, liveApplied map[string]string
	if like != "" {
		var lm map[string]any
		if err := yaml.Unmarshal([]byte(like), &lm); err != nil {
			return "", err
		}
		live = map[string]string{}
		envValues(lm, "", live)
		liveApplied = map[string]string{}
		if doc, _, ok := appliedDoc(lm); ok {
			envValues(doc, "", liveApplied)
		}
	}
	maskTree(m, live)
	if doc, _, ok := appliedDoc(m); ok { // kubectl apply keeps a JSON copy of the manifest, passwords included
		maskTree(doc, liveApplied)
		b, _ := json.Marshal(doc)
		setApplied(m, string(b))
	}
	b, err := yaml.Marshal(m)
	return string(b), err
}

// UnmaskEnvYAML puts the live values back where edited YAML still holds MaskedEnv.
func UnmaskEnvYAML(edited, live string) (string, error) {
	var m, lm map[string]any
	if err := yaml.Unmarshal([]byte(edited), &m); err != nil {
		return "", fmt.Errorf("invalid YAML: %w", err)
	}
	if err := yaml.Unmarshal([]byte(live), &lm); err != nil {
		return "", err
	}
	var missing string
	restore := func(tree, liveTree map[string]any) {
		orig := map[string]string{}
		envValues(liveTree, "", orig)
		setEnvValues(tree, "", func(path, val string) string {
			if val != MaskedEnv {
				return val
			}
			if old, ok := orig[path]; ok {
				return old
			}
			missing = path
			return val
		})
	}
	restore(m, lm)
	if doc, _, ok := appliedDoc(m); ok {
		liveDoc, liveRaw, liveOK := appliedDoc(lm)
		restore(doc, liveDoc)
		if liveOK && reflect.DeepEqual(doc, liveDoc) {
			setApplied(m, liveRaw) // unchanged: keep the exact original string
		} else {
			b, _ := json.Marshal(doc)
			setApplied(m, string(b))
		}
	}
	if missing != "" {
		return "", fmt.Errorf("%s is still %q but has no value in the cluster to keep: type a value", missing, MaskedEnv)
	}
	b, err := yaml.Marshal(m)
	return string(b), err
}

// hashEnvValues replaces sensitive env values with a short hash: a changed value still shows as drift.
func hashEnvValues(v any) {
	setEnvValues(v, "", func(_, val string) string {
		return fmt.Sprintf("<sha256 %x>", sha256.Sum256([]byte(val)))[:22] + ">"
	})
}
