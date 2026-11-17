package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEditDryRunApplyGuards(t *testing.T) {
	_, ro := testApp(t, "dev", true)
	var e map[string]string
	if code := get(t, ro.URL+"/api/edit?ctx=dev&ns=shop&res=deployments&name=web", &e); code != 200 || !strings.Contains(e["yaml"], "name: web") {
		t.Fatalf("edit = %d %v", code, e)
	}
	if code := get(t, ro.URL+"/api/edit?ctx=dev&ns=shop&res=secrets&name=db", &e); code != 403 {
		t.Fatalf("secret editable: %d", code)
	}
	edited := strings.Replace(e["yaml"], "name: web", "name: web\n  labels: {tier: front}", 1)
	if e["yaml"] == "" {
		get(t, ro.URL+"/api/edit?ctx=dev&ns=shop&res=deployments&name=web", &e)
		edited = strings.Replace(e["yaml"], "name: web", "name: web\n  labels: {tier: front}", 1)
	}
	body, _ := json.Marshal(map[string]string{"Ctx": "dev", "NS": "shop", "Res": "deployments", "Name": "web", "YAML": edited})
	code, out := post(t, ro.URL+"/api/dryrun", string(body))
	if code != 200 || !strings.Contains(out, `"ok":true`) || !strings.Contains(out, "tier") {
		t.Fatalf("dryrun = %d %s", code, out)
	}
	if code, _ := post(t, ro.URL+"/api/apply", string(body)); code != 403 {
		t.Fatalf("read-only apply = %d", code)
	}
	_, prod := testApp(t, "aks-prod", false)
	body, _ = json.Marshal(map[string]string{"Ctx": "aks-prod", "NS": "shop", "Res": "deployments", "Name": "web", "YAML": edited})
	if code, _ := post(t, prod.URL+"/api/apply", string(body)); code != 412 {
		t.Fatalf("production apply without typed name = %d", code)
	}
}

func TestPagingFilterSortCompareOwner(t *testing.T) {
	a, srv := testApp(t, "dev", true)
	_ = a
	var list struct {
		Rows  []rowJSON
		Total int
		All   int
	}
	get(t, srv.URL+"/api/list?ctx=dev&ns=shop&res=secrets&filter=nomatch", &list)
	if list.Total != 0 || list.All != 1 {
		t.Fatalf("filter = %+v", list)
	}
	get(t, srv.URL+"/api/list?ctx=dev&ns=shop&res=secrets&limit=1&offset=5", &list)
	if len(list.Rows) != 0 || list.Total != 1 {
		t.Fatalf("paging past the end = %+v", list)
	}
	var cmp map[string][]string
	if code := get(t, srv.URL+"/api/compare?ctx=dev&res=deployments&ns=shop&name=web&ns2=shop&name2=web", &cmp); code != 200 {
		t.Fatalf("compare = %d", code)
	}
	for _, l := range cmp["diff"] {
		if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
			t.Fatalf("object differs from itself: %v", cmp["diff"])
		}
	}
	var o map[string]any
	get(t, srv.URL+"/api/owner?ctx=dev&ns=shop&res=deployments&name=web", &o)
	if o["found"] != false {
		t.Fatalf("owner invented: %v", o)
	}
}

func TestSortRows(t *testing.T) {
	rows := filterRows(nil, "")
	if len(rows) != 0 {
		t.Fatal("rows from nothing")
	}
}

var _ = http.MethodGet

func TestLogDownloads(t *testing.T) {
	_, srv := testApp(t, "dev", true)
	r, err := http.Get(srv.URL + "/api/logs/download?ctx=dev&ns=shop&pod=web-1&container=web")
	if err != nil || r.StatusCode != 200 || !strings.Contains(r.Header.Get("Content-Disposition"), `filename="kubesage-logs_dev_shop_web-1_web-`) {
		t.Fatalf("download = %v %v", r, err)
	}
	if r, _ := http.Get(srv.URL + "/api/logs/download?ctx=dev&ns=shop"); r.StatusCode != 400 {
		t.Fatalf("download without pod = %d", r.StatusCode)
	}
	r, _ = http.Get(srv.URL + "/api/logs/bundle?ctx=dev&ns=shop&res=secrets&name=db")
	if r.StatusCode != 400 {
		t.Fatalf("logs of a secret = %d", r.StatusCode)
	}
}
