package kube

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// CustomRule is a user-defined policy check (policy as code).
type CustomRule struct {
	Name               string
	Kinds              []string
	Namespaces         string
	Severity           string
	RequireLabels      []string
	RequireAnnotations []string
	Path               string
	Match              string
	Forbid             string
	Message            string
}

type compiledRule struct {
	CustomRule
	ns, match, forbid *regexp.Regexp
	sev               Health
}

func compileRule(r CustomRule) (compiledRule, error) {
	cr := compiledRule{CustomRule: r, sev: HealthWarn}
	if strings.EqualFold(r.Severity, "error") {
		cr.sev = HealthErr
	}
	if r.Name == "" {
		return cr, fmt.Errorf("rule without a name")
	}
	if len(cr.Kinds) == 0 {
		cr.Kinds = []string{"deployments", "statefulsets", "daemonsets"}
	}
	var err error
	for _, x := range []struct {
		src string
		dst **regexp.Regexp
	}{{r.Namespaces, &cr.ns}, {r.Match, &cr.match}, {r.Forbid, &cr.forbid}} {
		if x.src == "" {
			continue
		}
		if *x.dst, err = regexp.Compile(x.src); err != nil {
			return cr, fmt.Errorf("rule %q: %w", r.Name, err)
		}
	}
	if r.Path == "" && (r.Match != "" || r.Forbid != "") {
		return cr, fmt.Errorf("rule %q: match/forbid need a path", r.Name)
	}
	return cr, nil
}

// PathValues returns every value at a dotted path; "[*]" walks all list items, "[N]" one item.
// found is false when the path does not exist at all.
func PathValues(obj map[string]any, path string) (vals []string, found bool) {
	var walk func(v any, parts []string)
	walk = func(v any, parts []string) {
		if len(parts) == 0 {
			if v == nil {
				return
			}
			found = true
			switch t := v.(type) {
			case string:
				vals = append(vals, t)
			default:
				vals = append(vals, fmt.Sprint(t))
			}
			return
		}
		p := parts[0]
		key, idx := p, ""
		if i := strings.Index(p, "["); i >= 0 && strings.HasSuffix(p, "]") {
			key, idx = p[:i], p[i+1:len(p)-1]
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		next, ok := m[key]
		if !ok {
			return
		}
		if idx == "" {
			walk(next, parts[1:])
			return
		}
		list, ok := next.([]any)
		if !ok {
			return
		}
		for i, item := range list {
			if idx == "*" || idx == fmt.Sprint(i) {
				walk(item, parts[1:])
			}
		}
	}
	walk(obj, strings.Split(path, "."))
	return vals, found
}

// EvalRule checks one object against a rule and returns the problem ("" when it passes).
func evalRule(r compiledRule, o *unstructured.Unstructured) string {
	var miss []string
	for _, l := range r.RequireLabels {
		if _, ok := o.GetLabels()[l]; !ok {
			miss = append(miss, "label "+l)
		}
	}
	for _, an := range r.RequireAnnotations {
		if _, ok := o.GetAnnotations()[an]; !ok {
			miss = append(miss, "annotation "+an)
		}
	}
	if len(miss) > 0 {
		return "missing " + strings.Join(miss, ", ")
	}
	if r.Path == "" {
		return ""
	}
	vals, found := PathValues(o.Object, r.Path)
	if !found {
		if r.forbid != nil && r.match == nil {
			return "" // nothing there, so nothing forbidden
		}
		return r.Path + " is not set"
	}
	for _, v := range vals {
		if r.forbid != nil && r.forbid.MatchString(v) {
			return fmt.Sprintf("%s = %q is not allowed", r.Path, v)
		}
		if r.match != nil && !r.match.MatchString(v) {
			return fmt.Sprintf("%s = %q does not match %s", r.Path, v, r.Match)
		}
	}
	// Lists: a [*] path with fewer values than items means some item lacks the field.
	if r.forbid == nil && strings.Contains(r.Path, "[*]") {
		prefix := r.Path[:strings.LastIndex(r.Path, "[*]")+3]
		if n := countItems(o.Object, prefix); n > len(vals) {
			return fmt.Sprintf("%s is not set on %d of %d items", r.Path, n-len(vals), n)
		}
	}
	return ""
}

func countItems(obj map[string]any, listPath string) int {
	n := 0
	walkPrefix := strings.TrimSuffix(listPath, "[*]")
	parts := strings.Split(walkPrefix, ".")
	var v any = obj
	for _, p := range parts {
		m, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		key, idx := p, ""
		if i := strings.Index(p, "["); i >= 0 {
			key, idx = p[:i], p[i+1:len(p)-1]
		}
		v = m[key]
		if idx != "" {
			return 0 // nested lists before the last [*]: skip the count check
		}
	}
	if l, ok := v.([]any); ok {
		n = len(l)
	}
	return n
}

// RuleScan runs the custom rules against objects in the client's namespace (or all namespaces).
func (c *Client) RuleScan(ctx context.Context, rules []CustomRule, skip map[string]bool) ([]Finding, error) {
	var out []Finding
	for _, raw := range rules {
		r, err := compileRule(raw)
		if err != nil {
			return nil, err
		}
		for _, kind := range r.Kinds {
			res, ok := Lookup(kind)
			if !ok || res.GVR.Resource == "" {
				return nil, fmt.Errorf("rule %q: unknown kind %q", r.Name, kind)
			}
			ns := c.Namespace
			if !res.Namespaced {
				ns = ""
			}
			items, err := listAcross(ctx, c, ns, func(n string) ([]unstructured.Unstructured, error) {
				l, err := c.Dynamic.Resource(res.GVR).Namespace(n).List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				return l.Items, nil
			})
			if err != nil {
				continue // not allowed or not served: skip quietly
			}
			for i := range items {
				o := &items[i]
				if skip[o.GetNamespace()] || (r.ns != nil && !r.ns.MatchString(o.GetNamespace())) {
					continue
				}
				if msg := evalRule(r, o); msg != "" {
					hint := r.Message
					if hint == "" {
						hint = "custom rule " + r.Name
					}
					out = append(out, Finding{Severity: r.sev, Check: r.Name, Kind: res.Name, Namespace: o.GetNamespace(), Workload: o.GetName(), Message: msg, Hint: hint})
				}
			}
		}
	}
	return out, nil
}
