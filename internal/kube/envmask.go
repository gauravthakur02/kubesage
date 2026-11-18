package kube

import (
	"crypto/sha256"
	"fmt"
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

// MaskEnvYAML masks sensitive env values in a manifest. With like set (the live manifest), only values equal to the
// live ones are masked, so a change someone typed or proposed stays visible.
func MaskEnvYAML(manifest, like string) (string, error) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &m); err != nil {
		return "", err
	}
	var live map[string]string
	if like != "" {
		var lm map[string]any
		if err := yaml.Unmarshal([]byte(like), &lm); err != nil {
			return "", err
		}
		live = map[string]string{}
		envValues(lm, "", live)
	}
	setEnvValues(m, "", func(path, val string) string {
		if live != nil {
			if old, ok := live[path]; !ok || old != val {
				return val
			}
		}
		return MaskedEnv
	})
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
	orig := map[string]string{}
	envValues(lm, "", orig)
	var missing string
	setEnvValues(m, "", func(path, val string) string {
		if val != MaskedEnv {
			return val
		}
		if old, ok := orig[path]; ok {
			return old
		}
		missing = path
		return val
	})
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
