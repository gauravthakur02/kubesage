package kube

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// DeprecatedAPI is a group/version/kind that is deprecated or removed in a Kubernetes release.
type DeprecatedAPI struct {
	GroupVersion, Kind string
	Deprecated         string // minor version, e.g. "1.19"
	Removed            string // minor version, "" if not scheduled
	Replacement        string
}

// DeprecatedAPIs is the built-in table (from the Kubernetes deprecation guide).
var DeprecatedAPIs = []DeprecatedAPI{
	{"extensions/v1beta1", "Ingress", "1.14", "1.22", "networking.k8s.io/v1"},
	{"networking.k8s.io/v1beta1", "Ingress", "1.19", "1.22", "networking.k8s.io/v1"},
	{"networking.k8s.io/v1beta1", "IngressClass", "1.19", "1.22", "networking.k8s.io/v1"},
	{"extensions/v1beta1", "Deployment", "1.9", "1.16", "apps/v1"},
	{"extensions/v1beta1", "DaemonSet", "1.9", "1.16", "apps/v1"},
	{"extensions/v1beta1", "ReplicaSet", "1.9", "1.16", "apps/v1"},
	{"extensions/v1beta1", "NetworkPolicy", "1.9", "1.16", "networking.k8s.io/v1"},
	{"extensions/v1beta1", "PodSecurityPolicy", "1.10", "1.16", "policy/v1beta1"},
	{"apps/v1beta1", "Deployment", "1.9", "1.16", "apps/v1"},
	{"apps/v1beta1", "StatefulSet", "1.9", "1.16", "apps/v1"},
	{"apps/v1beta2", "Deployment", "1.9", "1.16", "apps/v1"},
	{"apps/v1beta2", "StatefulSet", "1.9", "1.16", "apps/v1"},
	{"apps/v1beta2", "DaemonSet", "1.9", "1.16", "apps/v1"},
	{"apps/v1beta2", "ReplicaSet", "1.9", "1.16", "apps/v1"},
	{"apiextensions.k8s.io/v1beta1", "CustomResourceDefinition", "1.16", "1.22", "apiextensions.k8s.io/v1"},
	{"admissionregistration.k8s.io/v1beta1", "MutatingWebhookConfiguration", "1.16", "1.22", "admissionregistration.k8s.io/v1"},
	{"admissionregistration.k8s.io/v1beta1", "ValidatingWebhookConfiguration", "1.16", "1.22", "admissionregistration.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "ClusterRole", "1.17", "1.22", "rbac.authorization.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "ClusterRoleBinding", "1.17", "1.22", "rbac.authorization.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "Role", "1.17", "1.22", "rbac.authorization.k8s.io/v1"},
	{"rbac.authorization.k8s.io/v1beta1", "RoleBinding", "1.17", "1.22", "rbac.authorization.k8s.io/v1"},
	{"scheduling.k8s.io/v1beta1", "PriorityClass", "1.14", "1.22", "scheduling.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "StorageClass", "1.19", "1.22", "storage.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "CSIDriver", "1.19", "1.22", "storage.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "CSINode", "1.17", "1.22", "storage.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "VolumeAttachment", "1.19", "1.22", "storage.k8s.io/v1"},
	{"certificates.k8s.io/v1beta1", "CertificateSigningRequest", "1.19", "1.22", "certificates.k8s.io/v1"},
	{"coordination.k8s.io/v1beta1", "Lease", "1.14", "1.22", "coordination.k8s.io/v1"},
	{"batch/v1beta1", "CronJob", "1.21", "1.25", "batch/v1"},
	{"discovery.k8s.io/v1beta1", "EndpointSlice", "1.21", "1.25", "discovery.k8s.io/v1"},
	{"events.k8s.io/v1beta1", "Event", "1.19", "1.25", "events.k8s.io/v1"},
	{"autoscaling/v2beta1", "HorizontalPodAutoscaler", "1.22", "1.25", "autoscaling/v2"},
	{"autoscaling/v2beta2", "HorizontalPodAutoscaler", "1.23", "1.26", "autoscaling/v2"},
	{"policy/v1beta1", "PodDisruptionBudget", "1.21", "1.25", "policy/v1"},
	{"policy/v1beta1", "PodSecurityPolicy", "1.21", "1.25", "Pod Security Admission (no replacement API)"},
	{"node.k8s.io/v1beta1", "RuntimeClass", "1.20", "1.25", "node.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta1", "FlowSchema", "1.23", "1.26", "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta1", "PriorityLevelConfiguration", "1.23", "1.26", "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta2", "FlowSchema", "1.26", "1.29", "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta2", "PriorityLevelConfiguration", "1.26", "1.29", "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta3", "FlowSchema", "1.29", "1.32", "flowcontrol.apiserver.k8s.io/v1"},
	{"flowcontrol.apiserver.k8s.io/v1beta3", "PriorityLevelConfiguration", "1.29", "1.32", "flowcontrol.apiserver.k8s.io/v1"},
	{"storage.k8s.io/v1beta1", "CSIStorageCapacity", "1.24", "1.27", "storage.k8s.io/v1"},
	{"resource.k8s.io/v1alpha2", "ResourceClaim", "1.31", "1.31", "resource.k8s.io/v1beta1"},
	{"admissionregistration.k8s.io/v1beta1", "ValidatingAdmissionPolicy", "1.30", "1.32", "admissionregistration.k8s.io/v1"},
	{"admissionregistration.k8s.io/v1beta1", "ValidatingAdmissionPolicyBinding", "1.30", "1.32", "admissionregistration.k8s.io/v1"},
}

// Minor parses "1.27" or "v1.27.3" into 27 (major assumed 1).
func Minor(v string) int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	return n
}

