package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"0.1.0", "0.2.0", true}, {"0.2.0", "0.2.0", false}, {"1.0.0", "0.9.9", false}, {"v0.1.9", "0.1.10", true}, {"0.1.0-dev", "0.1.0", false}}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s,%s)=%v", c.a, c.b, got)
		}
	}
	if IsRelease("0.1.0-dev") || IsRelease("v0.1.0-3-gabc") || !IsRelease("0.2.0") {
		t.Error("IsRelease")
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "kubesage-update-check" {
			t.Errorf("unexpected user agent %q", r.Header.Get("User-Agent"))
		}
		w.Write([]byte(`{"tag_name":"v0.3.1","html_url":"https://example.com/r"}`))
	}))
	defer srv.Close()
	LatestURL = srv.URL
	r, err := Latest(context.Background())
	if err != nil || r.Version != "0.3.1" {
		t.Fatalf("%v %+v", err, r)
	}
}
