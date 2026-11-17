package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/gauravthakur02/kubesage/internal/ai"
	"github.com/gauravthakur02/kubesage/internal/audit"
	"github.com/gauravthakur02/kubesage/internal/diff"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

// AIFunc returns the AI provider allowed for a kube-context (nil error = allowed).
type AIFunc func(kctx string) (ai.Provider, error)

type objReq struct {
	Ctx, NS, Res, Name string
	YAML, Confirm      string
	Messages           []ai.Message
}

func (a *App) decode(w http.ResponseWriter, r *http.Request) (objReq, *kube.Client, kube.Resource, bool) {
	var req objReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		fail(w, 400, err)
		return req, nil, kube.Resource{}, false
	}
	c, err := a.scopedFor(r, req.Ctx, req.NS)
	if err != nil {
		fail(w, 400, err)
		return req, nil, kube.Resource{}, false
	}
	res, ok := a.resource(c.Context, req.Res)
	if !ok && req.Res != "" {
		fail(w, 400, fmt.Errorf("unknown resource %q", req.Res))
		return req, nil, kube.Resource{}, false
	}
	return req, c, res, true
}

func (a *App) audit(r *http.Request, kctx, ns, action, target string, err error) {
	if a.AuditPath == "" {
		return
	}
	res := "ok"
	if err != nil {
		res = "error: " + err.Error()
	}
	_ = audit.Append(a.AuditPath, audit.Entry{Time: time.Now().UTC(), Context: kctx, NS: ns, Action: "web: " + action, Target: target, By: a.who(r), Result: res})
}

// ---------- safe edit ----------

func (a *App) editYAML(w http.ResponseWriter, r *http.Request) {
	c, res, ns, name, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if res.Name == "secrets" {
		fail(w, 403, fmt.Errorf("secrets are not editable in the browser (their values stay masked)"))
		return
	}
	ctx, cancel := reqCtx(r, 20*time.Second)
	defer cancel()
	y, err := c.EditableYAML(ctx, res, ns, name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]string{"yaml": y})
}

func diffOf(before, after string) []string {
	return diff.Lines(strings.Split(before, "\n"), strings.Split(after, "\n"), 3)
}

// dryRun validates edited YAML on the API server (admission webhooks and policies included) and returns the diff.
func (a *App) dryRun(w http.ResponseWriter, r *http.Request) {
	req, c, res, ok := a.decode(w, r)
	if !ok {
		return
	}
	if res.Name == "secrets" {
		fail(w, 403, fmt.Errorf("secrets are not editable in the browser"))
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	cur, err := c.EditableYAML(ctx, res, req.NS, req.Name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	out := map[string]any{"diff": diffOf(cur, req.YAML)}
	result, err := c.UpdateFromYAML(ctx, res, req.NS, req.Name, req.YAML, true)
	out["warnings"] = result.Warnings
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["ok"] = true
	}
	writeJSON(w, out)
}

// applyYAML replaces the object after a passed dry-run. Read-only mode refuses; production-like contexts need the name typed.
func (a *App) applyYAML(w http.ResponseWriter, r *http.Request) {
	if a.ReadOnly {
		fail(w, 403, fmt.Errorf("read-only: start kubesage serve with --allow-changes to make changes"))
		return
	}
	req, c, res, ok := a.decode(w, r)
	if !ok {
		return
	}
	if res.Name == "secrets" {
		fail(w, 403, fmt.Errorf("secrets are not editable in the browser"))
		return
	}
	if a.Danger != nil && a.Danger.MatchString(c.Context) && req.Confirm != req.Name {
		fail(w, 412, fmt.Errorf("%s is production-like: type the object name to confirm", c.Context))
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	_, err := c.UpdateFromYAML(ctx, res, req.NS, req.Name, req.YAML, false)
	a.audit(r, c.Context, req.NS, "edit.apply", res.Name+"/"+req.Name, err)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]string{"status": "applied " + req.Name})
}

// ---------- compare ----------

