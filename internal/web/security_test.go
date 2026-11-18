package web

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestForwardIsolation(t *testing.T) {
	var seen http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte("app " + r.URL.RequestURI()))
	}))
	defer backend.Close()
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	f := &webForward{key: "k3y", port: 4242, proxy: newForwardProxy(port)}
	srv := httptest.NewServer(f)
	defer srv.Close()
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r, _ := cl.Get(srv.URL + "/x"); r.StatusCode != 403 {
		t.Fatalf("no key = %d", r.StatusCode)
	}
	if r, _ := cl.Get(srv.URL + "/x?kubeweave-fwd=wrong"); r.StatusCode != 403 {
		t.Fatalf("wrong key = %d", r.StatusCode)
	}
	r, _ := cl.Get(srv.URL + "/x?a=1&kubeweave-fwd=k3y")
	if r.StatusCode != 302 || r.Header.Get("Location") != "/x?a=1" {
		t.Fatalf("key = %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	ck := r.Cookies()[0]
	req, _ := http.NewRequest("GET", srv.URL+"/x?a=1", nil)
	req.AddCookie(ck)
	req.AddCookie(&http.Cookie{Name: "kubeweave_session", Value: "secret-session"})
	req.AddCookie(&http.Cookie{Name: "kubeweave_token", Value: "secret-token"})
	req.AddCookie(&http.Cookie{Name: "app", Value: "keep"})
	req.Header.Set("X-Kubeweave-Token", "secret-token")
	r, err := cl.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("proxied = %v %v", r, err)
	}
	b, _ := io.ReadAll(r.Body)
	if string(b) != "app /x?a=1" {
		t.Fatalf("body = %q", b)
	}
	all := strings.Join(seen.Values("Cookie"), ";") + seen.Get("X-Kubeweave-Token")
	if strings.Contains(all, "secret") || strings.Contains(all, "kubeweave_fwd") || !strings.Contains(all, "app=keep") {
		t.Fatalf("kubeweave credentials reached the forwarded app: %v", seen)
	}
}

func TestContextAllowList(t *testing.T) {
	a, srv := testApp(t, "dev", true)
	a.Contexts = regexp.MustCompile("^dev$")
	var e map[string]string
	if code := get(t, srv.URL+"/api/list?ctx=prod&ns=shop&res=pods", &e); code != 400 || !strings.Contains(e["error"], "not offered") {
		t.Fatalf("other context = %d %v", code, e)
	}
}

func TestLoginBackAndLogout(t *testing.T) {
	idp := newIdP(t)
	o, err := NewOIDC(t.Context(), OIDCConfig{Issuer: idp.srv.URL, ClientID: "kubeweave", RedirectURL: "http://127.0.0.1/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	for _, back := range []string{`/\evil.example`, "//evil.example", "https://evil.example"} {
		w := httptest.NewRecorder()
		o.Login(w, httptest.NewRequest("GET", "/auth/login?back="+back, nil))
		var f flow
		for _, c := range w.Result().Cookies() {
			if c.Name == flowCookie {
				o.verify(c.Value, &f)
			}
		}
		if f.Back != "" {
			t.Errorf("back %q kept as %q", back, f.Back)
		}
	}
	w := httptest.NewRecorder()
	o.Logout(w, httptest.NewRequest("GET", "/auth/logout", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET logout = %d", w.Code)
	}
}
