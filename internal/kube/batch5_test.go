package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestMergePatchLocally(t *testing.T) {
	orig := `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  template:
    spec:
      containers:
      - name: web
        image: web:1
        env: [{name: TOKEN, value: secret}]
      - name: proxy
        image: envoy:1
`
	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	out, err := MergePatchLocally(gvk, orig, "spec:\n  template:\n    spec:\n      containers:\n      - name: web\n        resources: {limits: {memory: 256Mi}}\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memory: 256Mi", "image: web:1", "value: secret", "name: proxy"} {
		if !strings.Contains(out, want) {
			t.Fatalf("merged object lost %q:\n%s", want, out)
		}
	}
	if _, err := MergePatchLocally(gvk, orig, "spec:\n  template:\n    spec:\n      containers:\n      - name: web\n        env: [{name: TOKEN, value: <redacted>}]\n"); err == nil {
		t.Fatal("patch writing a redacted value was accepted")
	}
	if _, err := MergePatchLocally(gvk, orig, "metadata: {name: other}\n"); err == nil {
		t.Fatal("rename accepted")
	}
	crd := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	out, err = MergePatchLocally(crd, "kind: Widget\nmetadata: {name: w}\nspec: {size: 1, color: red}\n", "spec: {size: 3}\n")
	if err != nil || !strings.Contains(out, "size: 3") || !strings.Contains(out, "color: red") {
		t.Fatalf("json merge: %v\n%s", err, out)
	}
}

func TestApprovalsTwoPersonRule(t *testing.T) {
	cs := fake.NewSimpleClientset()
	user := "alice"
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.SelfSubjectReview{Status: authnv1.SelfSubjectReviewStatus{UserInfo: authnv1.UserInfo{Username: user}}}, nil
	})
	c := NewForTesting(cs, "prod", "shop", map[string]string{"prod": "prod"})
	ctx := context.Background()
	ap, err := c.RequestApproval(ctx, "kubesage-approvals", Approval{Context: "prod", Action: "scale web to 0"})
	if err != nil || ap.Requester != "alice" || ap.Status != "pending" {
		t.Fatalf("request = %+v, %v", ap, err)
	}
	if _, err := c.DecideApproval(ctx, "kubesage-approvals", ap.ID, true, time.Hour); err == nil || !strings.Contains(err.Error(), "someone else") {
		t.Fatalf("self-approval allowed: %v", err)
	}
	user = "bob"
	got, err := c.DecideApproval(ctx, "kubesage-approvals", ap.ID, true, time.Hour)
	if err != nil || got.Status != "approved" || got.Approver != "bob" {
		t.Fatalf("approve = %+v, %v", got, err)
	}
	if _, err := c.DecideApproval(ctx, "kubesage-approvals", ap.ID, false, time.Hour); err == nil {
		t.Fatal("decided twice")
	}
	list, _ := c.Approvals(ctx, "kubesage-approvals", time.Hour)
	if len(list) != 1 || list[0].Status != "approved" {
		t.Fatalf("list = %+v", list)
	}
	if err := c.CloseApproval(ctx, "kubesage-approvals", ap.ID, "run"); err != nil {
		t.Fatal(err)
	}
}