func lookupDeprecated(gv, kind string) (DeprecatedAPI, bool) {
	for _, d := range DeprecatedAPIs {
		if d.GroupVersion == gv && d.Kind == kind {
			return d, true
		}
	}
	return DeprecatedAPI{}, false
}

// UpgradeFinding is one object or source using a deprecated or removed API.
type UpgradeFinding struct {
	API      DeprecatedAPI
	Source   string // helm release, last-applied, requested (apiserver metrics)
	Object   string
	Blocking bool // removed in the target version or earlier
}

// NodeVersion is a node's kubelet version.
type NodeVersion struct {
	Name, Kubelet, Pool string
}

// UpgradeReport is the readiness check for moving to Target.
type UpgradeReport struct {
	Server      string
	Target      string
	Findings    []UpgradeFinding
	Nodes       []NodeVersion
	MetricsNote string
	AKS         string // available AKS upgrades (az CLI), or a hint
	Notes       []string
}

var manifestHeader = regexp.MustCompile(`(?m)^---\s*$`)

// scanManifest finds deprecated apiVersion/kind pairs in a multi-document YAML stream.
func scanManifest(manifest string) [][3]string {
	var out [][3]string
	for _, doc := range manifestHeader.Split(manifest, -1) {
		var m struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if yaml.Unmarshal([]byte(doc), &m) != nil || m.Kind == "" {
			continue
		}
		out = append(out, [3]string{m.APIVersion, m.Kind, m.Metadata.Name})
	}
	return out
}

