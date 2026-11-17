// Package web is kubesage's browser UI: a JSON API over the kube package plus a small app (HTML, CSS, JS)
// embedded in the binary. kubesage serve mounts it; the terminal UI stays available at /term.
package web

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubesage/internal/audit"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

//go:embed static
var static embed.FS

// App serves the API for one kubeconfig.
type App struct {
	Kubeconfig     string
	DefaultContext string
	DefaultNS      string
	ReadOnly       bool
	Danger         *regexp.Regexp
	AuditPath      string
	Version        string
	Favorites      []string
	Redact         bool

	mu      sync.Mutex
	clients map[string]*kube.Client
	crds    map[string][]kube.Resource
}

// Assets serves /static/.
func Assets() http.Handler {
	sub, _ := fs.Sub(static, "static")
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// Index is the app page with the session token filled in.
func Index(token string) []byte {
	b, _ := static.ReadFile("static/index.html")
	return []byte(strings.ReplaceAll(string(b), "{{TOKEN}}", token))
}

func (a *App) client(name string) (*kube.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clients == nil {
		a.clients = map[string]*kube.Client{}
	}
	if name == "" {
		name = a.DefaultContext
	}
	if c, ok := a.clients[name]; ok {
		return c, nil
	}
	c, err := kube.New(a.Kubeconfig, name, a.DefaultNS)
	if err != nil {
		return nil, err
	}
	a.clients[c.Context] = c
	if name == "" {
		a.DefaultContext = c.Context
	}
	return c, nil
}

// scoped returns the client for ctx narrowed to namespace ns ("all" or "" = all namespaces).
func (a *App) scoped(ctxName, ns string) (*kube.Client, error) {
	c, err := a.client(ctxName)
	if err != nil {
		return nil, err
	}
	if ns == "all" || ns == "" {
		return c.WithNamespace(kube.AllNamespaces), nil
	}
	return c.WithNamespace(ns), nil
}

func (a *App) resource(ctxName, name string) (kube.Resource, bool) {
	if r, ok := kube.Lookup(name); ok {
		return r, true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return kube.FindIn(a.crds[ctxName], name)
}

var healthName = map[kube.Health]string{kube.HealthOK: "ok", kube.HealthWarn: "warn", kube.HealthErr: "err", kube.HealthNeutral: "neutral"}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func reqCtx(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// Handler serves /api/*.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/meta", a.meta)
	mux.HandleFunc("GET /api/resources", a.resources)
	mux.HandleFunc("GET /api/namespaces", a.namespaces)
	mux.HandleFunc("GET /api/list", a.list)
	mux.HandleFunc("GET /api/describe", a.text("describe"))
	mux.HandleFunc("GET /api/yaml", a.text("yaml"))
	mux.HandleFunc("GET /api/timeline", a.timeline)
	mux.HandleFunc("GET /api/graph", a.graph)
	mux.HandleFunc("GET /api/pods", a.pods)
	mux.HandleFunc("GET /api/logs", a.logs)
	mux.HandleFunc("GET /api/issues", a.issues)
	mux.HandleFunc("POST /api/action", a.action)
	return mux
}

type contextInfo struct {
	Name    string `json:"name"`
	Cluster string `json:"cluster"`
	Danger  bool   `json:"danger"`
}

func (a *App) meta(w http.ResponseWriter, r *http.Request) {
	c, err := a.client("")
	if err != nil {
		fail(w, 500, err)
		return
	}
	var ctxs []contextInfo
	for _, n := range c.Contexts() {
		ctxs = append(ctxs, contextInfo{Name: n, Cluster: c.ClusterOf(n), Danger: a.Danger != nil && a.Danger.MatchString(n)})
	}
	ns := a.DefaultNS
	if ns == "" {
		ns = c.Namespace
	}
	if ns == "" {
		ns = "all"
	}
	writeJSON(w, map[string]any{"version": a.Version, "readOnly": a.ReadOnly, "contexts": ctxs, "context": c.Context,
		"namespace": ns, "favorites": a.Favorites, "groups": kube.GroupOrder})
}

type resourceInfo struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Short      string `json:"short"`
	Group      string `json:"group"`
	APIGroup   string `json:"apiGroup,omitempty"`
	Namespaced bool   `json:"namespaced"`
	Logs       bool   `json:"logs"`
	Scale      bool   `json:"scale"`
	Restart    bool   `json:"restart"`
	Graph      bool   `json:"graph"`
}

var graphKinds = map[string]bool{"Gateway": true, "HTTPRoute": true, "Ingress": true, "Service": true, "Deployment": true, "StatefulSet": true, "DaemonSet": true, "ReplicaSet": true, "Pod": true}

func info(r kube.Resource) resourceInfo {
	return resourceInfo{Name: r.Name, Kind: r.Kind, Short: r.Short, Group: r.Group, APIGroup: r.GVR.Group, Namespaced: r.Namespaced,
		Logs: r.CanLogs(), Scale: r.CanScale(), Restart: r.CanRestart(), Graph: graphKinds[r.Kind]}
}

func (a *App) resources(w http.ResponseWriter, r *http.Request) {
	ctxName := r.URL.Query().Get("ctx")
	var out []resourceInfo
	for _, res := range kube.Resources() {
		out = append(out, info(res))
	}
	c, err := a.client(ctxName)
	if err == nil {
		a.mu.Lock()
		crds, cached := a.crds[c.Context]
		a.mu.Unlock()
		if !cached {
			ctx, cancel := reqCtx(r, 20*time.Second)
			crds, _ = c.Discover(ctx)
			cancel()
			a.mu.Lock()
			if a.crds == nil {
				a.crds = map[string][]kube.Resource{}
			}
			a.crds[c.Context] = crds
			a.mu.Unlock()
		}
		for _, res := range crds {
			out = append(out, info(res))
		}
	}
	writeJSON(w, out)
}

func (a *App) namespaces(w http.ResponseWriter, r *http.Request) {
	c, err := a.client(r.URL.Query().Get("ctx"))
	if err != nil {
		fail(w, 500, err)
		return
	}
	ctx, cancel := reqCtx(r, 15*time.Second)
	defer cancel()
	names := []string{"all"}
	l, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil { // namespace-scoped access: offer the context's own namespace
		if c.Namespace != "" {
			names = append(names, c.Namespace)
		}
		writeJSON(w, map[string]any{"names": names, "note": "cannot list namespaces: " + err.Error()})
		return
	}
	for _, n := range l.Items {
		names = append(names, n.Name)
	}
	writeJSON(w, map[string]any{"names": names})
}

type rowJSON struct {
	NS     string   `json:"ns"`
	Name   string   `json:"name"`
	Cells  []string `json:"cells"`
	Health string   `json:"health"`
}

func (a *App) target(r *http.Request) (*kube.Client, kube.Resource, string, string, error) {
	q := r.URL.Query()
	c, err := a.scoped(q.Get("ctx"), q.Get("ns"))
	if err != nil {
		return nil, kube.Resource{}, "", "", err
	}
	res, ok := a.resource(c.Context, q.Get("res"))
	if !ok {
		return nil, kube.Resource{}, "", "", fmt.Errorf("unknown resource %q", q.Get("res"))
	}
	return c, res, q.Get("ns"), q.Get("name"), nil
}

func (a *App) list(w http.ResponseWriter, r *http.Request) {
	c, res, _, _, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	t, err := res.List(ctx, c)
	if err != nil {
		fail(w, 502, err)
		return
	}
	rows := make([]rowJSON, 0, len(t.Rows))
	for _, row := range t.Rows {
		rows = append(rows, rowJSON{NS: row.Namespace, Name: row.Name, Cells: row.Cells, Health: healthName[row.Health]})
	}
	writeJSON(w, map[string]any{"columns": t.Columns, "rows": rows, "note": t.Note, "at": time.Now().Format(time.RFC3339)})
}

func (a *App) text(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, res, ns, name, err := a.target(r)
		if err != nil {
			fail(w, 400, err)
			return
		}
		ctx, cancel := reqCtx(r, 30*time.Second)
		defer cancel()
		var out string
		if kind == "yaml" {
			out, err = c.YAML(ctx, res, ns, name, false) // secret values stay masked in the browser
		} else {
			out, err = c.Describe(ctx, res, ns, name)
		}
		if err != nil {
			fail(w, 502, err)
			return
		}
		writeJSON(w, map[string]string{"text": out})
	}
}