func TestCloneNamespace(t *testing.T) {
	yes := true
	objs := []runtime.Object{
		&corev1.Service{TypeMeta: metav1.TypeMeta{Kind: "Service", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop", ResourceVersion: "9"},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, ClusterIP: "10.0.0.5", ClusterIPs: []string{"10.0.0.5"}, Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}}}},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "shop"}},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "shop"}},
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{Kind: "ServiceAccount", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop"}},
		&appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}},
		&appsv1.ReplicaSet{TypeMeta: metav1.TypeMeta{Kind: "ReplicaSet", APIVersion: "apps/v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: &yes}}}},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "batch", Version: "v1", Resource: "cronjobs"}: "CronJobList"}, objs...)
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}})
	c := NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	ctx := context.Background()
	res, err := c.CloneNamespace(ctx, "shop", "shop-pr-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Copied["services"] != 1 || res.Copied["configmaps"] != 1 || res.Copied["deployments"] != 1 || res.Copied["serviceaccounts"] != 0 {
		t.Fatalf("copied = %+v skipped = %v", res.Copied, res.Skipped)
	}
	svc, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "services"}).Namespace("shop-pr-1").Get(ctx, "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	typ, _, _ := unstructured.NestedString(svc.Object, "spec", "type")
	ip, _, _ := unstructured.NestedString(svc.Object, "spec", "clusterIP")
	if typ != "ClusterIP" || ip != "" || strings.Contains(fmt.Sprint(svc.Object), "30080") {
		t.Fatalf("service not cleaned: %v", svc.Object["spec"])
	}
	if _, err := c.CloneNamespace(ctx, "shop", "shop-pr-1", time.Hour); err == nil {
		t.Fatal("cloned into an existing namespace")
	}
	cl, _ := c.Clones(ctx)
	if len(cl) != 1 || cl[0].Of != "shop" {
		t.Fatalf("clones = %+v", cl)
	}
	if err := c.DeleteClone(ctx, "shop"); err == nil {
		t.Fatal("deleted a namespace that is not a clone")
	}
	if err := c.DeleteClone(ctx, "shop-pr-1"); err != nil {
		t.Fatal(err)
	}
}

func TestNodePoolsAndScaleCommand(t *testing.T) {
	node := func(name string, l map[string]string, ready bool) *corev1.Node {
		st := corev1.ConditionFalse
		if ready {
			st = corev1.ConditionTrue
		}
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}}}}
	}
	cs := fake.NewSimpleClientset(
		node("a1", map[string]string{"eks.amazonaws.com/nodegroup": "apps", "alpha.eksctl.io/cluster-name": "shop", "eks.amazonaws.com/capacityType": "SPOT",
			"node.kubernetes.io/instance-type": "m6i.large", "topology.kubernetes.io/zone": "eu-1a"}, true),
		node("a2", map[string]string{"eks.amazonaws.com/nodegroup": "apps", "alpha.eksctl.io/cluster-name": "shop", "node.kubernetes.io/instance-type": "m6i.large",
			"topology.kubernetes.io/zone": "eu-1b"}, false),
		node("k1", map[string]string{"karpenter.sh/nodepool": "default"}, true))
	c := NewForTesting(cs, "dev", "", map[string]string{"dev": "dev"})
	pools, err := c.NodePools(context.Background())
	if err != nil || len(pools) != 2 {
		t.Fatalf("pools = %+v, %v", pools, err)
	}
	p := pools[0]
	if p.Name != "apps" || p.Cloud != "EKS" || p.Nodes != 2 || p.Ready != 1 || !p.Spot || len(p.Zones) != 2 {
		t.Fatalf("pool = %+v", p)
	}
	argv, err := c.ScalePoolCommand(context.Background(), p, 4)
	if err != nil || strings.Join(argv, " ") != "aws eks update-nodegroup-config --cluster-name shop --nodegroup-name apps --scaling-config desiredSize=4" {
		t.Fatalf("argv = %v, %v", argv, err)
	}
	if _, err := c.ScalePoolCommand(context.Background(), pools[1], 3); err == nil {
		t.Fatal("karpenter pool scaled directly")
	}
}