// UpgradeReadiness checks Helm release manifests, last-applied annotations and the API server's
// deprecated-API request metrics against target (e.g. "1.36"; "" = next minor version).
func (c *Client) UpgradeReadiness(ctx context.Context, target string) (*UpgradeReport, error) {
	server, err := c.ServerVersion()
	if err != nil {
		return nil, err
	}
	if target == "" {
		target = fmt.Sprintf("1.%d", Minor(server)+1)
	}
	tgt := Minor(target)
	r := &UpgradeReport{Server: server, Target: target}
	seen := map[string]bool{}
	add := func(d DeprecatedAPI, source, obj string) {
		k := d.GroupVersion + d.Kind + source + obj
		if seen[k] {
			return
		}
		seen[k] = true
		blocking := d.Removed != "" && Minor(d.Removed) <= tgt
		r.Findings = append(r.Findings, UpgradeFinding{API: d, Source: source, Object: obj, Blocking: blocking})
	}
	// 1. Helm releases (what the next `helm upgrade` will send).
	if rels, err := c.HelmReleases(ctx, c.Namespace); err == nil {
		for _, rel := range rels {
			for _, m := range scanManifest(rel.Manifest) {
				if d, ok := lookupDeprecated(m[0], m[1]); ok {
					add(d, "helm "+rel.Namespace+"/"+rel.Name, m[1]+"/"+m[2])
				}
			}
		}
	} else {
		r.Notes = append(r.Notes, "Helm releases not readable: "+firstLine(err.Error()))
	}
	// 2. kubectl last-applied annotations on common kinds.
	for _, name := range []string{"deployments", "statefulsets", "daemonsets", "cronjobs", "ingresses", "horizontalpodautoscalers", "poddisruptionbudgets"} {
		res, ok := Lookup(name)
		if !ok || c.Dynamic == nil {
			continue
		}
		ri := c.Dynamic.Resource(res.GVR)
		l, err := ri.Namespace(c.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, u := range l.Items {
			last := u.GetAnnotations()["kubectl.kubernetes.io/last-applied-configuration"]
			if last == "" {
				continue
			}
			var m struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if json.Unmarshal([]byte(last), &m) == nil {
				if d, ok := lookupDeprecated(m.APIVersion, m.Kind); ok {
					add(d, "last kubectl apply", u.GetNamespace()+"/"+u.GetName())
				}
			}
		}
	}
	// 3. Deprecated APIs clients actually requested since the API server started.
	if c.Clientset != nil {
		if rc := c.Clientset.Discovery().RESTClient(); rc != nil && !isNilRest(rc) {
			raw, err := rc.Get().AbsPath("/metrics").DoRaw(ctx)
			if err != nil {
				r.MetricsNote = "API server metrics not readable (needs get on /metrics): " + firstLine(err.Error())
			} else {
				re := regexp.MustCompile(`^apiserver_requested_deprecated_apis\{(.*)\}`)
				sc := bufio.NewScanner(strings.NewReader(string(raw)))
				sc.Buffer(make([]byte, 1<<20), 1<<20)
				for sc.Scan() {
					m := re.FindStringSubmatch(sc.Text())
					if m == nil {
						continue
					}
					lbl := map[string]string{}
					for _, kv := range strings.Split(m[1], ",") {
						k, v, _ := strings.Cut(kv, "=")
						lbl[k] = strings.Trim(v, `"`)
					}
					gv := lbl["version"]
					if lbl["group"] != "" {
						gv = lbl["group"] + "/" + lbl["version"]
					}
					d := DeprecatedAPI{GroupVersion: gv, Kind: lbl["resource"], Removed: lbl["removed_release"], Replacement: "see the deprecation guide"}
					if known, ok := lookupDeprecatedResource(gv, lbl["resource"]); ok {
						d = known
					}
					add(d, "requested by a client", lbl["resource"])
				}
			}
		}
	}
	// 4. Nodes: kubelet version skew.
	if nodes, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for _, n := range nodes.Items {
			pool := n.Labels["kubernetes.azure.com/agentpool"]
			if pool == "" {
				pool = n.Labels["agentpool"]
			}
			r.Nodes = append(r.Nodes, NodeVersion{Name: n.Name, Kubelet: n.Status.NodeInfo.KubeletVersion, Pool: pool})
		}
		sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].Pool+r.Nodes[i].Name < r.Nodes[j].Pool+r.Nodes[j].Name })
		if len(nodes.Items) > 0 {
			r.AKS = c.aksUpgrades(ctx, nodes.Items[0].Spec.ProviderID)
		}
	}
	sort.SliceStable(r.Findings, func(i, j int) bool { return r.Findings[i].Blocking && !r.Findings[j].Blocking })
	return r, nil
}