// compare diffs the YAML of two objects (another name, namespace or cluster).
func (a *App) compare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c1, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	ctx2 := q.Get("ctx2")
	if ctx2 == "" {
		ctx2 = q.Get("ctx")
	}
	c2, err := a.scopedFor(r, ctx2, q.Get("ns2"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	res, ok := a.resource(c1.Context, q.Get("res"))
	if !ok {
		fail(w, 400, fmt.Errorf("unknown resource"))
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	defer cancel()
	y1, err := c1.YAML(ctx, res, q.Get("ns"), q.Get("name"), false)
	if err != nil {
		fail(w, 502, err)
		return
	}
	y2, err := c2.YAML(ctx, res, q.Get("ns2"), q.Get("name2"), false)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, map[string]any{"diff": diffOf(y1, y2)})
}

// ---------- AI ----------

func (a *App) provider(w http.ResponseWriter, kctx string) (ai.Provider, bool) {
	if a.AI == nil {
		fail(w, 501, fmt.Errorf("AI is not configured"))
		return nil, false
	}
	p, err := a.AI(kctx)
	if err != nil {
		fail(w, 403, err)
		return nil, false
	}
	return p, true
}

// stream writes AI output as it arrives (plain text, chunked).
func stream(w http.ResponseWriter, r *http.Request, p ai.Provider, msgs []ai.Message) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	out := make(chan string, 256)
	errc := make(chan error, 1)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	go func() { errc <- p.Stream(ctx, ai.SystemPrompt, msgs, out); close(out) }()
	for s := range out {
		_, _ = io.WriteString(w, s)
		if fl != nil {
			fl.Flush()
		}
	}
	if err := <-errc; err != nil && ctx.Err() == nil {
		fmt.Fprintf(w, "\n\n✗ %v", err)
	}
}

// aiAnalyze explains what is wrong with an object (manifest, events and logs, secrets redacted).
func (a *App) aiAnalyze(w http.ResponseWriter, r *http.Request) {
	req, c, res, ok := a.decode(w, r)
	if !ok {
		return
	}
	p, ok := a.provider(w, c.Context)
	if !ok {
		return
	}
	ctx, cancel := reqCtx(r, time.Minute)
	data, err := c.GatherContext(ctx, res, req.NS, req.Name, true, a.LogLines)
	cancel()
	if err != nil {
		fail(w, 502, err)
		return
	}
	msgs := append([]ai.Message{{Role: "user", Content: ai.AnalyzePrompt(res.Kind, req.NS, req.Name, "Cluster (kube-context): "+c.Context+"\n\n"+data)}}, req.Messages...)
	stream(w, r, p, msgs)
}

// aiChat answers free questions about the namespace, with the detected issues as context.
func (a *App) aiChat(w http.ResponseWriter, r *http.Request) {
	req, c, _, ok := a.decode(w, r)
	if !ok {
		return
	}
	p, ok := a.provider(w, c.Context)
	if !ok {
		return
	}
	if len(req.Messages) == 0 {
		fail(w, 400, fmt.Errorf("no question"))
		return
	}
	ctx, cancel := reqCtx(r, 30*time.Second)
	issues, _ := c.Scan(ctx)
	cancel()
	if len(issues) > 40 {
		issues = issues[:40]
	}
	prelude := fmt.Sprintf("Cluster context: kube-context=%q namespace=%q\nDetected issues:\n%s\n\n", c.Context, c.NamespaceLabel(), kube.FormatIssues(issues))
	msgs := append([]ai.Message(nil), req.Messages...)
	msgs[0].Content = prelude + msgs[0].Content
	stream(w, r, p, msgs)
}