func (a *App) timeline(w http.ResponseWriter, r *http.Request) {
	c, res, ns, name, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	tl, err := c.Timeline(ctx, res.Kind, ns, name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	type entry struct {
		Time, Source, Object, Text, Health string
		Count                              int32
	}
	out := make([]entry, 0, len(tl))
	for _, e := range tl {
		out = append(out, entry{Time: e.Time.Format(time.RFC3339), Source: e.Source, Object: e.Object, Text: e.Text, Health: healthName[e.Health], Count: e.Count})
	}
	writeJSON(w, out)
}

type nodeJSON struct {
	Kind     string      `json:"kind"`
	NS       string      `json:"ns"`
	Name     string      `json:"name"`
	Status   string      `json:"status"`
	Health   string      `json:"health"`
	Note     string      `json:"note,omitempty"`
	Children []*nodeJSON `json:"children,omitempty"`
}

func toNode(n *kube.GraphNode) *nodeJSON {
	if n == nil {
		return nil
	}
	j := &nodeJSON{Kind: n.Kind, NS: n.Namespace, Name: n.Name, Status: n.Status, Health: healthName[n.Health], Note: n.Note}
	for _, c := range n.Children {
		j.Children = append(j.Children, toNode(c))
	}
	return j
}

func (a *App) graph(w http.ResponseWriter, r *http.Request) {
	c, res, ns, name, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	root, front, err := c.Relations(ctx, res.Kind, ns, name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]any{"root": toNode(root), "front": toNode(front)})
}

