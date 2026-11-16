package kube

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
)

// Health classifies a row so the UI can colour it.
type Health int

const (
	HealthOK Health = iota
	HealthWarn
	HealthErr
	HealthNeutral
)

// Row is one rendered object.
type Row struct {
	Namespace string
	Name      string
	Cells     []string
	Health    Health
	Usage     *Usage // live CPU/memory (pods, when metrics-server is available)
}

// Table is a list result ready to render.
type Table struct {
	Columns []string
	Rows    []Row
	Note    string // short live status shown in the title (UI-provided tables)
}

// Resource describes a browsable resource type.
type Resource struct {
	Name       string // canonical plural, e.g. "pods"
	Kind       string
	Short      string // label for tabs
	Aliases    []string
	Namespaced bool
	GVR        schema.GroupVersionResource
	Group      string // navigator section: Workloads, Network, Config & Storage, Access control (RBAC), Cluster, Custom resources
	keepOrder  bool   // rows are already in a meaningful order (UI-provided lists)
	// Slow tables query the cluster heavily (kubesage's own reports); the UI refreshes them less often.
	Slow bool
	list func(ctx context.Context, c *Client, ns string) (Table, error)
}

// Capabilities used by the UI to enable key bindings.
func (r Resource) CanLogs() bool { return r.Name == "pods" || r.HasSelector() }
func (r Resource) CanScale() bool {
	return r.Name == "deployments" || r.Name == "statefulsets" || r.Name == "replicasets"
}
func (r Resource) CanRestart() bool {
	return r.Name == "deployments" || r.Name == "statefulsets" || r.Name == "daemonsets"
}
func (r Resource) HasSelector() bool {
	switch r.Name {
	case "deployments", "statefulsets", "daemonsets", "replicasets", "jobs":
		return true
	}
	return false
}

// List fetches the table for this resource in the client's namespace.
func (r Resource) List(ctx context.Context, c *Client) (Table, error) {
	if r.keepOrder { // UI-provided tables: no namespace fan-out, keep rows and note as given
		return r.list(ctx, c, c.Namespace)
	}
	ns := c.Namespace
	if !r.Namespaced {
		ns = ""
	}
	var (
		mu   sync.Mutex
		cols []string
	)
	rows, err := listAcross(ctx, c, ns, func(n string) ([]Row, error) {
		t, err := r.list(ctx, c, n)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		cols = t.Columns
		mu.Unlock()
		return t.Rows, nil
	})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: cols, Rows: rows}
	if r.Name != "events" && !r.keepOrder {
		sortRows(t.Rows)
	}
	if r.Namespaced && ns == AllNamespaces {
		t.Columns = append([]string{"NAMESPACE"}, t.Columns...)
		for i := range t.Rows {
			t.Rows[i].Cells = append([]string{t.Rows[i].Namespace}, t.Rows[i].Cells...)
		}
	}
	return t, nil
}

