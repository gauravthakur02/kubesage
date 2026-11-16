package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ---------- secrets health ----------

// SecretHealth is a Secret, ExternalSecret or SealedSecret with its sync and rotation state.
type SecretHealth struct {
	Kind, Namespace, Name, Type string
	Changed                     time.Time
	UsedBy                      int
	Status                      string
	H                           Health
}

var (
	esGVRs = []schema.GroupVersionResource{{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"},
		{Group: "external-secrets.io", Version: "v1beta1", Resource: "externalsecrets"}}
	sealedGVR = schema.GroupVersionResource{Group: "bitnami.com", Version: "v1alpha1", Resource: "sealedsecrets"}
)

func (c *Client) listDyn(ctx context.Context, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	return listAcross(ctx, c, c.Namespace, func(n string) ([]unstructured.Unstructured, error) {
		l, err := c.Dynamic.Resource(gvr).Namespace(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
}

// SecretsHealth lists Secrets (age since last change, pods using them), ExternalSecrets and SealedSecrets.
// Secret values are never read into the result.
func (c *Client) SecretsHealth(ctx context.Context, rotationDays int) ([]SecretHealth, error) {
	if rotationDays <= 0 {
		rotationDays = 90
	}
	var out []SecretHealth
	used := map[string]int{}
	if pods, err := c.listPodsAcross(ctx, c.Namespace); err == nil {
		for i := range pods {
			for _, u := range podConfigUses(&pods[i].Spec) {
				if u.kind == "Secret" {
					used[pods[i].Namespace+"/"+u.name]++
				}
			}
			for _, s := range pods[i].Spec.ImagePullSecrets {
				used[pods[i].Namespace+"/"+s.Name]++
			}
		}
	}
	managed := map[string]string{} // ns/secret → ExternalSecret/SealedSecret managing it
	stores := c.secretStoreProviders(ctx)
	if c.Dynamic != nil {
		for _, gvr := range esGVRs {
			items, err := c.listDyn(ctx, gvr)
			if err != nil && len(items) == 0 {
				continue
			}
			for _, u := range items {
				target, _, _ := unstructured.NestedString(u.Object, "spec", "target", "name")
				target = nonEmptyStr(target, u.GetName())
				managed[u.GetNamespace()+"/"+target] = "ExternalSecret/" + u.GetName()
				st, reason, msg := readyCondition(u.Object)
				refresh, _, _ := unstructured.NestedString(u.Object, "status", "refreshTime")
				t, _ := time.Parse(time.RFC3339, refresh)
				store, _, _ := unstructured.NestedString(u.Object, "spec", "secretStoreRef", "name")
				storeKind, _, _ := unstructured.NestedString(u.Object, "spec", "secretStoreRef", "kind")
				src := stores[storeKey(storeKind, u.GetNamespace(), store)]
				if keys := remoteKeys(u.Object); keys != "" {
					src = strings.TrimSpace(src + " " + keys)
				}
				h := HealthOK
				status := "synced from " + store
				if src != "" {
					status += " (" + src + ")"
				}
				if st != "True" {
					h, status = HealthErr, "not synced: "+nonEmptyStr(reason, firstLine(msg))
				}
				out = append(out, SecretHealth{Kind: "ExternalSecret", Namespace: u.GetNamespace(), Name: u.GetName(), Type: "→ " + target, Changed: t, Status: status, H: h})
			}
			break
		}
		if items, err := c.listDyn(ctx, sealedGVR); err == nil || len(items) > 0 {
			for _, u := range items {
				managed[u.GetNamespace()+"/"+u.GetName()] = "SealedSecret/" + u.GetName()
				st, reason, msg := "True", "", ""
				conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
				for _, x := range conds {
					m, _ := x.(map[string]any)
					if m["type"] == "Synced" {
						st, reason, msg = fmt.Sprint(m["status"]), fmt.Sprint(m["reason"]), fmt.Sprint(m["message"])
					}
				}
				h, status := HealthOK, "unsealed"
				if st != "True" {
					h, status = HealthErr, "cannot unseal: "+nonEmptyStr(firstLine(msg), reason)
				}
				out = append(out, SecretHealth{Kind: "SealedSecret", Namespace: u.GetNamespace(), Name: u.GetName(), Changed: u.GetCreationTimestamp().Time, Status: status, H: h})
			}
		}
	}
	if c.Dynamic != nil { // Secrets Store CSI driver: secrets mounted straight from Key Vault, Vault, AWS or GCP
		if items, err := c.listDyn(ctx, spcGVR); err == nil {
			for _, u := range items {
				prov, _, _ := unstructured.NestedString(u.Object, "spec", "provider")
				params, _, _ := unstructured.NestedStringMap(u.Object, "spec", "parameters")
				where := nonEmptyStr(params["keyvaultName"], nonEmptyStr(params["vaultAddress"], params["roleName"]))
				status := "mounted by CSI from " + prov
				if where != "" {
					status += ": " + where
				}
				out = append(out, SecretHealth{Kind: "SecretProviderClass", Namespace: u.GetNamespace(), Name: u.GetName(), Type: prov,
					Changed: u.GetCreationTimestamp().Time, Status: status, H: HealthOK})
			}
		}
	}
	secrets, err := listAcross(ctx, c, c.Namespace, func(n string) ([]corev1.Secret, error) {
		l, err := c.Clientset.CoreV1().Secrets(n).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	})
	if err != nil && len(out) == 0 {
		return nil, err
	}
	for _, s := range secrets {
		if s.Type == corev1.SecretTypeServiceAccountToken || s.Type == "helm.sh/release.v1" {
			continue
		}
		sh := SecretHealth{Kind: "Secret", Namespace: s.Namespace, Name: s.Name, Type: string(s.Type), Changed: lastChange(s.ObjectMeta),
			UsedBy: used[s.Namespace+"/"+s.Name], H: HealthOK, Status: "ok"}
		age := time.Since(sh.Changed)
		switch {
		case managed[s.Namespace+"/"+s.Name] != "":
			sh.Status = "managed by " + managed[s.Namespace+"/"+s.Name]
		case s.Type == corev1.SecretTypeTLS || s.Annotations["cert-manager.io/certificate-name"] != "":
			sh.Status = "TLS (see :certs for expiry)"
		case age > time.Duration(rotationDays)*24*time.Hour:
			sh.H, sh.Status = HealthWarn, fmt.Sprintf("not changed for %d days (rotation policy %d)", int(age.Hours()/24), rotationDays)
		}
		if sh.UsedBy == 0 && sh.Status == "ok" {
			sh.H, sh.Status = HealthNeutral, "not used by any running pod"
		}
		out = append(out, sh)
	}
	sort.SliceStable(out, func(i, j int) bool {
		rank := map[Health]int{HealthErr: 0, HealthWarn: 1, HealthNeutral: 2, HealthOK: 3}
		return rank[out[i].H] < rank[out[j].H]
	})
	return out, nil
}

var (
	spcGVR      = schema.GroupVersionResource{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Resource: "secretproviderclasses"}
	storeGVRs   = []schema.GroupVersionResource{{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}, {Group: "external-secrets.io", Version: "v1beta1", Resource: "secretstores"}}
	cstoreGVRs  = []schema.GroupVersionResource{{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}, {Group: "external-secrets.io", Version: "v1beta1", Resource: "clustersecretstores"}}
	providerTag = map[string]string{"azurekv": "Azure Key Vault", "vault": "HashiCorp Vault", "aws": "AWS", "gcpsm": "GCP Secret Manager",
		"kubernetes": "Kubernetes", "onepassword": "1Password", "doppler": "Doppler", "akeyless": "Akeyless", "gitlab": "GitLab", "ibm": "IBM"}
)

func storeKey(kind, ns, name string) string {
	if kind == "ClusterSecretStore" {
		return "cluster/" + name
	}
	return ns + "/" + name
}

// secretStoreProviders maps External Secrets stores to a readable source, e.g. "Azure Key Vault https://kv.vault.azure.net".
func (c *Client) secretStoreProviders(ctx context.Context) map[string]string {
	out := map[string]string{}
	if c.Dynamic == nil {
		return out
	}
	describe := func(u unstructured.Unstructured) string {
		prov, _, _ := unstructured.NestedMap(u.Object, "spec", "provider")
		for k, v := range prov {
			name := nonEmptyStr(providerTag[k], k)
			if m, ok := v.(map[string]any); ok {
				for _, f := range []string{"vaultUrl", "server", "region", "projectID"} {
					if s, ok := m[f].(string); ok && s != "" {
						return name + " " + s
					}
				}
			}
			return name
		}
		return ""
	}
	for _, set := range [][]schema.GroupVersionResource{storeGVRs, cstoreGVRs} {
		for _, gvr := range set {
			items, err := c.listDyn(ctx, gvr)
			if err != nil && len(items) == 0 {
				continue
			}
			for _, u := range items {
				kind := "SecretStore"
				if u.GetNamespace() == "" {
					kind = "ClusterSecretStore"
				}
				out[storeKey(kind, u.GetNamespace(), u.GetName())] = describe(u)
			}
			break
		}
	}
	return out
}

// remoteKeys lists the remote keys an ExternalSecret reads (at most three).
func remoteKeys(obj map[string]any) string {
	var keys []string
	data, _, _ := unstructured.NestedSlice(obj, "spec", "data")
	for _, d := range data {
		if m, ok := d.(map[string]any); ok {
			if rr, ok := m["remoteRef"].(map[string]any); ok {
				if k, ok := rr["key"].(string); ok {
					keys = append(keys, k)
				}
			}
		}
	}
	if len(keys) == 0 {
		return ""
	}
	if len(keys) > 3 {
		keys = append(keys[:3], fmt.Sprintf("+%d", len(keys)-3))
	}
	return "keys " + strings.Join(keys, ", ")
}

// ---------- service mesh ----------

// MeshNamespace is the sidecar injection state of a namespace.
type MeshNamespace struct {
	Namespace, Mesh, Injection  string
	Pods, WithSidecar, NotReady int
	H                           Health
}

// SidecarOf reports the mesh proxy of a pod ("istio", "linkerd" or "") and whether it is ready.
func SidecarOf(p *corev1.Pod) (string, bool) {
	names := map[string]string{"istio-proxy": "istio", "linkerd-proxy": "linkerd"}
	for _, ct := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		if mesh, ok := names[ct.Name]; ok {
			ready := false
			for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
				if s.Name == ct.Name {
					ready = s.Ready || (s.State.Running != nil && s.Started != nil && *s.Started)
				}
			}
			return mesh, ready
		}
	}
	return "", false
}

// MeshStatus summarises Istio/Linkerd injection per namespace.
func (c *Client) MeshStatus(ctx context.Context) ([]MeshNamespace, error) {
	pods, err := c.listPodsAcross(ctx, c.Namespace)
	if err != nil {
		return nil, err
	}
	labels := map[string]map[string]string{}
	annots := map[string]map[string]string{}
	if nss, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err == nil {
		for _, n := range nss.Items {
			labels[n.Name], annots[n.Name] = n.Labels, n.Annotations
		}
	}
	by := map[string]*MeshNamespace{}
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		m := by[p.Namespace]
		if m == nil {
			m = &MeshNamespace{Namespace: p.Namespace}
			l, an := labels[p.Namespace], annots[p.Namespace]
			switch {
			case l["istio-injection"] != "":
				m.Mesh, m.Injection = "istio", "istio-injection="+l["istio-injection"]
			case l["istio.io/rev"] != "":
				m.Mesh, m.Injection = "istio", "istio.io/rev="+l["istio.io/rev"]
			case l["istio.io/dataplane-mode"] != "":
				m.Mesh, m.Injection = "istio", "ambient ("+l["istio.io/dataplane-mode"]+")"
			case an["linkerd.io/inject"] != "":
				m.Mesh, m.Injection = "linkerd", "linkerd.io/inject="+an["linkerd.io/inject"]
			default:
				m.Injection = "-"
			}
			by[p.Namespace] = m
		}
		m.Pods++
		if mesh, ready := SidecarOf(p); mesh != "" {
			m.WithSidecar++
			if m.Mesh == "" {
				m.Mesh = mesh
			}
			if !ready {
				m.NotReady++
			}
		}
	}
	var out []MeshNamespace
	for _, m := range by {
		switch {
		case m.NotReady > 0:
			m.H = HealthErr
		case m.Mesh != "" && m.Injection != "-" && !strings.HasPrefix(m.Injection, "ambient") && m.WithSidecar < m.Pods && !strings.HasSuffix(m.Injection, "disabled"):
			m.H = HealthWarn // injection on, but some pods started before it
		case m.Mesh != "":
			m.H = HealthOK
		default:
			m.H = HealthNeutral
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out, nil
}

// ---------- OpenCost / Kubecost ----------

// CostLine is the cost of one namespace or workload over the window.
type CostLine struct {
	Name                              string
	CPU, RAM, PV, Network, Total, Eff float64
}

var costCandidates = []struct{ ns, svc, port, path string }{
	{"opencost", "opencost", "9003", "allocation/compute"},
	{"kubecost", "kubecost-cost-analyzer", "9090", "model/allocation"},
	{"opencost", "opencost", "9090", "allocation/compute"},
}

// Costs queries OpenCost or Kubecost (configured URL, service:<ns>/<svc>:<port>, or auto-detected).
func (c *Client) Costs(ctx context.Context, configured, window, aggregate string) ([]CostLine, string, error) {
	params := map[string]string{"window": window, "aggregate": aggregate, "accumulate": "true"}
	var raw []byte
	var source string
	get := func(ns, svc, port, path string) error {
		b, err := c.Clientset.CoreV1().Services(ns).ProxyGet("http", svc, port, path, params).DoRaw(ctx)
		raw, source = b, fmt.Sprintf("%s/%s:%s", ns, svc, port)
		return err
	}
	switch {
	case strings.HasPrefix(configured, "service:"):
		nsSvc, port, _ := strings.Cut(strings.TrimPrefix(configured, "service:"), ":")
		ns, svc, _ := strings.Cut(nsSvc, "/")
		path := "allocation/compute"
		if strings.Contains(svc, "kubecost") {
			path = "model/allocation"
		}
		if err := get(ns, svc, port, path); err != nil {
			return nil, source, err
		}
	case configured != "":
		u := strings.TrimRight(configured, "/") + "/allocation/compute?" + mapQuery(params)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, configured, err
		}
		defer resp.Body.Close()
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		source = configured
	default:
		found := false
		for _, cand := range costCandidates {
			if _, err := c.Clientset.CoreV1().Services(cand.ns).Get(ctx, cand.svc, metav1.GetOptions{}); err != nil {
				continue
			}
			if err := get(cand.ns, cand.svc, cand.port, cand.path); err == nil {
				found = true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("no OpenCost or Kubecost found (set opencost.url in the config)")
		}
	}
	var resp struct {
		Code int                         `json:"code"`
		Data []map[string]map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, source, fmt.Errorf("unexpected cost API answer: %w", err)
	}
	num := func(m map[string]any, k string) float64 { f, _ := m[k].(float64); return f }
	var out []CostLine
	for _, set := range resp.Data {
		for name, m := range set {
			if name == "__idle__" || name == "__unallocated__" {
				continue
			}
			out = append(out, CostLine{Name: name, CPU: num(m, "cpuCost"), RAM: num(m, "ramCost"), PV: num(m, "pvCost"),
				Network: num(m, "networkCost"), Total: num(m, "totalCost"), Eff: num(m, "totalEfficiency")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	return out, source, nil
}

func mapQuery(m map[string]string) string {
	v := url.Values{}
	for k, x := range m {
		v.Set(k, x)
	}
	return v.Encode()
}

var _ = apierrors.IsNotFound
