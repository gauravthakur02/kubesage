package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NodePool groups nodes by their cloud node pool (AKS agent pool, EKS node group, GKE node pool, Karpenter NodePool).
type NodePool struct {
	Name, Cloud, Cluster, Group string // Cluster/Group: what the cloud CLI needs (AKS: cluster + resource group)
	Nodes, Ready                int
	Spot                        bool
	InstanceTypes, Zones        []string
	Versions                    []string
	Sub                         string
}

var poolLabels = []struct{ label, cloud string }{
	{"kubernetes.azure.com/agentpool", "AKS"}, {"agentpool", "AKS"}, {"eks.amazonaws.com/nodegroup", "EKS"},
	{"alpha.eksctl.io/nodegroup-name", "EKS"}, {"cloud.google.com/gke-nodepool", "GKE"}, {"karpenter.sh/nodepool", "Karpenter"},
}

func isSpot(l map[string]string) bool {
	return l["kubernetes.azure.com/scalesetpriority"] == "spot" || l["eks.amazonaws.com/capacityType"] == "SPOT" ||
		l["cloud.google.com/gke-spot"] == "true" || l["cloud.google.com/gke-preemptible"] == "true" || l["karpenter.sh/capacity-type"] == "spot"
}

func addUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// NodePools lists the pools of the cluster from node labels (needs permission to list nodes).
func (c *Client) NodePools(ctx context.Context) ([]NodePool, error) {
	nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pools := map[string]*NodePool{}
	for _, n := range nodes.Items {
		l := n.Labels
		name, cloud := "(no pool label)", ""
		for _, pl := range poolLabels {
			if v := l[pl.label]; v != "" {
				name, cloud = v, pl.cloud
				break
			}
		}
		key := cloud + "/" + name
		p := pools[key]
		if p == nil {
			p = &NodePool{Name: name, Cloud: cloud, Cluster: l["alpha.eksctl.io/cluster-name"]}
			if m := providerRe.FindStringSubmatch(n.Spec.ProviderID); m != nil {
				p.Sub, p.Group = m[1], m[2] // node resource group; the cluster is looked up with az when scaling
			}
			pools[key] = p
		}
		p.Nodes++
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				p.Ready++
			}
		}
		p.Spot = p.Spot || isSpot(l)
		p.InstanceTypes = addUnique(p.InstanceTypes, l["node.kubernetes.io/instance-type"])
		p.Zones = addUnique(p.Zones, l["topology.kubernetes.io/zone"])
		p.Versions = addUnique(p.Versions, n.Status.NodeInfo.KubeletVersion)
	}
	out := make([]NodePool, 0, len(pools))
	for _, p := range pools {
		sort.Strings(p.Zones)
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SpotEvictions counts recent eviction/preemption events per node pool.
func (c *Client) SpotEvictions(ctx context.Context) map[string]int {
	out := map[string]int{}
	evs, err := c.Clientset.CoreV1().Events("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, e := range evs.Items {
		r := strings.ToLower(e.Reason + " " + e.Message)
		if strings.Contains(r, "preempt") || strings.Contains(r, "spot") && strings.Contains(r, "interrupt") || strings.Contains(r, "scheduledevent") {
			out[e.InvolvedObject.Name]++
		}
	}
	return out
}

// ScalePoolCommand is the cloud CLI command that scales a pool to n nodes (empty when not supported).
func (c *Client) ScalePoolCommand(ctx context.Context, p NodePool, n int) ([]string, error) {
	switch p.Cloud {
	case "AKS":
		if p.Group == "" {
			return nil, fmt.Errorf("cannot find the AKS cluster from the node provider IDs")
		}
		if _, err := exec.LookPath("az"); err != nil {
			return nil, fmt.Errorf("install the az CLI to scale AKS node pools")
		}
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "az", "aks", "list", "--subscription", p.Sub, "-o", "json",
			"--query", "[].{name:name, rg:resourceGroup, nodeRG:nodeResourceGroup}").Output()
		if err != nil {
			return nil, fmt.Errorf("az aks list: %v (run az login)", firstLine(err.Error()))
		}
		var cl []struct{ Name, RG, NodeRG string }
		_ = json.Unmarshal(out, &cl)
		for _, x := range cl {
			if strings.EqualFold(x.NodeRG, p.Group) {
				return []string{"az", "aks", "nodepool", "scale", "--subscription", p.Sub, "-g", x.RG, "--cluster-name", x.Name,
					"-n", p.Name, "--node-count", fmt.Sprint(n), "--no-wait"}, nil
			}
		}
		return nil, fmt.Errorf("AKS cluster not found with az aks list")
	case "EKS":
		if p.Cluster == "" {
			return nil, fmt.Errorf("EKS cluster name unknown (no alpha.eksctl.io/cluster-name label)")
		}
		return []string{"aws", "eks", "update-nodegroup-config", "--cluster-name", p.Cluster, "--nodegroup-name", p.Name,
			"--scaling-config", fmt.Sprintf("desiredSize=%d", n)}, nil
	case "Karpenter":
		return nil, fmt.Errorf("Karpenter scales on demand; change the NodePool limits instead (:karpenter)")
	}
	return nil, fmt.Errorf("scaling %s pools is not supported yet; use the cloud console or CLI", nonEmptyStr(p.Cloud, "unknown"))
}
