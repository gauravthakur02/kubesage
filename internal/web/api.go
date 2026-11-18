// Package web is kubeweave's browser UI: a JSON API over the kube package plus a small app (HTML, CSS, JS)
// embedded in the binary. kubeweave serve mounts it; the terminal UI stays available at /term.
package web

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gauravthakur02/kubeweave/internal/config"
	"github.com/gauravthakur02/kubeweave/internal/kube"
	"github.com/gauravthakur02/kubeweave/internal/owners"
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
	// AI returns the provider for a context (nil = no AI in the browser).
	AI       AIFunc
	LogLines int
	// PromURL gives the Prometheus URL configured for a context ("" = none); PromToken is its bearer-token command.
	PromURL   func(kctx string) string
	PromToken string
	// Owners are the ownership rules from the config.
	Owners []owners.Rule
	// SLOs and SnapshotBase (folder for saved snapshots) come from the config.
	SLOs         []config.SLO
	SnapshotBase string
	// Contexts limits which kube-contexts the web UI offers (nil = all in the kubeconfig).
	Contexts *regexp.Regexp
	// ForwardBind is the address port-forward proxies listen on (127.0.0.1, or 0.0.0.0 when serving the network).
	ForwardBind string
	// Impersonate makes every request act as the signed-in user (needs the server's credentials to allow impersonation).
	Impersonate bool

	mu       sync.Mutex
	clients  map[string]*kube.Client
	crds     map[string][]kube.Resource
	samples  map[string][]sample
	forwards map[int]*webForward
	nextFwd  int
	users    map[string]*kube.Client // impersonated clients per context and user
}

// AssetVersion is added to script and style URLs (the kubeweave version), so an upgrade never shows a cached old app.
var AssetVersion = "dev"

// Assets serves /static/. Browsers must revalidate, so a restarted server always delivers its own files.
func Assets() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}

// Index is the app page with the session token and asset version filled in.
func Index(token string) []byte {
	b, _ := static.ReadFile("static/index.html")
	v := url.QueryEscape(AssetVersion)
	return []byte(strings.NewReplacer("{{TOKEN}}", token, "/static/app.js", "/static/app.js?v="+v, "/static/app.css", "/static/app.css?v="+v).Replace(string(b)))
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
	if name != "" && a.Contexts != nil && !a.Contexts.MatchString(name) {
		return nil, fmt.Errorf("context %q is not offered by this kubeweave (serve.contexts)", name)
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

// scopedFor is scoped acting as the signed-in user when impersonation is on (clients are cached per user).
func (a *App) scopedFor(r *http.Request, ctxName, ns string) (*kube.Client, error) {
	u := UserFrom(r)
	if !a.Impersonate || u == nil {
		return a.scoped(ctxName, ns)
	}
	base, err := a.client(ctxName)
	if err != nil {
		return nil, err
	}
	key := base.Context + "\x00" + u.Email + "\x00" + strings.Join(u.Groups, ",")
	a.mu.Lock()
	c, ok := a.users[key]
	a.mu.Unlock()
	if !ok {
		if c, err = base.Impersonate(u.Email, u.Groups); err != nil {
			return nil, err
		}
		a.mu.Lock()
		if a.users == nil {
			a.users = map[string]*kube.Client{}
		}
		a.users[key] = c
		a.mu.Unlock()
	}
	if ns == "all" || ns == "" {
		return c.WithNamespace(kube.AllNamespaces), nil
	}
	return c.WithNamespace(ns), nil
}

// who is the name recorded in the audit log.
func (a *App) who(r *http.Request) string {
	if u := UserFrom(r); u != nil {
		return "web:" + u.Email
	}
	return "web"
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
	mux.HandleFunc("GET /api/logs/download", a.logsDownload)
	mux.HandleFunc("GET /api/logs/bundle", a.logsBundle)
	mux.HandleFunc("GET /api/issues", a.issues)
	mux.HandleFunc("POST /api/action", a.action)
	mux.HandleFunc("GET /api/edit", a.editYAML)
	mux.HandleFunc("POST /api/dryrun", a.dryRun)
	mux.HandleFunc("POST /api/apply", a.applyYAML)
	mux.HandleFunc("GET /api/compare", a.compare)
	mux.HandleFunc("POST /api/ai/analyze", a.aiAnalyze)
	mux.HandleFunc("POST /api/ai/chat", a.aiChat)
	mux.HandleFunc("POST /api/ai/fix", a.aiFix)
	mux.HandleFunc("POST /api/forward", a.forwardStart)
	mux.HandleFunc("GET /api/forwards", a.forwardList)
	mux.HandleFunc("POST /api/forward/stop", a.forwardStop)
	mux.HandleFunc("GET /api/metrics", a.metrics)
	mux.HandleFunc("GET /api/owner", a.owner)
	mux.HandleFunc("GET /api/changes", a.changes)
	mux.HandleFunc("GET /api/slos", a.slos)
	mux.HandleFunc("GET /api/snapshots", a.snapshots)
	mux.HandleFunc("POST /api/snapshots", a.snapshots)
	mux.HandleFunc("GET /api/snapshot/diff", a.snapshotDiff)
	mux.HandleFunc("GET /api/whoami", func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r)
		if u == nil {
			writeJSON(w, map[string]any{"signedIn": false})
			return
		}
		writeJSON(w, map[string]any{"signedIn": true, "email": u.Email, "groups": u.Groups})
	})
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
		if a.Contexts != nil && !a.Contexts.MatchString(n) && n != c.Context {
			continue
		}
		ctxs = append(ctxs, contextInfo{Name: n, Cluster: c.ClusterOf(n), Danger: a.Danger != nil && a.Danger.MatchString(n)})
	}
	ns := a.DefaultNS
	if ns == "" {
		ns = c.Namespace
	}
	if ns == "" {
		ns = "all"
	}
	user := ""
	if u := UserFrom(r); u != nil {
		user = u.Email
	}
	writeJSON(w, map[string]any{"version": a.Version, "readOnly": a.ReadOnly, "contexts": ctxs, "context": c.Context,
		"namespace": ns, "favorites": a.Favorites, "groups": kube.GroupOrder, "ai": a.AI != nil, "user": user})
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
	Trend  []int64  `json:"trend,omitempty"` // CPU millicores over time (pods)
}