// aiFix asks for the smallest patch, merges it locally, dry-runs it and returns the diff. Nothing is applied here.
func (a *App) aiFix(w http.ResponseWriter, r *http.Request) {
	req, c, res, ok := a.decode(w, r)
	if !ok {
		return
	}
	if res.Name == "secrets" {
		fail(w, 403, fmt.Errorf("secrets are never sent to the AI"))
		return
	}
	p, ok := a.provider(w, c.Context)
	if !ok {
		return
	}
	ctx, cancel := reqCtx(r, 4*time.Minute)
	defer cancel()
	original, err := c.EditableYAML(ctx, res, req.NS, req.Name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	obj, err := c.Get(ctx, res, req.NS, req.Name)
	if err != nil {
		fail(w, 502, err)
		return
	}
	clean := kube.Clean(obj, true, false)
	delete(clean, "status")
	shown, _ := yaml.Marshal(clean)
	problems, _ := c.GatherContext(ctx, res, req.NS, req.Name, true, a.LogLines/2)
	reply, err := ai.Complete(ctx, p, ai.SystemPrompt, ai.FixPrompt(res.Kind, req.NS, req.Name, string(shown), problems))
	if err != nil {
		fail(w, 502, err)
		return
	}
	why := ""
	if i := strings.Index(reply, "Why:"); i >= 0 {
		why = strings.TrimSpace(reply[i:])
	}
	if strings.Contains(strings.ToUpper(reply), "NO CHANGE") {
		writeJSON(w, map[string]any{"noChange": true, "why": reply})
		return
	}
	patch, found := ai.ExtractBlock(reply, "yaml", "yml", "")
	if !found {
		fail(w, 502, fmt.Errorf("the AI did not return a YAML patch"))
		return
	}
	edited, err := kube.MergePatchLocally(schema.GroupVersionKind{Group: res.GVR.Group, Version: res.GVR.Version, Kind: res.Kind}, original, patch)
	if err != nil {
		fail(w, 422, err)
		return
	}
	out := map[string]any{"patch": patch, "why": why, "yaml": edited, "diff": diffOf(original, edited)}
	result, err := c.UpdateFromYAML(ctx, res, req.NS, req.Name, edited, true)
	out["warnings"] = result.Warnings
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["ok"] = true
	}
	writeJSON(w, out)
}

// ---------- port-forward (proxied through kubesage serve) ----------

type webForward struct {
	ID    int    `json:"id"`
	Desc  string `json:"desc"`
	Local int    `json:"local"`
	Path  string `json:"path"`
	fw    *kube.Forward
	proxy *httputil.ReverseProxy
}

var fwMu sync.Mutex

func (a *App) forwardStart(w http.ResponseWriter, r *http.Request) {
	if a.ReadOnly {
		fail(w, 403, fmt.Errorf("read-only: port-forwards need --allow-changes"))
		return
	}
	req, c, res, ok := a.decode(w, r)
	if !ok {
		return
	}
	ctx, cancel := reqCtx(r, 20*time.Second)
	defer cancel()
	ts, err := c.PortTargets(ctx, res, req.NS, req.Name)
	if err != nil || len(ts) == 0 {
		if err == nil {
			err = fmt.Errorf("no ports to forward")
		}
		fail(w, 502, err)
		return
	}
	t := ts[0]
	local := kube.SuggestLocalPort(20000 + t.Port%10000)
	fw, err := c.PortForward(t, local, t.Port)
	a.audit(r, c.Context, req.NS, fmt.Sprintf("port-forward %s:%d", t.ObjectDesc, t.Port), res.Name+"/"+req.Name, err)
	if err != nil {
		fail(w, 502, err)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", local))
	fwMu.Lock()
	a.nextFwd++
	id := a.nextFwd
	wf := &webForward{ID: id, Desc: t.Label(), Local: local, Path: fmt.Sprintf("/fwd/%d/", id), fw: fw, proxy: httputil.NewSingleHostReverseProxy(target)}
	if a.forwards == nil {
		a.forwards = map[int]*webForward{}
	}
	a.forwards[id] = wf
	fwMu.Unlock()
	writeJSON(w, wf)
}

func (a *App) forwardList(w http.ResponseWriter, r *http.Request) {
	fwMu.Lock()
	defer fwMu.Unlock()
	out := []*webForward{}
	for _, f := range a.forwards {
		out = append(out, f)
	}
	writeJSON(w, out)
}

func (a *App) forwardStop(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	fwMu.Lock()
	f := a.forwards[id]
	delete(a.forwards, id)
	fwMu.Unlock()
	if f == nil {
		fail(w, 404, fmt.Errorf("no such port-forward"))
		return
	}
	f.fw.Stop()
	writeJSON(w, map[string]string{"status": "stopped " + f.Desc})
}

var fwdPath = regexp.MustCompile(`^/fwd/(\d+)(/.*)?$`)

// ForwardProxy serves /fwd/<id>/… by proxying to the local end of a port-forward.
func (a *App) ForwardProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := fwdPath.FindStringSubmatch(r.URL.Path)
		if m == nil {
			http.NotFound(w, r)
			return
		}
		id, _ := strconv.Atoi(m[1])
		fwMu.Lock()
		f := a.forwards[id]
		fwMu.Unlock()
		if f == nil {
			http.Error(w, "port-forward stopped", http.StatusGone)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = m[2]
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		q := r2.URL.Query()
		q.Del("t")
		r2.URL.RawQuery = q.Encode()
		f.proxy.ServeHTTP(w, r2)
	})
}