var registry = []Resource{
	{Name: "pods", Kind: "Pod", Short: "Pods", Aliases: []string{"po", "pod"}, Namespaced: true,
		GVR: gvr("", "v1", "pods"), list: listPods},
	{Name: "deployments", Kind: "Deployment", Short: "Deploy", Aliases: []string{"deploy", "dp", "deployment"}, Namespaced: true,
		GVR: gvr("apps", "v1", "deployments"), list: listDeployments},
	{Name: "statefulsets", Kind: "StatefulSet", Short: "STS", Aliases: []string{"sts", "statefulset"}, Namespaced: true,
		GVR: gvr("apps", "v1", "statefulsets"), list: listStatefulSets},
	{Name: "daemonsets", Kind: "DaemonSet", Short: "DS", Aliases: []string{"ds", "daemonset"}, Namespaced: true,
		GVR: gvr("apps", "v1", "daemonsets"), list: listDaemonSets},
	{Name: "replicasets", Kind: "ReplicaSet", Short: "RS", Aliases: []string{"rs", "replicaset"}, Namespaced: true,
		GVR: gvr("apps", "v1", "replicasets"), list: listReplicaSets},
	{Name: "jobs", Kind: "Job", Short: "Jobs", Aliases: []string{"job"}, Namespaced: true,
		GVR: gvr("batch", "v1", "jobs"), list: listJobs},
	{Name: "cronjobs", Kind: "CronJob", Short: "CronJobs", Aliases: []string{"cj", "cronjob"}, Namespaced: true,
		GVR: gvr("batch", "v1", "cronjobs"), list: listCronJobs},
	{Name: "services", Kind: "Service", Short: "Svc", Aliases: []string{"svc", "service"}, Namespaced: true,
		GVR: gvr("", "v1", "services"), list: listServices},
	{Name: "ingresses", Kind: "Ingress", Short: "Ing", Aliases: []string{"ing", "ingress"}, Namespaced: true,
		GVR: gvr("networking.k8s.io", "v1", "ingresses"), list: listIngresses},
	{Name: "configmaps", Kind: "ConfigMap", Short: "CM", Aliases: []string{"cm", "configmap"}, Namespaced: true,
		GVR: gvr("", "v1", "configmaps"), list: listConfigMaps},
	{Name: "secrets", Kind: "Secret", Short: "Secrets", Aliases: []string{"secret", "sec"}, Namespaced: true,
		GVR: gvr("", "v1", "secrets"), list: listSecrets},
	{Name: "persistentvolumeclaims", Kind: "PersistentVolumeClaim", Short: "PVC", Aliases: []string{"pvc", "pvcs"}, Namespaced: true,
		GVR: gvr("", "v1", "persistentvolumeclaims"), list: listPVCs},
	{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Short: "HPA", Aliases: []string{"hpa", "hpas"}, Namespaced: true,
		GVR: gvr("autoscaling", "v2", "horizontalpodautoscalers"), list: listHPAs},
	{Name: "events", Kind: "Event", Short: "Events", Aliases: []string{"ev", "event"}, Namespaced: true,
		GVR: gvr("", "v1", "events"), list: listEvents},
	{Name: "nodes", Kind: "Node", Short: "Nodes", Aliases: []string{"no", "node"}, Namespaced: false,
		GVR: gvr("", "v1", "nodes"), list: listNodes},
	{Name: "namespaces", Kind: "Namespace", Short: "NS", Aliases: []string{"ns", "namespace"}, Namespaced: false,
		GVR: gvr("", "v1", "namespaces"), list: listNamespaces},
	{Name: "persistentvolumes", Kind: "PersistentVolume", Short: "PV", Aliases: []string{"pv", "pvs"}, Namespaced: false,
		GVR: gvr("", "v1", "persistentvolumes"), list: listPVs},
	{Name: "storageclasses", Kind: "StorageClass", Short: "SC", Aliases: []string{"sc", "storageclass"}, Namespaced: false,
		GVR: gvr("storage.k8s.io", "v1", "storageclasses"), list: listStorageClasses},
	{Name: "networkpolicies", Kind: "NetworkPolicy", Short: "NetPol", Aliases: []string{"netpol", "networkpolicy", "np"}, Namespaced: true,
		GVR: gvr("networking.k8s.io", "v1", "networkpolicies"), list: listNetworkPolicies},
	{Name: "endpoints", Kind: "Endpoints", Short: "EP", Aliases: []string{"ep"}, Namespaced: true,
		GVR: gvr("", "v1", "endpoints"), list: listEndpoints},
	{Name: "poddisruptionbudgets", Kind: "PodDisruptionBudget", Short: "PDB", Aliases: []string{"pdb", "pdbs"}, Namespaced: true,
		GVR: gvr("policy", "v1", "poddisruptionbudgets"), list: listPDBs},
	{Name: "serviceaccounts", Kind: "ServiceAccount", Short: "SA", Aliases: []string{"sa", "serviceaccount"}, Namespaced: true,
		GVR: gvr("", "v1", "serviceaccounts"), list: listServiceAccounts},
	{Name: "roles", Kind: "Role", Short: "Roles", Aliases: []string{"role"}, Namespaced: true,
		GVR: gvr("rbac.authorization.k8s.io", "v1", "roles"), list: listRoles},
	{Name: "rolebindings", Kind: "RoleBinding", Short: "RB", Aliases: []string{"rb", "rolebinding"}, Namespaced: true,
		GVR: gvr("rbac.authorization.k8s.io", "v1", "rolebindings"), list: listRoleBindings},
	{Name: "clusterroles", Kind: "ClusterRole", Short: "CRoles", Aliases: []string{"cr", "clusterrole"}, Namespaced: false,
		GVR: gvr("rbac.authorization.k8s.io", "v1", "clusterroles"), list: listClusterRoles},
	{Name: "clusterrolebindings", Kind: "ClusterRoleBinding", Short: "CRB", Aliases: []string{"crb", "clusterrolebinding"}, Namespaced: false,
		GVR: gvr("rbac.authorization.k8s.io", "v1", "clusterrolebindings"), list: listClusterRoleBindings},
	{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition", Short: "CRDs", Aliases: []string{"crd", "crds", "customresourcedefinition"}, Namespaced: false,
		GVR: gvr("apiextensions.k8s.io", "v1", "customresourcedefinitions"), list: listCRDs},
}