func (a *App) target(r *http.Request) (*kube.Client, kube.Resource, string, string, error) {
	q := r.URL.Query()
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
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
	if res.Name == "pods" {
		a.record(c.Context, t.Rows)
	}
	q := r.URL.Query()
	rows := filterRows(t.Rows, q.Get("filter"))
	if col, err := strconv.Atoi(q.Get("sort")); err == nil && col >= 0 && col < len(t.Columns) {
		sortRows(rows, col, q.Get("dir") == "desc")
	}
	total := len(rows)
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 500
	}
	offset = min(max(0, offset), total)
	rows = rows[offset:min(total, offset+limit)]
	out := make([]rowJSON, 0, len(rows))
	for _, row := range rows {
		j := rowJSON{NS: row.Namespace, Name: row.Name, Cells: row.Cells, Health: healthName[row.Health]}
		if res.Name == "pods" {
			for _, s := range a.trend(c.Context, row.Namespace, row.Name) {
				j.Trend = append(j.Trend, s.CPU)
			}
		}
		out = append(out, j)
	}
	writeJSON(w, map[string]any{"columns": t.Columns, "rows": out, "note": t.Note, "total": total, "all": len(t.Rows),
		"offset": offset, "at": time.Now().Format(time.RFC3339)})
}

// filterRows keeps rows whose cells contain every word of the filter.
func filterRows(rows []kube.Row, filter string) []kube.Row {
	words := strings.Fields(strings.ToLower(filter))
	if len(words) == 0 {
		return append([]kube.Row(nil), rows...)
	}
	var out []kube.Row
	for _, r := range rows {
		line := strings.ToLower(r.Namespace + " " + strings.Join(r.Cells, " "))
		ok := true
		for _, w := range words {
			if !strings.Contains(line, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

var leadingNum = regexp.MustCompile(`^(\d+(?:\.\d+)?)`)

// sortRows sorts by a column, numerically when both cells start with a number ("12", "3/4", "250Mi", "5d").
func sortRows(rows []kube.Row, col int, desc bool) {
	cell := func(r kube.Row) string {
		if col < len(r.Cells) {
			return r.Cells[col]
		}
		return ""
	}
	sort.SliceStable(rows, func(i, j int) bool {
		x, y := cell(rows[i]), cell(rows[j])
		mx, my := leadingNum.FindString(x), leadingNum.FindString(y)
		var less bool
		if mx != "" && my != "" {
			fx, _ := strconv.ParseFloat(mx, 64)
			fy, _ := strconv.ParseFloat(my, 64)
			if fx == fy {
				less = x < y
			} else {
				less = fx < fy
			}
		} else {
			less = strings.ToLower(x) < strings.ToLower(y)
		}
		if desc {
			return !less && x != y
		}
		return less
	})
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
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
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
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
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
		fail(w, 403, fmt.Errorf("read-only: start kubeweave serve with --allow-changes to make changes"))
		return
	}
	var req actionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, 400, err)
		return
	}
	c, err := a.scopedFor(r, req.Ctx, req.NS)
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
	_ = result
	a.audit(r, c.Context, req.NS, desc, res.Name+"/"+req.Name, err)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]string{"status": desc + " " + req.Name})
}

// owner says which team owns an object (config rules first, then owner/team labels).
func (a *App) owner(w http.ResponseWriter, r *http.Request) {
	c, res, ns, name, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx, cancel := reqCtx(r, 15*time.Second)
	defer cancel()
	var labels, annos map[string]string
	if obj, err := c.Get(ctx, res, ns, name); err == nil {
		labels, annos = obj.GetLabels(), obj.GetAnnotations()
	}
	o, found := owners.Resolve(a.Owners, ns, labels, annos)
	writeJSON(w, map[string]any{"found": found, "team": o.Team, "contact": o.Contact, "source": o.Source})
}
