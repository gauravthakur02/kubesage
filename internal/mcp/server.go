// Package mcp serves kubesage's read-only cluster insights over the Model Context Protocol (stdio),
// so AI agents such as Claude Code can use them as tools. No tool changes the cluster.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

// ProtocolVersion is the MCP revision this server implements.
const ProtocolVersion = "2025-06-18"

// Server answers MCP requests using kube clients (one per context, created on demand).
type Server struct {
	Version    string
	Kubeconfig string
	Default    *kube.Client
	// New connects to another context (defaults to kube.New).
	New func(kubeconfig, context, namespace string) (*kube.Client, error)

	mu      sync.Mutex
	clients map[string]*kube.Client
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type prop struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	run         func(ctx context.Context, c *kube.Client, args map[string]any) (string, error)
}

func schema(required []string, props map[string]prop) map[string]any {
	p := map[string]any{"context": prop{"string", "kube-context to use (default: the one kubesage was started with)"}}
	for k, v := range props {
		p[k] = v
	}
	s := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(args map[string]any, k, d string) string {
	if v, ok := args[k].(string); ok && v != "" {
		return v
	}
	return d
}

var nsProp = prop{"string", `namespace ("" or "all" = every namespace you can read)`}

func (s *Server) tools() []tool {
	ro := map[string]any{"readOnlyHint": true, "openWorldHint": false}
	kindProp := prop{"string", "Ingress, Service, Deployment, StatefulSet, DaemonSet, ReplicaSet or Pod"}
	return []tool{
		{Name: "list_contexts", Description: "List the kube-contexts in the kubeconfig.", InputSchema: schema(nil, nil), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, _ map[string]any) (string, error) {
				return strings.Join(c.Contexts(), "\n"), nil
			}},
		{Name: "list_resources", Description: "List objects of a resource type (pods, deployments, services, events, nodes, configmaps, …) as a table with health.",
			InputSchema: schema([]string{"resource"}, map[string]prop{"resource": {"string", "resource name or alias, e.g. pods, deploy, svc, events"}, "namespace": nsProp}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				r, ok := kube.Lookup(str(a, "resource", ""))
				if !ok {
					return "", fmt.Errorf("unknown resource %q", str(a, "resource", ""))
				}
				t, err := r.List(ctx, c)
				if err != nil {
					return "", err
				}
				return formatTable(t), nil
			}},
		{Name: "find_issues", Description: "Detect problems (crashing pods, image pull errors, unavailable deployments, pending PVCs, …) with hints.",
			InputSchema: schema(nil, map[string]prop{"namespace": nsProp}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				is, err := c.Scan(ctx)
				if err != nil {
					return "", err
				}
				return kube.FormatIssues(is), nil
			}},
		{Name: "incident_report", Description: "Everything unhealthy in scope: issues, warning events of the last hour, failed Helm releases, unbound PVCs, node problems and recent rollouts.",
			InputSchema: schema(nil, map[string]prop{"namespace": nsProp}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				r, err := c.Incident(ctx)
				if err != nil {
					return "", err
				}
				return r.Format(), nil
			}},
		{Name: "relationships", Description: "The tree around an object: Ingress → Service → workload → ReplicaSet → Pods, plus ConfigMaps, Secrets, PVCs and HPA, with health.",
			InputSchema: schema([]string{"kind", "namespace", "name"}, map[string]prop{"kind": kindProp, "namespace": {"string", "namespace"}, "name": {"string", "object name"}}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				root, front, err := c.Relations(ctx, str(a, "kind", ""), str(a, "namespace", ""), str(a, "name", ""))
				if err != nil {
					return "", err
				}
				var b strings.Builder
				if front != nil && len(front.Children) > 0 {
					b.WriteString("reached through:\n")
					for _, f := range front.Children {
						writeGraph(&b, f, "  ")
					}
					b.WriteString("\n")
				}
				writeGraph(&b, root, "")
				return b.String(), nil
			}},
		{Name: "timeline", Description: "What happened to an object and its parts, newest first: events, rollouts, container restarts with exit codes, scaling, pod starts.",
			InputSchema: schema([]string{"kind", "namespace", "name"}, map[string]prop{"kind": kindProp, "namespace": {"string", "namespace"}, "name": {"string", "object name"}}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				tl, err := c.Timeline(ctx, str(a, "kind", ""), str(a, "namespace", ""), str(a, "name", ""))
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for i, e := range tl {
					if i >= 150 {
						break
					}
					fmt.Fprintf(&b, "%s  %-7s %s  %s\n", e.Time.UTC().Format(time.RFC3339), e.Source, e.Object, e.Text)
				}
				return b.String(), nil
			}},
		{Name: "pod_logs", Description: "Recent log lines of a pod container (optionally of the previous, crashed container).",
			InputSchema: schema([]string{"namespace", "pod"}, map[string]prop{"namespace": {"string", "namespace"}, "pod": {"string", "pod name"},
				"container": {"string", "container (default: first)"}, "previous": {"boolean", "logs of the previous container instance"}, "tail": {"integer", "lines (default 200, max 2000)"}}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				tail := int64(200)
				if v, ok := a["tail"].(float64); ok && v > 0 {
					tail = min(int64(v), 2000)
				}
				prev, _ := a["previous"].(bool)
				return c.LogsText(ctx, str(a, "namespace", ""), str(a, "pod", ""), kube.LogOptions{Container: str(a, "container", ""), Tail: tail, Previous: prev})
			}},
		{Name: "helm_releases", Description: "Helm releases (latest revision each) with status, chart and app version.",
			InputSchema: schema(nil, map[string]prop{"namespace": nsProp}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				rels, err := c.HelmReleases(ctx, c.Namespace)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, r := range rels {
					fmt.Fprintf(&b, "%s/%s rev %d %s %s-%s app %s updated %s\n", r.Namespace, r.Name, r.Revision, r.Status, r.Chart, r.ChartVersion, r.AppVersion, r.Updated.UTC().Format(time.RFC3339))
				}
				return b.String(), nil
			}},
		{Name: "upgrade_readiness", Description: "Deprecated/removed API usage (Helm manifests, last-applied, API server metrics) and node version skew for a target Kubernetes version.",
			InputSchema: schema(nil, map[string]prop{"target": {"string", `target version like "1.36" (default: next minor)`}, "namespace": nsProp}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				r, err := c.UpgradeReadiness(ctx, str(a, "target", ""))
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "server %s → target %s\n", r.Server, r.Target)
				for _, f := range r.Findings {
					state := "deprecated"
					if f.Blocking {
						state = "REMOVED"
					}
					fmt.Fprintf(&b, "%s %s %s (removed in %s, use %s) via %s %s\n", state, f.API.GroupVersion, f.API.Kind, f.API.Removed, f.API.Replacement, f.Source, f.Object)
				}
				for _, n := range r.Nodes {
					fmt.Fprintf(&b, "node %s pool %s kubelet %s\n", n.Name, n.Pool, n.Kubelet)
				}
				if r.AKS != "" {
					b.WriteString(r.AKS + "\n")
				}
				return b.String(), nil
			}},
		{Name: "right_sizing", Description: "Requests vs observed peak usage per workload, with suggested requests (needs metrics-server).",
			InputSchema: schema(nil, map[string]prop{"namespace": nsProp}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				rows, err := c.RightSizing(ctx, nil, nil)
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, s := range rows {
					fmt.Fprintf(&b, "%s/%s pods=%d cpu req=%dm peak=%dm suggest=%dm · mem req=%s peak=%s suggest=%s · %s\n", s.Namespace, s.Workload, s.Pods,
						s.CPUReq, s.CPUPeak, s.CPUSuggest, kube.FormatBytes(s.MemReq), kube.FormatBytes(s.MemPeak), kube.FormatBytes(s.MemSuggest), s.Verdict)
				}
				return b.String(), nil
			}},
		{Name: "my_access", Description: "What the current user may do in a namespace (SelfSubjectAccessReview for common resources and verbs).",
			InputSchema: schema([]string{"namespace"}, map[string]prop{"namespace": {"string", "namespace"}}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				cells, err := c.AccessMatrix(ctx, str(a, "namespace", "default"))
				if err != nil {
					return "", err
				}
				by := map[string][]string{}
				var order []string
				for _, cl := range cells {
					if _, ok := by[cl.Resource]; !ok {
						order = append(order, cl.Resource)
					}
					if cl.Allowed {
						by[cl.Resource] = append(by[cl.Resource], cl.Verb)
					} else if by[cl.Resource] == nil {
						by[cl.Resource] = []string{}
					}
				}
				var b strings.Builder
				for _, r := range order {
					v := strings.Join(by[r], ",")
					if v == "" {
						v = "(none)"
					}
					fmt.Fprintf(&b, "%-26s %s\n", r, v)
				}
				return b.String(), nil
			}},
		{Name: "search", Description: "Find pods, workloads, services, ingresses and configmaps by name, image, IP, host or label (key=value) in all namespaces.",
			InputSchema: schema([]string{"query"}, map[string]prop{"query": {"string", "text, image, IP, host or label key=value"}}), Annotations: ro,
			run: func(ctx context.Context, c *kube.Client, a map[string]any) (string, error) {
				hits, err := c.Search(ctx, str(a, "query", ""))
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "%s %s/%s (matched %s)\n", h.Kind, h.Namespace, h.Name, h.Match)
				}
				return b.String(), nil
			}},
	}
}

