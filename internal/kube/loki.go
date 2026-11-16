package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LogLine is one line returned by Loki.
type LogLine struct {
	Time   time.Time
	Labels map[string]string
	Line   string
}

var lokiCandidates = [][3]string{ // namespace, service, port
	{"loki", "loki-gateway", "80"}, {"loki", "loki", "3100"}, {"loki", "loki-read", "3100"},
	{"monitoring", "loki-gateway", "80"}, {"monitoring", "loki", "3100"}, {"logging", "loki", "3100"}, {"observability", "loki", "3100"},
}

// LokiQuery runs a LogQL query over a time window. configured is an http(s) URL, "service:<ns>/<svc>:<port>",
// or empty to look for common Loki services through the API server proxy.
func (c *Client) LokiQuery(ctx context.Context, configured, query string, since time.Duration, limit int) ([]LogLine, string, error) {
	params := url.Values{"query": {query}, "limit": {strconv.Itoa(limit)}, "direction": {"backward"},
		"start": {strconv.FormatInt(time.Now().Add(-since).UnixNano(), 10)}, "end": {strconv.FormatInt(time.Now().UnixNano(), 10)}}
	const path = "loki/api/v1/query_range"
	viaProxy := func(ns, svc, port string) ([]byte, error) {
		pp := map[string]string{}
		for k, v := range params {
			pp[k] = v[0]
		}
		return c.Clientset.CoreV1().Services(ns).ProxyGet("http", svc, port, path, pp).DoRaw(ctx)
	}
	var raw []byte
	var source string
	var err error
	switch {
	case strings.HasPrefix(configured, "service:"):
		nsSvc, port, _ := strings.Cut(strings.TrimPrefix(configured, "service:"), ":")
		ns, svc, _ := strings.Cut(nsSvc, "/")
		source = configured
		raw, err = viaProxy(ns, svc, port)
	case configured != "":
		source = configured
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(configured, "/")+"/"+path+"?"+params.Encode(), nil)
		if err == nil {
			var resp *http.Response
			if resp, err = http.DefaultClient.Do(req); err == nil {
				defer resp.Body.Close()
				raw, err = io.ReadAll(io.LimitReader(resp.Body, 50<<20))
				if err == nil && resp.StatusCode >= 300 {
					err = fmt.Errorf("loki: %s: %s", resp.Status, firstLine(string(raw)))
				}
			}
		}
	default:
		if isNilRest(c.Clientset.CoreV1().RESTClient()) {
			return nil, "", fmt.Errorf("no Loki found")
		}
		err = fmt.Errorf("no Loki found in namespaces loki, monitoring, logging, observability (set loki.url in the config)")
		for _, cand := range lokiCandidates {
			if b, e := viaProxy(cand[0], cand[1], cand[2]); e == nil {
				raw, err, source = b, nil, fmt.Sprintf("service:%s/%s:%s", cand[0], cand[1], cand[2])
				break
			}
		}
	}
	if err != nil {
		return nil, source, err
	}
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, source, fmt.Errorf("loki: unexpected answer: %s", firstLine(string(raw)))
	}
	var out []LogLine
	for _, r := range res.Data.Result {
		for _, v := range r.Values {
			ns, _ := strconv.ParseInt(v[0], 10, 64)
			out = append(out, LogLine{Time: time.Unix(0, ns), Labels: r.Stream, Line: v[1]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, source, nil
}

// LokiSelector is the stream selector for a pod or workload, e.g. {namespace="shop", pod=~"web-.*"}.
func LokiSelector(ns, pod string) string {
	if pod == "" {
		return fmt.Sprintf(`{namespace=%q}`, ns)
	}
	return fmt.Sprintf(`{namespace=%q, pod=~%q}`, ns, pod)
}
