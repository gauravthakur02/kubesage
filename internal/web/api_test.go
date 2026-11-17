package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gauravthakur02/kubesage/internal/audit"
	"github.com/gauravthakur02/kubesage/internal/kube"
)

func testApp(t *testing.T, ctxName string, readOnly bool) (*App, *httptest.Server) {
	t.Helper()
	two := int32(2)
	dep := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}, Spec: appsv1.DeploymentSpec{Replicas: &two}}
	sec := &corev1.Secret{TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"}, ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"}, Data: map[string][]byte{"password": []byte("hunter2")}}
	cs := fake.NewSimpleClientset(dep, sec)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{}, dep, sec)
	c := kube.NewForTesting(cs, ctxName, "shop", map[string]string{ctxName: ctxName})
	c.Dynamic = dyn
	a := &App{DefaultContext: ctxName, DefaultNS: "shop", ReadOnly: readOnly, Danger: regexp.MustCompile("prod"),
		AuditPath: filepath.Join(t.TempDir(), "audit.log"), clients: map[string]*kube.Client{ctxName: c}, crds: map[string][]kube.Resource{ctxName: nil}}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return a, srv
}

func get(t *testing.T, url string, v any) int {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	_ = json.NewDecoder(r.Body).Decode(v)
	return r.StatusCode
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(b)
}

func TestListAndYAML(t *testing.T) {
	_, srv := testApp(t, "dev", true)
	var list struct {
		Columns []string
		Rows    []rowJSON
	}
	if code := get(t, srv.URL+"/api/list?ctx=dev&ns=shop&res=deployments", &list); code != 200 || len(list.Rows) != 1 || list.Rows[0].Name != "web" {
		t.Fatalf("list = %d %+v", code, list)
	}
	var y map[string]string
	if code := get(t, srv.URL+"/api/yaml?ctx=dev&ns=shop&res=secrets&name=db", &y); code != 200 || strings.Contains(y["text"], "aHVudGVyMg") || strings.Contains(y["text"], "hunter2") {
		t.Fatalf("secret value exposed (%d): %s", code, y["text"])
	}
	var res []resourceInfo
	get(t, srv.URL+"/api/resources?ctx=dev", &res)
	found := false
	for _, r := range res {
		if r.Name == "deployments" && r.Scale && r.Graph {
			found = true
		}
	}
	if !found {
		t.Fatal("deployments missing from resources")
	}
	var e map[string]string
	if code := get(t, srv.URL+"/api/list?ctx=dev&ns=shop&res=nope", &e); code != 400 {
		t.Fatalf("unknown resource = %d", code)
	}
}

func TestActionsGuarded(t *testing.T) {
	_, ro := testApp(t, "dev", true)
	if code, _ := post(t, ro.URL+"/api/action", `{"Ctx":"dev","NS":"shop","Res":"deployments","Name":"web","Action":"scale","Replicas":0}`); code != 403 {
		t.Fatalf("read-only allowed a change: %d", code)
	}
	a, prod := testApp(t, "aks-prod", false)
	if code, _ := post(t, prod.URL+"/api/action", `{"Ctx":"aks-prod","NS":"shop","Res":"deployments","Name":"web","Action":"scale","Replicas":0}`); code != 412 {
		t.Fatalf("production change without typed name: %d", code)
	}
	code, body := post(t, prod.URL+"/api/action", `{"Ctx":"aks-prod","NS":"shop","Res":"deployments","Name":"web","Action":"scale","Replicas":3,"Confirm":"web"}`)
	if code != 200 {
		t.Fatalf("scale = %d %s", code, body)
	}
	d, _ := a.clients["aks-prod"].Dynamic.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace("shop").Get(t.Context(), "web", metav1.GetOptions{})
	if r, _, _ := unstructuredInt(d.Object, "spec", "replicas"); r != 3 {
		t.Fatalf("replicas = %d", r)
	}
	es, _ := audit.Read(a.AuditPath)
	if len(es) != 1 || es[0].By != "web" || !strings.Contains(es[0].Action, "scale to 3") {
		t.Fatalf("audit = %+v", es)
	}
	if code, _ := post(t, prod.URL+"/api/action", `{"Ctx":"aks-prod","NS":"shop","Res":"deployments","Name":"web","Action":"explode","Confirm":"web"}`); code != 400 {
		t.Fatalf("unknown action = %d", code)
	}
}

func unstructuredInt(o map[string]any, f ...string) (int64, bool, error) {
	var v any = o
	for _, k := range f {
		m, ok := v.(map[string]any)
		if !ok {
			return 0, false, nil
		}
		v = m[k]
	}
	switch n := v.(type) {
	case int64:
		return n, true, nil
	case float64:
		return int64(n), true, nil
	}
	return 0, false, nil
}

func TestIndexAndAssets(t *testing.T) {
	AssetVersion = "9.9.9"
	page := string(Index("tok123"))
	if !strings.Contains(page, `"tok123"`) || !strings.Contains(page, "/static/app.js?v=9.9.9") || !strings.Contains(page, "/static/app.css?v=9.9.9") {
		t.Fatalf("token or versioned assets missing:\n%s", page)
	}
	srv := httptest.NewServer(http.StripPrefix("", Assets()))
	defer srv.Close()
	for _, f := range []string{"/static/app.js", "/static/app.css"} {
		r, err := http.Get(srv.URL + f + "?v=9.9.9")
		if err != nil || r.StatusCode != 200 || r.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s: %v %v", f, r, err)
		}
	}
}
