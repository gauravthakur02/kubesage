package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func helmReleaseSecret(t *testing.T, ns, name, manifest string) *corev1.Secret {
	t.Helper()
	js, _ := json.Marshal(map[string]any{"name": name, "namespace": ns, "version": 1, "info": map[string]any{"status": "deployed"},
		"chart": map[string]any{"metadata": map[string]any{"name": name, "version": "1.0.0"}}, "manifest": manifest})
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(js)
	w.Close()
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1." + name + ".v1", Namespace: ns,
		Labels: map[string]string{"owner": "helm", "name": name, "version": "1", "status": "deployed"}},
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))}}
}

func TestUpgradeReadiness(t *testing.T) {
	manifest := "---\napiVersion: batch/v1beta1\nkind: CronJob\nmetadata:\n  name: nightly\n---\napiVersion: flowcontrol.apiserver.k8s.io/v1beta3\nkind: FlowSchema\nmetadata:\n  name: fs\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: ok\n"
	cs := fake.NewSimpleClientset(helmReleaseSecret(t, "shop", "jobs", manifest),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"kubernetes.azure.com/agentpool": "system"}},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.30.4"}}})
	c := NewForTesting(cs, "dev", "shop", map[string]string{"dev": "dev"})
	r, err := c.UpgradeReadiness(context.Background(), "1.30")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range r.Findings {
		got = append(got, f.API.Kind+":"+map[bool]string{true: "blocking", false: "warn"}[f.Blocking])
	}
	s := strings.Join(got, ",")
	if !strings.Contains(s, "CronJob:blocking") || !strings.Contains(s, "FlowSchema:warn") || strings.Contains(s, "Deployment") {
		t.Fatalf("findings: %s", s)
	}
	if len(r.Nodes) != 1 || r.Nodes[0].Pool != "system" {
		t.Fatalf("nodes: %+v", r.Nodes)
	}
	if Minor("v1.35.7") != 35 || Minor("1.36") != 36 {
		t.Error("Minor")
	}
}

func TestNetCheckScript(t *testing.T) {
	s, err := NetCheckScript("http://web.shop:8080/health")
	if err != nil || !strings.Contains(s, "nslookup web.shop") || !strings.Contains(s, "nc -z -w 3 web.shop 8080") || !strings.Contains(s, "wget") {
		t.Fatalf("%v\n%s", err, s)
	}
	if s, _ := NetCheckScript("db:5432"); strings.Contains(s, "wget") || !strings.Contains(s, "db 5432") {
		t.Fatalf("tcp-only target:\n%s", s)
	}
	for _, bad := range []string{"x; rm -rf /", "$(id)", "a`b`", ""} {
		if _, err := NetCheckScript(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestWorkloadOfPodAndForbidden(t *testing.T) {
	got := strings.Join(WorkloadOfPod("web-7d9f8c6b5-x2k4p"), ",")
	if !strings.Contains(got, "web") || !strings.Contains(got, "web-7d9f8c6b5") {
		t.Errorf("deployment pod: %s", got)
	}
	if got := WorkloadOfPod("db-0"); len(got) == 0 || got[len(got)-1] != "db" {
		t.Errorf("statefulset pod: %v", got)
	}
	v, r, ns, ok := ParseForbidden(`secrets is forbidden: User "u" cannot list resource "secrets" in API group "" in the namespace "shop"`)
	if !ok || v != "list" || r != "secrets" || ns != "shop" {
		t.Errorf("%s %s %s %v", v, r, ns, ok)
	}
}

func TestPromWorkloadPeaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		val := "0.25"
		if strings.Contains(q, "memory") {
			val = "536870912"
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"shop","pod":"web-7d9f8c6b5-x2k4p"},"value":[1,"` + val + `"]}]}}`))
	}))
	defer srv.Close()
	c := NewForTesting(fake.NewSimpleClientset(), "dev", "shop", map[string]string{"dev": "dev"})
	p, err := c.NewProm(context.Background(), srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	peaks, err := p.WorkloadPeaks(context.Background(), "shop", "7d")
	if err != nil {
		t.Fatal(err)
	}
	if pk := peaks["shop/web"]; pk[0] != 250 || pk[1] != 512<<20 {
		t.Fatalf("peaks: %+v", peaks)
	}
	if _, err := c.NewProm(context.Background(), "", ""); err == nil {
		t.Error("no Prometheus should be found in an empty cluster")
	}
}