func lookupDeprecatedResource(gv, resource string) (DeprecatedAPI, bool) {
	for _, d := range DeprecatedAPIs {
		if d.GroupVersion == gv && strings.EqualFold(d.Kind+"s", resource) || d.GroupVersion == gv && strings.EqualFold(strings.TrimSuffix(d.Kind, "y")+"ies", resource) {
			return d, true
		}
	}
	return DeprecatedAPI{}, false
}

var providerRe = regexp.MustCompile(`(?i)^azure:///subscriptions/([^/]+)/resourceGroups/([^/]+)/`)

// aksUpgrades asks the az CLI (read-only) which versions an AKS cluster can upgrade to. The node's
// providerID gives the subscription and node resource group; az aks list finds the cluster that owns it.
func (c *Client) aksUpgrades(ctx context.Context, providerID string) string {
	m := providerRe.FindStringSubmatch(providerID)
	if m == nil {
		return ""
	}
	sub, nodeRG := m[1], m[2]
	if _, err := exec.LookPath("az"); err != nil {
		return "AKS cluster detected; install the az CLI to list available versions (az aks get-upgrades)"
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "az", "aks", "list", "--subscription", sub, "-o", "json",
		"--query", "[].{name:name, rg:resourceGroup, nodeRG:nodeResourceGroup}").Output()
	if err != nil {
		return "could not list AKS clusters with az (" + firstLine(err.Error()) + "); run az login, then az aks get-upgrades -g <rg> -n <cluster>"
	}
	var clusters []struct{ Name, RG, NodeRG string }
	if json.Unmarshal(out, &clusters) != nil {
		return ""
	}
	var name, rg string
	for _, cl := range clusters {
		if strings.EqualFold(cl.NodeRG, nodeRG) {
			name, rg = cl.Name, cl.RG
		}
	}
	if name == "" {
		return "AKS cluster not found in the subscription with az aks list (missing Azure RBAC?)"
	}
	hint := fmt.Sprintf("az aks get-upgrades -g %s -n %s -o table", rg, name)
	out, err = exec.CommandContext(ctx, "az", "aks", "get-upgrades", "--subscription", sub, "-g", rg, "-n", name, "-o", "json").Output()
	if err != nil {
		return "could not query AKS upgrades (" + firstLine(err.Error()) + "); try: " + hint
	}
	var res struct {
		ControlPlaneProfile struct {
			KubernetesVersion string `json:"kubernetesVersion"`
			Upgrades          []struct {
				KubernetesVersion string `json:"kubernetesVersion"`
				IsPreview         *bool  `json:"isPreview"`
			} `json:"upgrades"`
		} `json:"controlPlaneProfile"`
	}
	if json.Unmarshal(out, &res) != nil {
		return "unexpected az output; try: " + hint
	}
	var vs []string
	for _, u := range res.ControlPlaneProfile.Upgrades {
		v := u.KubernetesVersion
		if u.IsPreview != nil && *u.IsPreview {
			v += " (preview)"
		}
		vs = append(vs, v)
	}
	if len(vs) == 0 {
		return fmt.Sprintf("AKS %s (%s): no upgrades available yet", name, res.ControlPlaneProfile.KubernetesVersion)
	}
	return fmt.Sprintf("AKS %s control plane %s can upgrade to: %s", name, res.ControlPlaneProfile.KubernetesVersion, strings.Join(vs, ", "))
}

// isNilRest reports a typed-nil REST client (fake clientsets in tests).
func isNilRest(rc any) bool {
	r, ok := rc.(*rest.RESTClient)
	return ok && r == nil
}
