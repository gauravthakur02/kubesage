package kube

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

// VolumeInfo is one PersistentVolumeClaim with its usage (when the kubelet stats are readable).
type VolumeInfo struct {
	Namespace, Name string
	Phase           string
	Capacity        int64
	Used            int64 // -1 when unknown
	StorageClass    string
	Access          string
	Volume          string
	MountedBy       []string
	StatsErr        string
}

// UsePercent is used/capacity in percent, or -1.
func (v VolumeInfo) UsePercent() int {
	if v.Used < 0 || v.Capacity <= 0 {
		return -1
	}
	return int(v.Used * 100 / v.Capacity)
}

type kubeletSummary struct {
	Pods []struct {
		Volume []struct {
			PVCRef *struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"pvcRef"`
			UsedBytes     *int64 `json:"usedBytes"`
			CapacityBytes *int64 `json:"capacityBytes"`
		} `json:"volume"`
	} `json:"pods"`
}

var accessShort = map[corev1.PersistentVolumeAccessMode]string{"ReadWriteOnce": "RWO", "ReadOnlyMany": "ROX", "ReadWriteMany": "RWX", "ReadWriteOncePod": "RWOP"}

// Storage lists PVCs with the pods mounting them and, where the kubelet summary API is
// allowed (nodes/proxy), how full each volume is.
func (c *Client) Storage(ctx context.Context) ([]VolumeInfo, error) {
	pvcs, err := c.listPVCsAcross(ctx, c.Namespace)
	if err != nil {
		return nil, err
	}
	pods, _ := c.listPodsAcross(ctx, c.Namespace)
	mounted := map[string][]string{}
	nodes := map[string]bool{}
	for _, p := range pods {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				k := p.Namespace + "/" + v.PersistentVolumeClaim.ClaimName
				mounted[k] = append(mounted[k], p.Name)
				if p.Spec.NodeName != "" && p.Status.Phase == corev1.PodRunning {
					nodes[p.Spec.NodeName] = true
				}
			}
		}
	}
	used := map[string][2]int64{}
	statsErr := ""
	rc := c.Clientset.CoreV1().RESTClient()
	if r, ok := rc.(*rest.RESTClient); ok && r == nil {
		nodes = nil // fake clients in tests have no REST transport
	}
	for n := range nodes {
		raw, err := rc.Get().Resource("nodes").Name(n).SubResource("proxy").Suffix("stats/summary").DoRaw(ctx)
		if err != nil {
			statsErr = firstLine(err.Error())
			continue
		}
		var s kubeletSummary
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		for _, p := range s.Pods {
			for _, v := range p.Volume {
				if v.PVCRef != nil && v.UsedBytes != nil && v.CapacityBytes != nil {
					used[v.PVCRef.Namespace+"/"+v.PVCRef.Name] = [2]int64{*v.UsedBytes, *v.CapacityBytes}
				}
			}
		}
	}
	out := make([]VolumeInfo, 0, len(pvcs))
	for _, p := range pvcs {
		k := p.Namespace + "/" + p.Name
		v := VolumeInfo{Namespace: p.Namespace, Name: p.Name, Phase: string(p.Status.Phase), Used: -1, Volume: p.Spec.VolumeName, MountedBy: mounted[k], StatsErr: statsErr}
		if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
			v.Capacity = q.Value()
		} else if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			v.Capacity = q.Value()
		}
		if u, ok := used[k]; ok {
			v.Used = u[0]
			if u[1] > 0 {
				v.Capacity = u[1]
			}
		}
		if p.Spec.StorageClassName != nil {
			v.StorageClass = *p.Spec.StorageClassName
		}
		modes := []string{}
		for _, m := range p.Spec.AccessModes {
			modes = append(modes, accessShort[m])
		}
		v.Access = strings.Join(modes, ",")
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UsePercent() > out[j].UsePercent() })
	return out, nil
}
