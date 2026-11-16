package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Prom queries a Prometheus-compatible API, directly or through the API server's service proxy.
type Prom struct {
	URL          string // http(s)://host:port, when direct
	SvcNS, Svc   string // service proxy target
	SvcPort      string
	TokenCommand string
	kube         *Client
}

// Desc describes where the queries go.
func (p *Prom) Desc() string {
	if p.URL != "" {
		return p.URL
	}
	return fmt.Sprintf("service %s/%s:%s (API server proxy)", p.SvcNS, p.Svc, p.SvcPort)
}

var promCandidates = []struct{ ns, svc, port string }{
	{"monitoring", "prometheus-operated", "9090"}, {"monitoring", "kube-prometheus-stack-prometheus", "9090"},
	{"monitoring", "prometheus-server", "80"}, {"monitoring", "prometheus", "9090"},
	{"prometheus", "prometheus-server", "80"}, {"prometheus", "prometheus-operated", "9090"},
	{"observability", "prometheus-operated", "9090"}, {"kube-system", "prometheus", "9090"},
}

// NewProm builds a client from a configured value ("http://…" or "service:<ns>/<svc>:<port>"),
// or finds a well-known in-cluster Prometheus service when configured is empty.
func (c *Client) NewProm(ctx context.Context, configured, tokenCmd string) (*Prom, error) {
	p := &Prom{TokenCommand: tokenCmd, kube: c}
	switch {
	case strings.HasPrefix(configured, "service:"):
		rest := strings.TrimPrefix(configured, "service:")
		nsSvc, port, _ := strings.Cut(rest, ":")
		ns, svc, ok := strings.Cut(nsSvc, "/")
		if !ok || port == "" {
			return nil, fmt.Errorf("prometheus: use service:<namespace>/<service>:<port>")
		}
		p.SvcNS, p.Svc, p.SvcPort = ns, svc, port
		return p, nil
	case configured != "":
		p.URL = strings.TrimRight(configured, "/")
		return p, nil
	}
	for _, cand := range promCandidates {
		if _, err := c.Clientset.CoreV1().Services(cand.ns).Get(ctx, cand.svc, metav1.GetOptions{}); err == nil {
			p.SvcNS, p.Svc, p.SvcPort = cand.ns, cand.svc, cand.port
			return p, nil
		}
	}
	return nil, fmt.Errorf("no Prometheus configured or found (set prometheus.urls in the config)")
}

// PromSample is one series of an instant or range query.
type PromSample struct {
	Labels map[string]string
	Value  float64
	Values []float64 // range queries
}

func (p *Prom) get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	if p.URL == "" {
		pp := map[string]string{}
		for k, v := range params {
			pp[k] = v[0]
		}
		return p.kube.Clientset.CoreV1().Services(p.SvcNS).ProxyGet("http", p.Svc, p.SvcPort, path, pp).DoRaw(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/"+path+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if p.TokenCommand != "" {
		fields := strings.Fields(p.TokenCommand)
		out, err := exec.CommandContext(ctx, fields[0], fields[1:]...).Output()
		if err != nil {
			return nil, fmt.Errorf("bearer_token_command: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(out)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus: %s: %s", resp.Status, firstLine(string(b)))
	}
	return b, err
}

// Query runs an instant query.
func (p *Prom) Query(ctx context.Context, q string) ([]PromSample, error) {
	return p.query(ctx, "api/v1/query", url.Values{"query": {q}})
}

// QueryRange runs a range query ending now.
func (p *Prom) QueryRange(ctx context.Context, q string, window, step time.Duration) ([]PromSample, error) {
	now := time.Now()
	return p.query(ctx, "api/v1/query_range", url.Values{"query": {q}, "start": {strconv.FormatInt(now.Add(-window).Unix(), 10)},
		"end": {strconv.FormatInt(now.Unix(), 10)}, "step": {strconv.Itoa(int(step.Seconds()))}})
}

func (p *Prom) query(ctx context.Context, path string, params url.Values) ([]PromSample, error) {
	b, err := p.get(ctx, path, params)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("prometheus: unexpected answer: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", r.Error)
	}
	num := func(v []any) float64 {
		if len(v) == 2 {
			s, _ := v[1].(string)
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}
		return 0
	}
	out := make([]PromSample, 0, len(r.Data.Result))
	for _, x := range r.Data.Result {
		s := PromSample{Labels: x.Metric, Value: num(x.Value)}
		for _, v := range x.Values {
			s.Values = append(s.Values, num(v))
		}
		if len(s.Values) > 0 {
			s.Value = s.Values[len(s.Values)-1]
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out, nil
}

var podSuffix = regexp.MustCompile(`-[a-z0-9]{5}$`)
var rsHash = regexp.MustCompile(`-[a-z0-9]{6,10}$`)
var ordinal = regexp.MustCompile(`-[0-9]+$`)

// WorkloadOfPod guesses the workload name from a pod name (deployment-hash-xxxxx, sts-0, ds-xxxxx).
func WorkloadOfPod(pod string) []string {
	var out []string
	if podSuffix.MatchString(pod) {
		base := podSuffix.ReplaceAllString(pod, "")
		out = append(out, base) // DaemonSet / Job
		if rsHash.MatchString(base) {
			out = append(out, rsHash.ReplaceAllString(base, "")) // Deployment
		}
	}
	if ordinal.MatchString(pod) {
		out = append(out, ordinal.ReplaceAllString(pod, "")) // StatefulSet
	}
	return out
}

// WorkloadPeaks returns the highest CPU (millicores) and memory (bytes) per pod over window,
// keyed by namespace/workload-name guesses (pods come and go, workloads stay).
func (p *Prom) WorkloadPeaks(ctx context.Context, ns, window string) (map[string][2]int64, error) {
	sel := `container!="",container!="POD"`
	if ns != "" {
		sel += `,namespace="` + ns + `"`
	}
	cpuQ := fmt.Sprintf(`max_over_time(sum by (namespace,pod) (rate(container_cpu_usage_seconds_total{%s}[5m]))[%s:5m])`, sel, window)
	memQ := fmt.Sprintf(`max_over_time(sum by (namespace,pod) (container_memory_working_set_bytes{%s})[%s:5m])`, sel, window)
	cpu, err := p.Query(ctx, cpuQ)
	if err != nil {
		return nil, err
	}
	mem, err := p.Query(ctx, memQ)
	if err != nil {
		return nil, err
	}
	out := map[string][2]int64{}
	put := func(s PromSample, idx int, v int64) {
		for _, wl := range WorkloadOfPod(s.Labels["pod"]) {
			k := s.Labels["namespace"] + "/" + wl
			cur := out[k]
			if v > cur[idx] {
				cur[idx] = v
			}
			out[k] = cur
		}
	}
	for _, s := range cpu {
		put(s, 0, int64(s.Value*1000))
	}
	for _, s := range mem {
		put(s, 1, int64(s.Value))
	}
	return out, nil
}
