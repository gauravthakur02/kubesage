package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gauravthakur02/kubesage/internal/kube"
)

var unsafeFileName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func attachment(w http.ResponseWriter, contentType string, parts ...string) {
	name := unsafeFileName.ReplaceAllString(strings.Join(nonEmpty(parts), "_"), "-") + "-" + time.Now().Format("20060102-150405")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ext := ".log"
	if contentType == "application/zip" {
		ext = ".zip"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s%s"`, name, ext))
}

// logsDownload sends one container's complete log as a file.
func (a *App) logsDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := a.scopedFor(r, q.Get("ctx"), q.Get("ns"))
	if err != nil {
		fail(w, 400, err)
		return
	}
	ns, pod, ctr := q.Get("ns"), q.Get("pod"), q.Get("container")
	if pod == "" {
		fail(w, 400, fmt.Errorf("pod is required"))
		return
	}
	o := kube.LogOptions{Container: ctr, Previous: q.Get("previous") == "1", Since: kube.ParseSince(q.Get("since"), 0), Timestamps: q.Get("timestamps") == "1"}
	// open the stream first, so an error is still a proper JSON answer
	rc, err := c.Logs(r.Context(), ns, pod, kube.LogOptions{Container: ctr, Previous: o.Previous, Tail: 1})
	if err != nil {
		fail(w, 502, err)
		return
	}
	rc.Close()
	suffix := ""
	if o.Previous {
		suffix = "previous"
	}
	attachment(w, "text/plain; charset=utf-8", "kubesage-logs", c.Context, ns, pod, ctr, suffix)
	_, _ = c.WriteLogs(r.Context(), w, ns, pod, o)
}

// logsBundle sends a zip with the logs of every pod and container of an object.
func (a *App) logsBundle(w http.ResponseWriter, r *http.Request) {
	c, res, ns, name, err := a.target(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	if !res.CanLogs() {
		fail(w, 400, fmt.Errorf("%s have no logs", res.Name))
		return
	}
	pods, err := c.PodsFor(r.Context(), res, ns, name)
	if err != nil || len(pods) == 0 {
		if err == nil {
			err = fmt.Errorf("no pods")
		}
		fail(w, 502, err)
		return
	}
	attachment(w, "application/zip", "kubesage-logs", c.Context, ns, name)
	_, _ = c.WriteLogBundle(r.Context(), w, res, ns, name, kube.ParseSince(r.URL.Query().Get("since"), 0), true)
}

func nonEmpty(parts []string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