// groupOf assigns navigator sections to built-in resources.
var groupOf = map[string]string{
	"pods": "Workloads", "deployments": "Workloads", "statefulsets": "Workloads", "daemonsets": "Workloads",
	"replicasets": "Workloads", "jobs": "Workloads", "cronjobs": "Workloads", "horizontalpodautoscalers": "Workloads",
	"poddisruptionbudgets": "Workloads",
	"services":             "Network", "ingresses": "Network", "endpoints": "Network", "networkpolicies": "Network",
	"configmaps": "Config & Storage", "secrets": "Config & Storage", "persistentvolumeclaims": "Config & Storage",
	"persistentvolumes": "Config & Storage", "storageclasses": "Config & Storage",
	"serviceaccounts": "Access control (RBAC)", "roles": "Access control (RBAC)", "rolebindings": "Access control (RBAC)",
	"clusterroles": "Access control (RBAC)", "clusterrolebindings": "Access control (RBAC)",
	"events": "Cluster", "nodes": "Cluster", "namespaces": "Cluster", "customresourcedefinitions": "Custom resources",
}

// GroupOrder is the navigator section order.
var GroupOrder = []string{"Workloads", "Network", "Config & Storage", "Access control (RBAC)", "Cluster", "Custom resources", "Other APIs"}

func init() {
	for i := range registry {
		registry[i].Group = groupOf[registry[i].Name]
	}
}

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

// Resources returns every supported resource type.
func Resources() []Resource { return registry }

// Lookup finds a resource by name or alias.
func Lookup(name string) (Resource, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, r := range registry {
		if r.Name == name || strings.EqualFold(r.Kind, name) {
			return r, true
		}
		for _, a := range r.Aliases {
			if a == name {
				return r, true
			}
		}
	}
	return Resource{}, false
}

// Age renders a timestamp the way kubectl does.
func Age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(time.Since(t.Time))
}

func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
}

// ---------- workloads ----------

// PodStatus mirrors kubectl's STATUS column logic.
func PodStatus(p *corev1.Pod) string {
	reason := string(p.Status.Phase)
	if p.Status.Reason != "" {
		reason = p.Status.Reason
	}
	for i := len(p.Status.InitContainerStatuses) - 1; i >= 0; i-- {
		cs := p.Status.InitContainerStatuses[i]
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0 {
			continue
		}
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing" {
			return "Init:" + cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil {
			return "Init:" + cs.State.Terminated.Reason
		}
	}
	running := false
	for i := len(p.Status.ContainerStatuses) - 1; i >= 0; i-- {
		cs := p.Status.ContainerStatuses[i]
		switch {
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			reason = cs.State.Waiting.Reason
		case cs.State.Terminated != nil && cs.State.Terminated.Reason != "":
			reason = cs.State.Terminated.Reason
		case cs.State.Terminated != nil:
			reason = fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode)
		case cs.Ready && cs.State.Running != nil:
			running = true
		}
	}
	if reason == "Completed" && running {
		reason = "Running"
	}
	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}
	return reason
}

func podHealth(p *corev1.Pod, status string) Health {
	switch status {
	case "Running":
		for _, cs := range p.Status.ContainerStatuses {
			if !cs.Ready {
				return HealthWarn
			}
		}
		return HealthOK
	case "Succeeded", "Completed":
		return HealthNeutral
	case "Pending", "ContainerCreating", "PodInitializing", "Terminating":
		return HealthWarn
	}
	return HealthErr
}

