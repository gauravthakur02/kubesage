package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Sizing compares a workload's requests with what its pods actually use.
type Sizing struct {
	Namespace, Workload string // Workload is Kind/name
	Pods                int
	CPUReq, CPUPeak     int64 // per pod, millicores
	MemReq, MemPeak     int64 // per pod, bytes
	CPUSuggest          int64
	MemSuggest          int64
	Verdict             string // over, under, ok, no-requests
	WasteCPU, WasteMem  int64  // over-requested across all pods
	LongWindow          bool   // peaks include Prometheus history
}

// PeakFunc returns the highest usage seen for a pod (from kubesage's own history), if known.
type PeakFunc func(ns, pod string) (cpu, mem int64, ok bool)

// WorkloadPeaks maps namespace/workload-name to the highest per-pod [cpu millicores, memory bytes]
// over a long window (from Prometheus), when available.
type WorkloadPeaks map[string][2]int64

func roundUp(v, step int64) int64 {
	if v <= 0 {
		return step
	}
	return ((v + step - 1) / step) * step
}

// RightSizing groups running pods by workload and suggests requests: CPU peak ×1.5 (min 10m),
// memory peak ×1.3 (min 32Mi). Needs metrics-server.
func (c *Client) RightSizing(ctx context.Context, peak PeakFunc, long WorkloadPeaks) ([]Sizing, error) {
	pods, err := c.listPodsAcross(ctx, c.Namespace)
	if err != nil {
		return nil, err
	}
	usage, err := c.PodMetrics(ctx, c.Namespace)
	if err != nil {
		return nil, fmt.Errorf("metrics-server needed for right-sizing: %w", err)
	}
	rsOwner := map[string]string{} // ns/replicaset → Deployment/name
	if rss, err := listAcross(ctx, c, c.Namespace, func(ns string) ([]appsv1.ReplicaSet, error) {
		l, err := c.Clientset.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}); err == nil {
		for _, rs := range rss {
			for _, o := range rs.OwnerReferences {
				if o.Kind == "Deployment" {
					rsOwner[rs.Namespace+"/"+rs.Name] = "Deployment/" + o.Name
				}
			}
		}
	}
	by := map[string]*Sizing{}
	var keys []string
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != "Running" {
			continue
		}
		wl := "Pod/" + p.Name
		if len(p.OwnerReferences) > 0 {
			o := p.OwnerReferences[0]
			wl = o.Kind + "/" + o.Name
			if o.Kind == "ReplicaSet" {
				if d, ok := rsOwner[p.Namespace+"/"+o.Name]; ok {
					wl = d
				} else if h := p.Labels["pod-template-hash"]; h != "" && len(o.Name) > len(h)+1 {
					wl = "Deployment/" + o.Name[:len(o.Name)-len(h)-1]
				}
			}
		}
		key := p.Namespace + "|" + wl
		s := by[key]
		if s == nil {
			s = &Sizing{Namespace: p.Namespace, Workload: wl}
			by[key] = s
			keys = append(keys, key)
		}
		s.Pods++
		cpuReq, _, memReq, _ := podLimits(p)
		s.CPUReq, s.MemReq = max(s.CPUReq, cpuReq), max(s.MemReq, memReq)
		var cpu, mem int64
		for _, u := range usage[p.Namespace+"/"+p.Name] {
			cpu += u.CPUMilli
			mem += u.MemBytes
		}
		if pk := peak; pk != nil {
			if pc, pm, ok := pk(p.Namespace, p.Name); ok {
				cpu, mem = max(cpu, pc), max(mem, pm)
			}
		}
		s.CPUPeak, s.MemPeak = max(s.CPUPeak, cpu), max(s.MemPeak, mem)
	}
	out := make([]Sizing, 0, len(keys))
	for _, k := range keys {
		s := by[k]
		if long != nil {
			_, name, _ := strings.Cut(s.Workload, "/")
			if pk, ok := long[s.Namespace+"/"+name]; ok {
				s.CPUPeak, s.MemPeak = max(s.CPUPeak, pk[0]), max(s.MemPeak, pk[1])
				s.LongWindow = true
			}
		}
		s.CPUSuggest = max(roundUp(s.CPUPeak*3/2, 5), 10)
		s.MemSuggest = max(roundUp(s.MemPeak*13/10, 1<<20), 32<<20)
		switch {
		case s.CPUReq == 0 && s.MemReq == 0:
			s.Verdict = "no-requests"
		case s.CPUPeak > s.CPUReq && s.CPUReq > 0, s.MemPeak > s.MemReq && s.MemReq > 0:
			s.Verdict = "under"
		case (s.CPUReq > 2*s.CPUSuggest && s.CPUReq-s.CPUSuggest >= 100) || (s.MemReq > 2*s.MemSuggest && s.MemReq-s.MemSuggest >= 128<<20):
			s.Verdict = "over"
		default:
			s.Verdict = "ok"
		}
		if s.CPUReq > s.CPUSuggest {
			s.WasteCPU = (s.CPUReq - s.CPUSuggest) * int64(s.Pods)
		}
		if s.MemReq > s.MemSuggest {
			s.WasteMem = (s.MemReq - s.MemSuggest) * int64(s.Pods)
		}
		out = append(out, *s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		wi := out[i].WasteCPU + out[i].WasteMem/(4<<20) // ~1 core ≈ 4Gi
		wj := out[j].WasteCPU + out[j].WasteMem/(4<<20)
		return wi > wj
	})
	return out, nil
}