// StopForwards ends every port-forward (server shutdown).
func (a *App) StopForwards() {
	fwMu.Lock()
	defer fwMu.Unlock()
	for id, f := range a.forwards {
		f.fw.Stop()
		delete(a.forwards, id)
	}
}

// ---------- metrics ----------

type sample struct {
	T   int64 `json:"t"`
	CPU int64 `json:"cpu"` // millicores
	Mem int64 `json:"mem"` // bytes
}

const maxSamples = 120

// record keeps recent CPU/memory samples per pod (fed by the pod list the browser refreshes).
func (a *App) record(kctx string, rows []kube.Row) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.samples == nil {
		a.samples = map[string][]sample{}
	}
	now := time.Now().Unix()
	for _, row := range rows {
		if row.Usage == nil {
			continue
		}
		k := kctx + "/" + row.Namespace + "/" + row.Name
		s := a.samples[k]
		if n := len(s); n > 0 && now-s[n-1].T < 10 {
			continue
		}
		s = append(s, sample{T: now, CPU: row.Usage.CPUMilli, Mem: row.Usage.MemBytes})
		if len(s) > maxSamples {
			s = s[len(s)-maxSamples:]
		}
		a.samples[k] = s
	}
}

func (a *App) trend(kctx, ns, pod string) []sample {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sample(nil), a.samples[kctx+"/"+ns+"/"+pod]...)
}

// metrics returns CPU/memory over time for a pod: the last hour from Prometheus when configured, else what this
// server sampled while the page was open.
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	ns, pod := q.Get("ns"), q.Get("pod")
	out := map[string]any{"source": "kubesage (sampled while open)", "samples": a.trend(c.Context, ns, pod)}
	if a.PromURL != nil {
		if u := a.PromURL(c.Context); u != "" {
			ctx, cancel := reqCtx(r, 20*time.Second)
			defer cancel()
			if p, err := c.NewProm(ctx, u, a.PromToken); err == nil {
				sel := fmt.Sprintf(`namespace=%q,pod=%q,container!="",container!="POD"`, ns, pod)
				cpu, err1 := p.QueryRange(ctx, `sum(rate(container_cpu_usage_seconds_total{`+sel+`}[5m]))*1000`, time.Hour, time.Minute)
				mem, err2 := p.QueryRange(ctx, `sum(container_memory_working_set_bytes{`+sel+`})`, time.Hour, time.Minute)
				if err1 == nil && err2 == nil && len(cpu) > 0 && len(mem) > 0 {
					var s []sample
					start := time.Now().Add(-time.Hour).Unix()
					for i := range cpu[0].Values {
						if i < len(mem[0].Values) {
							s = append(s, sample{T: start + int64(i)*60, CPU: int64(cpu[0].Values[i]), Mem: int64(mem[0].Values[i])})
						}
					}
					out["samples"], out["source"] = s, "Prometheus (last hour)"
				}
			}
		}
	}
	writeJSON(w, out)
}