func listPods(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	// Live usage from metrics-server (optional: columns are omitted when it is missing or forbidden).
	mctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	metrics, merr := c.PodMetrics(mctx, ns)
	cancel()
	t := Table{Columns: []string{"NAME", "READY", "STATUS", "RESTARTS", "IP", "NODE", "AGE"}}
	if merr == nil {
		t.Columns = []string{"NAME", "READY", "STATUS", "RESTARTS", "CPU", "MEM", "IP", "NODE", "AGE"}
	}
	for i := range list.Items {
		p := &list.Items[i]
		ready, restarts := 0, int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		status := PodStatus(p)
		health := podHealth(p, status)
		cells := []string{p.Name, fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers)), status, fmt.Sprint(restarts)}
		if merr == nil {
			cpu, mem := "-", "-"
			if m, ok := metrics[p.Namespace+"/"+p.Name]; ok {
				var u Usage
				for _, cu := range m {
					u.CPUMilli += cu.CPUMilli
					u.MemBytes += cu.MemBytes
				}
				_, cpuLim, _, memLim := podLimits(p)
				cpuPct, memPct := Percent(u.CPUMilli, cpuLim), Percent(u.MemBytes, memLim)
				cpu = usageCell(fmt.Sprintf("%dm", u.CPUMilli), cpuPct)
				mem = usageCell(FormatBytes(u.MemBytes), memPct)
				if (memPct >= 90 || cpuPct >= 90) && health == HealthOK {
					health = HealthWarn // close to a limit
				}
			}
			cells = append(cells, cpu, mem)
		}
		cells = append(cells, p.Status.PodIP, p.Spec.NodeName, Age(p.CreationTimestamp))
		row := Row{Namespace: p.Namespace, Name: p.Name, Health: health, Cells: cells}
		if m, ok := metrics[p.Namespace+"/"+p.Name]; ok && merr == nil {
			var u Usage
			for _, cu := range m {
				u.CPUMilli += cu.CPUMilli
				u.MemBytes += cu.MemBytes
			}
			row.Usage = &u
		}
		t.Rows = append(t.Rows, row)
	}
	sortRows(t.Rows)
	return t, nil
}

func replicaHealth(ready, desired int32) Health {
	switch {
	case desired == 0:
		return HealthNeutral
	case ready >= desired:
		return HealthOK
	case ready == 0:
		return HealthErr
	}
	return HealthWarn
}

func images(spec corev1.PodSpec) string {
	var out []string
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return strings.Join(out, ",")
}