func TestArgoRolloutsKarpenterAndHubble(t *testing.T) {
	ro := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout",
		"metadata": map[string]any{"name": "web", "namespace": "shop"},
		"spec": map[string]any{"replicas": int64(4), "strategy": map[string]any{"canary": map[string]any{"steps": []any{map[string]any{"setWeight": int64(20)}, map[string]any{"pause": map[string]any{}}}}},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "web", "image": "reg.example/web:2"}}}}},
		"status": map[string]any{"phase": "Paused", "currentStepIndex": int64(1), "readyReplicas": int64(4), "pauseConditions": []any{map[string]any{"reason": "CanaryPauseStep"}}}}}
	pool := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "karpenter.sh/v1", "kind": "NodePool", "metadata": map[string]any{"name": "default"},
		"spec": map[string]any{"limits": map[string]any{"cpu": "100"}, "disruption": map[string]any{"consolidationPolicy": "WhenEmptyOrUnderutilized"}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}
	claim := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "karpenter.sh/v1", "kind": "NodeClaim",
		"metadata": map[string]any{"name": "default-x", "labels": map[string]any{"karpenter.sh/nodepool": "default", "node.kubernetes.io/instance-type": "c7g.large"}},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		RolloutGVR: "RolloutList", KarpenterPoolGVR: "NodePoolList", KarpenterClaimGVR: "NodeClaimList"}, ro, pool, claim)
	c := NewForTesting(fake.NewSimpleClientset(), "dev", "shop", map[string]string{"dev": "dev"})
	c.Dynamic = dyn
	ctx := context.Background()
	rs, found, err := c.ArgoRollouts(ctx)
	if err != nil || !found || len(rs) != 1 || rs[0].Strategy != "canary" || !rs[0].Paused || rs[0].Steps != 2 || rs[0].Step != 1 {
		t.Fatalf("rollouts = %+v %v %v", rs, found, err)
	}
	if err := c.RolloutAction(ctx, "shop", "web", "promote"); err != nil {
		t.Fatal(err)
	}
	got, _ := dyn.Resource(RolloutGVR).Namespace("shop").Get(ctx, "web", metav1.GetOptions{})
	if pc, _, _ := unstructured.NestedSlice(got.Object, "status", "pauseConditions"); len(pc) != 0 {
		t.Fatalf("promote did not clear pause conditions: %v", got.Object["status"])
	}
	if err := c.RolloutAction(ctx, "shop", "web", "explode"); err == nil {
		t.Fatal("unknown action accepted")
	}
	ps, found, err := c.KarpenterPools(ctx)
	if err != nil || !found || len(ps) != 1 || ps[0].Claims != 1 || ps[0].ClaimsReady != 1 || ps[0].Limits != "cpu=100" {
		t.Fatalf("karpenter = %+v %v %v", ps, found, err)
	}
	flows := ParseHubble([]byte(`{"flow":{"time":"2026-11-17T10:00:00Z","verdict":"DROPPED","drop_reason_desc":"POLICY_DENIED","source":{"namespace":"shop","pod_name":"web-1"},"destination":{"namespace":"db","pod_name":"pg-0"},"IP":{"source":"10.0.0.1","destination":"10.0.0.2"},"Summary":"TCP Flags: SYN"}}
not json
{"flow":{"verdict":"FORWARDED","IP":{"source":"10.0.0.1","destination":"1.1.1.1"}}}`))
	if len(flows) != 2 || flows[0].Source != "shop/web-1" || flows[0].Dest != "db/pg-0" || !strings.HasPrefix(flows[0].Summary, "POLICY_DENIED") || flows[1].Dest != "1.1.1.1" {
		t.Fatalf("flows = %+v", flows)
	}
}

func TestLokiQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" || !strings.Contains(r.URL.Query().Get("query"), `pod=~"web-.*"`) {
			http.Error(w, "bad", 400)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"result":[{"stream":{"pod":"web-1"},"values":[["1700000000000000002","second"],["1700000000000000001","first"]]}]}}`)
	}))
	defer srv.Close()
	c := NewForTesting(fake.NewSimpleClientset(), "dev", "shop", map[string]string{"dev": "dev"})
	lines, src, err := c.LokiQuery(context.Background(), srv.URL, LokiSelector("shop", "web-.*"), time.Hour, 100)
	if err != nil || src != srv.URL || len(lines) != 2 || lines[0].Line != "first" || lines[1].Labels["pod"] != "web-1" {
		t.Fatalf("loki = %+v %s %v", lines, src, err)
	}
}

func TestResizeValidation(t *testing.T) {
	c := NewForTesting(fake.NewSimpleClientset(), "dev", "shop", map[string]string{"dev": "dev"})
	if err := c.ResizePod(context.Background(), "shop", "p", "web", "lots", "", "", "", true); err == nil {
		t.Fatal("bad quantity accepted")
	}
	if err := c.ResizePod(context.Background(), "shop", "p", "web", "", "", "", "", true); err == nil {
		t.Fatal("empty resize accepted")
	}
}