func writeGraph(b *strings.Builder, n *kube.GraphNode, indent string) {
	health := map[kube.Health]string{kube.HealthOK: "ok", kube.HealthWarn: "degraded", kube.HealthErr: "FAILING", kube.HealthNeutral: "-"}[n.Health]
	fmt.Fprintf(b, "%s%s %s [%s] %s %s\n", indent, n.Kind, n.Name, health, n.Status, n.Note)
	for _, c := range n.Children {
		writeGraph(b, c, indent+"  ")
	}
}

func formatTable(t kube.Table) string {
	var b strings.Builder
	b.WriteString(strings.Join(t.Columns, "\t") + "\n")
	health := map[kube.Health]string{kube.HealthOK: "", kube.HealthWarn: " [degraded]", kube.HealthErr: " [FAILING]", kube.HealthNeutral: ""}
	for _, r := range t.Rows {
		cells := make([]string, len(r.Cells))
		for i, c := range r.Cells {
			cells[i] = stripANSI(c)
		}
		b.WriteString(strings.Join(cells, "\t") + health[r.Health] + "\n")
	}
	return b.String()
}

func stripANSI(s string) string {
	var b strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			esc = true
		case esc && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			esc = false
		case !esc:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// client returns a client for the requested context and namespace.
func (s *Server) client(args map[string]any) (*kube.Client, error) {
	ctxName := str(args, "context", s.Default.Context)
	var c *kube.Client
	if ctxName == s.Default.Context {
		c = s.Default
	} else {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.clients == nil {
			s.clients = map[string]*kube.Client{}
		}
		if c = s.clients[ctxName]; c == nil {
			newFn := s.New
			if newFn == nil {
				newFn = kube.New
			}
			var err error
			if c, err = newFn(s.Kubeconfig, ctxName, ""); err != nil {
				return nil, err
			}
			s.clients[ctxName] = c
		}
	}
	if ns, ok := args["namespace"].(string); ok {
		if ns == "all" {
			ns = kube.AllNamespaces
		}
		return c.WithNamespace(ns), nil
	}
	return c, nil
}

// Serve reads newline-delimited JSON-RPC requests from in and writes responses to out until EOF.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	enc := json.NewEncoder(out)
	var wmu sync.Mutex
	send := func(r response) {
		wmu.Lock()
		defer wmu.Unlock()
		r.JSONRPC = "2.0"
		_ = enc.Encode(r)
	}
	tools := s.tools()
	byName := map[string]tool{}
	for _, t := range tools {
		byName[t.Name] = t
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			send(response{ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
			continue
		}
		if len(req.ID) == 0 { // notification (e.g. notifications/initialized)
			continue
		}
		switch req.Method {
		case "initialize":
			send(response{ID: req.ID, Result: map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "kubesage", "version": s.Version},
				"instructions":    "Read-only Kubernetes insights from kubesage. Start with incident_report or find_issues, then relationships, timeline and pod_logs for a specific object. No tool changes the cluster.",
			}})
		case "ping":
			send(response{ID: req.ID, Result: map[string]any{}})
		case "tools/list":
			list := make([]tool, len(tools))
			copy(list, tools)
			sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
			send(response{ID: req.ID, Result: map[string]any{"tools": list}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				send(response{ID: req.ID, Error: &rpcError{-32602, "invalid params"}})
				continue
			}
			t, ok := byName[p.Name]
			if !ok {
				send(response{ID: req.ID, Error: &rpcError{-32602, "unknown tool " + p.Name}})
				continue
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			wg.Add(1)
			go func(id json.RawMessage) {
				defer wg.Done()
				cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
				defer cancel()
				text, err := "", error(nil)
				c, cerr := s.client(p.Arguments)
				if cerr != nil {
					err = cerr
				} else {
					text, err = t.run(cctx, c, p.Arguments)
				}
				if err != nil {
					send(response{ID: id, Result: map[string]any{"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}}, "isError": true}})
					return
				}
				if strings.TrimSpace(text) == "" {
					text = "(nothing found)"
				}
				send(response{ID: id, Result: map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}})
			}(req.ID)
		default:
			send(response{ID: req.ID, Error: &rpcError{-32601, "method not found: " + req.Method}})
		}
	}
	return sc.Err()
}