func deref(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func listDeployments(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "READY", "UP-TO-DATE", "AVAILABLE", "IMAGES", "AGE"}}
	for _, d := range list.Items {
		want := deref(d.Spec.Replicas, 1)
		t.Rows = append(t.Rows, Row{Namespace: d.Namespace, Name: d.Name, Health: replicaHealth(d.Status.AvailableReplicas, want), Cells: []string{
			d.Name, fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, want), fmt.Sprint(d.Status.UpdatedReplicas),
			fmt.Sprint(d.Status.AvailableReplicas), images(d.Spec.Template.Spec), Age(d.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listStatefulSets(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "READY", "SERVICE", "IMAGES", "AGE"}}
	for _, s := range list.Items {
		want := deref(s.Spec.Replicas, 1)
		t.Rows = append(t.Rows, Row{Namespace: s.Namespace, Name: s.Name, Health: replicaHealth(s.Status.ReadyReplicas, want), Cells: []string{
			s.Name, fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, want), s.Spec.ServiceName, images(s.Spec.Template.Spec), Age(s.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listDaemonSets(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "DESIRED", "CURRENT", "READY", "AVAILABLE", "AGE"}}
	for _, d := range list.Items {
		s := d.Status
		t.Rows = append(t.Rows, Row{Namespace: d.Namespace, Name: d.Name, Health: replicaHealth(s.NumberAvailable, s.DesiredNumberScheduled), Cells: []string{
			d.Name, fmt.Sprint(s.DesiredNumberScheduled), fmt.Sprint(s.CurrentNumberScheduled), fmt.Sprint(s.NumberReady),
			fmt.Sprint(s.NumberAvailable), Age(d.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listReplicaSets(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "DESIRED", "CURRENT", "READY", "AGE"}}
	for _, r := range list.Items {
		want := deref(r.Spec.Replicas, 1)
		t.Rows = append(t.Rows, Row{Namespace: r.Namespace, Name: r.Name, Health: replicaHealth(r.Status.ReadyReplicas, want), Cells: []string{
			r.Name, fmt.Sprint(want), fmt.Sprint(r.Status.Replicas), fmt.Sprint(r.Status.ReadyReplicas), Age(r.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func jobFailed(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func listJobs(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "COMPLETIONS", "FAILED", "DURATION", "AGE"}}
	for i := range list.Items {
		j := &list.Items[i]
		h := HealthWarn
		dur := "-"
		if j.Status.StartTime != nil {
			end := time.Now()
			if j.Status.CompletionTime != nil {
				end = j.Status.CompletionTime.Time
				h = HealthNeutral
			}
			dur = duration.HumanDuration(end.Sub(j.Status.StartTime.Time))
		}
		if jobFailed(j) {
			h = HealthErr
		}
		t.Rows = append(t.Rows, Row{Namespace: j.Namespace, Name: j.Name, Health: h, Cells: []string{
			j.Name, fmt.Sprintf("%d/%d", j.Status.Succeeded, deref(j.Spec.Completions, 1)), fmt.Sprint(j.Status.Failed), dur, Age(j.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listCronJobs(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.BatchV1().CronJobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "SCHEDULE", "SUSPEND", "ACTIVE", "LAST SCHEDULE", "AGE"}}
	for _, cj := range list.Items {
		suspend := cj.Spec.Suspend != nil && *cj.Spec.Suspend
		last := "<none>"
		if cj.Status.LastScheduleTime != nil {
			last = Age(*cj.Status.LastScheduleTime)
		}
		h := HealthOK
		if suspend {
			h = HealthNeutral
		}
		t.Rows = append(t.Rows, Row{Namespace: cj.Namespace, Name: cj.Name, Health: h, Cells: []string{
			cj.Name, cj.Spec.Schedule, fmt.Sprint(suspend), fmt.Sprint(len(cj.Status.Active)), last, Age(cj.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

// ---------- network / config ----------

func listServices(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "TYPE", "CLUSTER-IP", "EXTERNAL-IP", "PORTS", "AGE"}}
	for _, s := range list.Items {
		ext := "<none>"
		var ips []string
		for _, in := range s.Status.LoadBalancer.Ingress {
			if in.IP != "" {
				ips = append(ips, in.IP)
			} else if in.Hostname != "" {
				ips = append(ips, in.Hostname)
			}
		}
		ips = append(ips, s.Spec.ExternalIPs...)
		h := HealthOK
		if len(ips) > 0 {
			ext = strings.Join(ips, ",")
		} else if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
			ext, h = "<pending>", HealthWarn
		}
		var ports []string
		for _, p := range s.Spec.Ports {
			ps := fmt.Sprintf("%d/%s", p.Port, p.Protocol)
			if p.NodePort != 0 {
				ps = fmt.Sprintf("%d:%d/%s", p.Port, p.NodePort, p.Protocol)
			}
			ports = append(ports, ps)
		}
		t.Rows = append(t.Rows, Row{Namespace: s.Namespace, Name: s.Name, Health: h, Cells: []string{
			s.Name, string(s.Spec.Type), s.Spec.ClusterIP, ext, strings.Join(ports, ","), Age(s.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listIngresses(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.NetworkingV1().Ingresses(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "CLASS", "HOSTS", "ADDRESS", "AGE"}}
	for _, in := range list.Items {
		class := "<none>"
		if in.Spec.IngressClassName != nil {
			class = *in.Spec.IngressClassName
		}
		var hosts, addrs []string
		for _, r := range in.Spec.Rules {
			if r.Host != "" {
				hosts = append(hosts, r.Host)
			}
		}
		for _, lb := range in.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				addrs = append(addrs, lb.IP)
			} else {
				addrs = append(addrs, lb.Hostname)
			}
		}
		h := HealthOK
		if len(addrs) == 0 {
			h = HealthWarn
		}
		t.Rows = append(t.Rows, Row{Namespace: in.Namespace, Name: in.Name, Health: h, Cells: []string{
			in.Name, class, strings.Join(hosts, ","), strings.Join(addrs, ","), Age(in.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listConfigMaps(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "DATA", "AGE"}}
	for _, cm := range list.Items {
		t.Rows = append(t.Rows, Row{Namespace: cm.Namespace, Name: cm.Name, Health: HealthNeutral, Cells: []string{
			cm.Name, fmt.Sprint(len(cm.Data) + len(cm.BinaryData)), Age(cm.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listSecrets(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "TYPE", "DATA", "AGE"}}
	for _, s := range list.Items {
		t.Rows = append(t.Rows, Row{Namespace: s.Namespace, Name: s.Name, Health: HealthNeutral, Cells: []string{
			s.Name, string(s.Type), fmt.Sprint(len(s.Data)), Age(s.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listPVCs(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "STATUS", "VOLUME", "CAPACITY", "STORAGECLASS", "AGE"}}
	for _, p := range list.Items {
		capacity := ""
		if q, ok := p.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		sc := ""
		if p.Spec.StorageClassName != nil {
			sc = *p.Spec.StorageClassName
		}
		h := HealthOK
		if p.Status.Phase != corev1.ClaimBound {
			h = HealthWarn
		}
		t.Rows = append(t.Rows, Row{Namespace: p.Namespace, Name: p.Name, Health: h, Cells: []string{
			p.Name, string(p.Status.Phase), p.Spec.VolumeName, capacity, sc, Age(p.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listHPAs(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "REFERENCE", "MIN", "MAX", "REPLICAS", "AGE"}}
	for _, h := range list.Items {
		health := HealthOK
		if h.Status.CurrentReplicas >= h.Spec.MaxReplicas {
			health = HealthWarn
		}
		t.Rows = append(t.Rows, Row{Namespace: h.Namespace, Name: h.Name, Health: health, Cells: []string{
			h.Name, h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name, fmt.Sprint(deref(h.Spec.MinReplicas, 1)),
			fmt.Sprint(h.Spec.MaxReplicas), fmt.Sprint(h.Status.CurrentReplicas), Age(h.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

// ---------- cluster ----------

func eventTime(e *corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func listEvents(ctx context.Context, c *Client, ns string) (Table, error) {
	list, err := c.Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool { return eventTime(&items[i]).After(eventTime(&items[j])) })
	t := Table{Columns: []string{"LAST SEEN", "TYPE", "REASON", "OBJECT", "COUNT", "MESSAGE"}}
	for i := range items {
		e := &items[i]
		h := HealthNeutral
		if e.Type == corev1.EventTypeWarning {
			h = HealthWarn
		}
		obj := strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name
		t.Rows = append(t.Rows, Row{Namespace: e.Namespace, Name: e.Name, Health: h, Cells: []string{
			duration.HumanDuration(time.Since(eventTime(e))), e.Type, e.Reason, obj, fmt.Sprint(max(e.Count, 1)),
			strings.ReplaceAll(e.Message, "\n", " "),
		}})
	}
	return t, nil
}

// NodeReady reports whether the node's Ready condition is true.
func NodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeRoles(n *corev1.Node) string {
	var roles []string
	for k := range n.Labels {
		if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && r != "" {
			roles = append(roles, r)
		}
	}
	if len(roles) == 0 {
		if pool := n.Labels["kubernetes.azure.com/agentpool"]; pool != "" {
			return "pool:" + pool
		}
		return "<none>"
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func listNodes(ctx context.Context, c *Client, _ string) (Table, error) {
	list, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "STATUS", "ROLES", "VERSION", "CPU", "MEMORY", "AGE"}}
	for i := range list.Items {
		n := &list.Items[i]
		status, h := "Ready", HealthOK
		if !NodeReady(n) {
			status, h = "NotReady", HealthErr
		}
		if n.Spec.Unschedulable {
			status += ",SchedulingDisabled"
			if h == HealthOK {
				h = HealthWarn
			}
		}
		cpu := n.Status.Allocatable[corev1.ResourceCPU]
		mem := n.Status.Allocatable[corev1.ResourceMemory]
		t.Rows = append(t.Rows, Row{Name: n.Name, Health: h, Cells: []string{
			n.Name, status, nodeRoles(n), n.Status.NodeInfo.KubeletVersion, cpu.String(),
			fmt.Sprintf("%.1fGi", float64(mem.Value())/(1<<30)), Age(n.CreationTimestamp),
		}})
	}
	sortRows(t.Rows)
	return t, nil
}

func listNamespaces(ctx context.Context, c *Client, _ string) (Table, error) {
	list, err := c.Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Table{}, err
	}
	t := Table{Columns: []string{"NAME", "STATUS", "AGE"}}
	for _, n := range list.Items {
		h := HealthOK
		if n.Status.Phase != corev1.NamespaceActive {
			h = HealthWarn
		}
		t.Rows = append(t.Rows, Row{Name: n.Name, Health: h, Cells: []string{n.Name, string(n.Status.Phase), Age(n.CreationTimestamp)}})
	}
	sortRows(t.Rows)
	return t, nil
}

// LocalResource is a UI-provided table built from memory only; it refreshes at the normal rate.
func LocalResource(name, kind, short string, rows func() Table) Resource {
	r := VirtualResource(name, kind, short, rows)
	r.Slow = false
	return r
}

// VirtualResource is a UI-provided table (e.g. the Clusters overview) that is not a Kubernetes API resource.
func VirtualResource(name, kind, short string, rows func() Table) Resource {
	return Resource{Name: name, Kind: kind, Short: short, keepOrder: true, Slow: true, list: func(context.Context, *Client, string) (Table, error) {
		return rows(), nil
	}}
}