func (a *App) pods(w http.ResponseWriter, r *http.Request) {
	c, res, ns, name, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := reqCtx(r, 20*time.Second)
	defer cancel()
	pods, err := c.PodsFor(ctx, res, ns, name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	type pod struct {
		Name       string   `json:"name"`
		Containers []string `json:"containers"`
	}
	out := []pod{}
	for i := range pods {
		out = append(out, pod{Name: pods[i].Name, Containers: kube.Containers(&pods[i])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

// logs streams container logs as server-sent events (one event per line).
func (a *App) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := a.scoped(q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	tail, _ := strconv.ParseInt(q.Get("tail"), 10, 64)
	if tail <= 0 || tail > 5000 {
		tail = 500
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	rc, err := c.Logs(r.Context(), q.Get("ns"), q.Get("pod"), kube.LogOptions{Container: q.Get("container"), Tail: tail,
		Follow: q.Get("follow") != "0", Previous: q.Get("previous") == "1"})
	if err != nil {
		fail(w, 502, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	last := time.Now()
	for sc.Scan() {
		b, _ := json.Marshal(sc.Text())
		fmt.Fprintf(w, "data: %s\n\n", b)
		if time.Since(last) > 100*time.Millisecond {
			fl.Flush()
			last = time.Now()
		}
	}
	fmt.Fprint(w, "event: end\ndata: \"\"\n\n")
	fl.Flush()
}

func (a *App) issues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := a.scoped(q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := reqCtx(r, 40*time.Second)
	defer cancel()
	is, err := c.Scan(ctx)
	if err != nil {
		fail(w, 502, err)
		return
	}
	type issue struct {
		Severity, Kind, NS, Name, Reason, Message, Hint string
	}
	out := make([]issue, 0, len(is))
	for _, i := range is {
		out = append(out, issue{healthName[i.Severity], i.Kind, i.Namespace, i.Name, i.Reason, i.Message, i.Hint})
	}
	writeJSON(w, out)
}

type actionReq struct {
	Ctx, NS, Res, Name, Action string
	Replicas                   int
	Confirm                    string
}

// action changes the cluster: scale, restart or delete. Refused in read-only mode; production-like contexts need
// the object's name typed as confirmation. Every attempt is written to the audit log.
func (a *App) action(w http.ResponseWriter, r *http.Request) {
	if a.ReadOnly {
		fail(w, 403, fmt.Errorf("read-only: start kubesage serve with --allow-changes to make changes"))
		return
	}
	var req actionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	c, err := a.scoped(req.Ctx, req.NS)
	if err != nil {
		fail(w, 400, err)
		return
	}
	res, ok := a.resource(c.Context, req.Res)
	if !ok {
		fail(w, 400, fmt.Errorf("unknown resource %q", req.Res))
		return
	}
	if a.Danger != nil && a.Danger.MatchString(c.Context) && req.Confirm != req.Name {
		fail(w, 412, fmt.Errorf("%s is production-like: type the object name to confirm", c.Context))
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	desc := req.Action
	switch req.Action {
	case "scale":
		if req.Replicas < 0 || req.Replicas > 1000 {
			fail(w, 400, fmt.Errorf("replicas must be 0-1000"))
			return
		}
		desc = fmt.Sprintf("scale to %d", req.Replicas)
		err = c.Scale(ctx, res, req.NS, req.Name, req.Replicas)
	case "restart":
		desc = "rollout restart"
		err = c.Restart(ctx, res, req.NS, req.Name)
	case "delete":
		err = c.Delete(ctx, res, req.NS, req.Name)
	default:
		fail(w, 400, fmt.Errorf("unknown action %q", req.Action))
		return
	}
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	if a.AuditPath != "" {
		_ = audit.Append(a.AuditPath, audit.Entry{Time: time.Now().UTC(), Context: c.Context, NS: req.NS, Action: "web: " + desc,
			Target: res.Name + "/" + req.Name, By: "web", Result: result})
	}
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]string{"status": desc + " " + req.Name})
}
